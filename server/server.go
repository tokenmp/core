// Package server 把 relay、channel 与 plugin 装配成一个可监听的 HTTP 服务，
// 对外暴露四种方言的端点。
//
// 装配规则只有一条：client 方言与渠道方言相同时按字节透传，不同时经 relay 转换。
// 插件在四个时机介入，插件失败不影响转发可用性。
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tokenmp/core/channel"
	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/internal/affinity"
	"github.com/tokenmp/core/plugin"
	"github.com/tokenmp/core/relay"
)

// 服务端固定超时。
const (
	// readHeaderTimeout 约束请求头读取，防慢速连接占用连接。
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout 是收到取消信号后等待在途请求结束的时间。
	shutdownTimeout = 10 * time.Second
)

// Options 是服务的装配参数。
type Options struct {
	// Config 是配置，必填。
	Config *config.Config
	// Channels 是渠道客户端集合，必填。
	Channels *channel.Set
	// Plugins 是插件集合，为 nil 时视为没有插件。
	Plugins *plugin.Store
	// Engine 是转换内核，为 nil 时按配置构造。
	Engine *relay.Engine
	// Logger 是日志入口，为 nil 时丢弃日志。
	Logger *slog.Logger
	// Now 返回当前时间，为 nil 时用 time.Now。
	Now func() time.Time
	// MaxBodyBytes 是单个请求体的上限，为 0 时用 config.DefaultMaxBodyBytes。
	MaxBodyBytes int64
	// OnRequestDone 在每次请求结束时同步调用，交出这次请求的原始量。
	//
	// ctx 是本次请求一直沿用下来的那个：宿主在 Options.Middleware 里加进 context 的
	// 值在这里能读到，用来把请求级的记账状态跟结束时这份原始量对上。
	//
	// 为 nil 时什么也不做，也不构造 Outcome。
	OnRequestDone func(context.Context, Outcome)
	// Middleware 包住内核的整个处理器，包括访问日志。宿主在这里记自己的日志、
	// 测首字节、加响应头、接 tracing，也可以换掉请求体（内核从 request.Body 读，
	// 换过之后参与路由的就是新内容）或根本不调用 next。
	//
	// 装在外侧：宿主加进 context 的值会一路传下去，OnRequestDone 的 ctx 里有它。
	//
	// 改请求头不会传到上游：上游请求头来自渠道配置与 `Auth`，内核不从进来的请求里抄。
	// 要按请求加上游头，在中间件里调 WithUpstreamHeader 往 ctx 上挂。
	//
	// 包装 http.ResponseWriter 时必须把 Flush 转发下去，否则流式响应会静默退化成
	// 不刷新。
	//
	// 为 nil 时什么都不装。
	Middleware func(http.Handler) http.Handler
	// DisableAccessLog 关掉内核自己那两行访问日志。
	//
	// 关掉的是「请求进入」与「请求完成」，包括失败请求的 WARN 完成行；request_id 仍然
	// 生成、其余 WARN 行照旧带着它，OnRequestDone 也不受影响。
	// 进入行里的 client 与 user_agent 会一起没有，需要的话从 Outcome 的同名字段自己记。
	// 默认为 false，即照常记录。
	DisableAccessLog bool
}

// Option 覆盖 FromConfig 的装配参数。
type Option func(*Options)

// WithRequestDone 设置每次请求结束时调用的回调，见 Options.OnRequestDone。
func WithRequestDone(onDone func(context.Context, Outcome)) Option {
	return func(options *Options) { options.OnRequestDone = onDone }
}

// WithMiddleware 设置包住内核处理器的宿主中间件，见 Options.Middleware。
func WithMiddleware(middleware func(http.Handler) http.Handler) Option {
	return func(options *Options) { options.Middleware = middleware }
}

// WithoutAccessLog 关掉内核自己那两行访问日志，见 Options.DisableAccessLog。
func WithoutAccessLog() Option {
	return func(options *Options) { options.DisableAccessLog = true }
}

// Server 是一个转发服务。
type Server struct {
	config       *config.Config
	channels     *channel.Set
	plugins      *plugin.Store
	engine       *relay.Engine
	logger       *slog.Logger
	now          func() time.Time
	maxBodyBytes int64
	handler      http.Handler
	// requestCounter 是访问日志里请求 id 的来源，进程内自增。
	requestCounter atomic.Uint64
	// affinityTable 记住「请求前缀 → 渠道」，供没有会话标识的客户端兜底。
	affinityTable *affinity.Table
	// minPrefixBytes 是前缀的字节下限，短于它时不做亲和。零表示不判断。
	minPrefixBytes int
	// onRequestDone 是宿主的请求结束回调，为 nil 表示不回调。
	onRequestDone func(context.Context, Outcome)
	// disableAccessLog 为真时不写内核自己那两行访问日志。
	disableAccessLog bool
}

// New 创建服务。
func New(opts Options) (*Server, error) {
	if opts.Config == nil {
		return nil, errors.New("server: Config 不能为空")
	}
	if opts.Channels == nil {
		return nil, errors.New("server: Channels 不能为空")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	engine := opts.Engine
	if engine == nil {
		engine = relay.NewEngine(relay.Options{})
	}
	plugins := opts.Plugins
	if plugins == nil {
		plugins = plugin.EmptyStore(logger)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = config.DefaultMaxBodyBytes
	}
	server := &Server{
		config:       opts.Config,
		channels:     opts.Channels,
		plugins:      plugins,
		engine:       engine,
		logger:       logger,
		now:          now,
		maxBodyBytes: maxBodyBytes,
		affinityTable: affinity.NewTable(
			opts.Config.AffinityTableSize(),
			opts.Config.AffinityTTL(),
			now,
		),
		minPrefixBytes:   opts.Config.AffinityMinPrefixBytes(),
		onRequestDone:    opts.OnRequestDone,
		disableAccessLog: opts.DisableAccessLog,
	}
	server.handler = server.routes()
	if opts.Middleware != nil {
		// 装在外侧：宿主中间件才是服务的边界，访问日志包在它里面。这样宿主往
		// context 里加的值会一路传下去，OnRequestDone 收到的 ctx 里有它。
		server.handler = opts.Middleware(server.handler)
	}
	return server, nil
}

// FromConfig 按配置装配渠道与插件，再创建服务。
//
// 配置里启用但目录里不存在的插件会直接报错：静默降级会让「插件没生效」变成
// 一个只能靠读日志发现的问题。
func FromConfig(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	return FromConfigWith(cfg, logger)
}

// FromConfigWith 与 FromConfig 相同，另外可以用 opts 覆盖装配参数。
//
// 做成单独一个函数而不是给 FromConfig 加可变参数：可变参数会让它不再能赋给
// func(*config.Config, *slog.Logger) (*Server, error) 类型的变量，按仓库的兼容
// 承诺算破坏性变更。要完全掌控装配就直接用 New。
func FromConfigWith(cfg *config.Config, logger *slog.Logger, opts ...Option) (*Server, error) {
	specs := make([]channel.Spec, 0, len(cfg.Channels))
	for _, item := range cfg.Channels {
		itemDialect, _ := dialect.Parse(item.Dialect)
		specs = append(specs, channel.Spec{
			Name:       item.Name,
			Dialect:    itemDialect,
			BaseURL:    item.BaseURL,
			Path:       item.Path,
			Auth:       channel.Auth(item.Auth),
			Timeout:    item.Timeout.Duration(),
			MaxRetries: retriesOf(item),
			Headers:    item.Headers,
		})
	}
	channels, err := channel.NewSet(specs)
	if err != nil {
		return nil, err
	}
	plugins, err := BuildPlugins(cfg, logger)
	if err != nil {
		return nil, err
	}
	options := Options{
		Config:   cfg,
		Channels: channels,
		Plugins:  plugins,
		Logger:   logger,
	}
	for _, apply := range opts {
		// nil 当空操作：宿主用变量拼选项时递进来 nil 不该让装配炸掉。
		if apply == nil {
			continue
		}
		apply(&options)
	}
	return New(options)
}

// BuildPlugins 按配置加载插件目录并构造插件集合。
//
// 插件目录不存在且没有启用任何插件时视为没有插件；启用了插件却找不到目录则报错，
// 否则「插件没生效」只能靠读日志发现。
func BuildPlugins(cfg *config.Config, logger *slog.Logger) (*plugin.Store, error) {
	enabled := cfg.EnabledPlugins()
	loaded, err := plugin.LoadDir(cfg.Plugins.Dir, plugin.DefaultLimits())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && len(enabled) == 0 {
			return plugin.NewStore(nil, logger)
		}
		return nil, err
	}
	byKey := make(map[string]*plugin.Loaded, len(loaded))
	for _, item := range loaded {
		byKey[item.Key()] = item
	}
	positions := make(map[string]int, len(loaded))
	for index, item := range loaded {
		positions[item.Key()] = index
	}
	var entries []plugin.Entry
	var missing []string
	for _, item := range enabled {
		loadedPlugin, ok := byKey[item.Key]
		if !ok {
			missing = append(missing, item.Key)
			continue
		}
		entries = append(entries, plugin.Entry{
			Instance: loadedPlugin.Bind(item.Config),
			Enabled:  true,
			Order:    item.Order,
			Position: positions[item.Key],
		})
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("server: 配置里启用但 %s 里找不到的插件: %s",
			cfg.Plugins.Dir, strings.Join(missing, "、"))
	}
	return plugin.NewStore(entries, logger)
}

// Handler 返回 HTTP 处理器，便于嵌入其它服务或直接做测试。
func (s *Server) Handler() http.Handler { return s.handler }

// Listen 按配置创建监听器。ctx 只影响监听建立阶段。
func (s *Server) Listen(ctx context.Context) (net.Listener, error) {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.config.Listen)
	if err != nil {
		return nil, fmt.Errorf("server: 监听 %s 失败: %w", s.config.Listen, err)
	}
	return listener, nil
}

// Run 监听并服务，直到 ctx 取消。ctx 取消后停止接收新连接并等待在途请求结束。
func (s *Server) Run(ctx context.Context) error {
	listener, err := s.Listen(ctx)
	if err != nil {
		return err
	}
	return s.Serve(ctx, listener)
}

// Serve 在给定监听器上服务，直到 ctx 取消。
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	httpServer := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		// 不设 WriteTimeout：流式响应会长时间占用写通道，总时长由调用方的
		// context 与上游超时共同约束。
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.Serve(listener) }()
	s.logger.Info("开始监听", "address", listener.Addr().String())
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		// 脱离取消信号但保留调用链：这里需要的是一个不再被取消的短窗口。
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			return fmt.Errorf("server: 关闭失败: %w", err)
		}
		return nil
	}
}

// retriesOf 返回渠道的重试次数。
func retriesOf(item config.Channel) int {
	if item.MaxRetries == nil {
		return 0
	}
	return *item.MaxRetries
}
