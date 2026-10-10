package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Calcium-Ion/moejs"
	"github.com/Calcium-Ion/moejs/engine"
)

// 插件文件名。目录形式为 <key>/plugin.js，也接受平铺的 <key>.js。
const (
	PluginFile     = "plugin.js"
	pluginFileExt  = ".js"
	manifestExport = "meta"
)

// 错误集合。
var (
	// ErrPayloadTooLarge 表示单次输入或输出超过限额。
	ErrPayloadTooLarge = errors.New("plugin: 负载超过限额")
	// ErrAsyncHook 表示钩子返回了 Promise。钩子必须是同步函数。
	ErrAsyncHook = errors.New("plugin: 钩子必须是同步函数，不能返回 Promise")
	// ErrMissingMeta 表示插件没有导出 meta。
	ErrMissingMeta = errors.New("plugin: 缺少 meta 导出")
	// ErrHookMissing 表示插件没有导出调用方要求的钩子。
	ErrHookMissing = errors.New("plugin: 插件未导出该钩子")
)

// Loaded 是编译好的插件。编译一次，可被任意多个 Instance 复用，可并发使用。
type Loaded struct {
	manifest Manifest
	path     string
	module   *moejs.Module
	hooks    map[Hook]moejs.Hook
	limits   Limits

	slots chan struct{}
}

// Compile 编译插件源码并读出它的自述信息与钩子。
//
// path 只用于报错与展示。key 取自 meta.key。
func Compile(path string, source []byte, limits Limits) (*Loaded, error) {
	limits = limits.fill()
	if len(source) > limits.SourceBytes {
		return nil, fmt.Errorf("plugin: %s 源码 %d 字节，超过上限 %d", path, len(source), limits.SourceBytes)
	}
	module, err := moejs.Compile(path, string(source))
	if err != nil {
		return nil, fmt.Errorf("plugin: 编译 %s 失败: %w", path, err)
	}
	runtime := moejs.NewRuntime(runtimeOptions())
	manifest, hooks, err := inspect(runtime, module, limits.Timeout)
	if err != nil {
		return nil, fmt.Errorf("plugin: %s: %w", path, err)
	}
	return &Loaded{
		manifest: manifest,
		path:     path,
		module:   module,
		hooks:    hooks,
		limits:   limits,
		slots:    make(chan struct{}, limits.Concurrency),
	}, nil
}

// runtimeOptions 是插件运行时的固定选项：关掉动态代码，插件因此不能用 eval 与
// Function 构造器绕过源码审查。
func runtimeOptions() moejs.Options {
	return moejs.Options{DisableDynamicCode: true}
}

// inspect 在一个运行时里读出 meta 与钩子导出。
//
// 必须先 Load 再 Export：导出绑定要等模块求值完成后才可读。
// Load 会执行模块顶层代码，因此这里和调用路径一样带超时。
func inspect(runtime *moejs.Runtime, module *moejs.Module, timeout time.Duration) (Manifest, map[Hook]moejs.Hook, error) {
	timer := time.AfterFunc(timeout, func() { runtime.Interrupt(errTimeout{phase: "源码求值", limit: timeout}) })
	defer func() {
		timer.Stop()
		runtime.ClearInterrupt()
	}()
	if err := runtime.Load(module); err != nil {
		return Manifest{}, nil, fmt.Errorf("加载模块失败: %w", err)
	}
	value, ok := runtime.Export(manifestExport)
	if !ok {
		return Manifest{}, nil, ErrMissingMeta
	}
	var manifest Manifest
	if err := runtime.Unmarshal(value, &manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("解析 %s 失败: %w", manifestExport, err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, nil, err
	}
	hooks := make(map[Hook]moejs.Hook, len(hookOrder))
	exported := make(map[string]bool)
	for _, name := range module.Exports() {
		exported[name] = true
	}
	for _, hook := range hookOrder {
		if !exported[string(hook)] {
			continue
		}
		value, ok := runtime.Export(string(hook))
		if !ok || !engine.IsCallable(value) {
			return Manifest{}, nil, fmt.Errorf("导出 %s 不是函数", hook)
		}
		handle, err := module.Hook(string(hook))
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("解析钩子 %s 失败: %w", hook, err)
		}
		hooks[hook] = handle
	}
	if len(hooks) == 0 {
		return Manifest{}, nil, fmt.Errorf("没有导出任何钩子，可用钩子为 %s", hookNames())
	}
	for _, hook := range hookOrder {
		if _, ok := hooks[hook]; ok {
			manifest.Hooks = append(manifest.Hooks, hook)
		}
	}
	return manifest, hooks, nil
}

// hookNames 返回钩子名的可读列表。
func hookNames() string {
	names := make([]string, 0, len(hookOrder))
	for _, hook := range hookOrder {
		names = append(names, string(hook))
	}
	return strings.Join(names, "、")
}

// LoadFile 读取并编译一个插件文件。
func LoadFile(path string, limits Limits) (*Loaded, error) {
	source, err := readSource(path, limits.fill().SourceBytes)
	if err != nil {
		return nil, err
	}
	loaded, err := Compile(path, source, limits)
	if err != nil {
		return nil, err
	}
	expected := keyFromPath(path)
	if expected != "" && loaded.manifest.Key != expected {
		return nil, fmt.Errorf("plugin: %s 的 meta.key 是 %q，与文件所在位置要求的 %q 不一致",
			path, loaded.manifest.Key, expected)
	}
	return loaded, nil
}

// LoadDir 扫描插件目录，按文件名排序返回全部插件。
//
// 目录里每一项要么是子目录，要么是 .js 文件；两种形式都要求名字与 meta.key 一致。
// 其它文件与目录会被忽略，因此插件目录里可以放图标与说明。
func LoadDir(dir string, limits Limits) ([]*Loaded, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// 不额外包 os.ErrNotExist：os.ReadDir 自己的错误已经能满足
		// errors.Is(err, fs.ErrNotExist)，多包一层会让权限错误也变成「不存在」。
		return nil, fmt.Errorf("plugin: 读取插件目录 %s 失败: %w", dir, err)
	}
	var paths []string
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.IsDir():
			candidate := filepath.Join(dir, name, PluginFile)
			if _, err := os.Stat(candidate); err == nil {
				paths = append(paths, candidate)
			}
		case strings.HasSuffix(name, pluginFileExt):
			paths = append(paths, filepath.Join(dir, name))
		}
	}
	sort.Strings(paths)
	loaded := make([]*Loaded, 0, len(paths))
	for _, path := range paths {
		item, err := LoadFile(path, limits)
		if err != nil {
			return nil, err
		}
		loaded = append(loaded, item)
	}
	return loaded, nil
}

// keyFromPath 从插件路径推出期望的 key。
func keyFromPath(path string) string {
	base := filepath.Base(path)
	if base == PluginFile {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(base, pluginFileExt)
}

// readSource 按上限读取插件源码。
func readSource(path string, limit int) ([]byte, error) {
	//nolint:gosec // 路径来自调用方：插件本来就是管理员指定的输入。
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("plugin: 打开 %s 失败: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	source, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("plugin: 读取 %s 失败: %w", path, err)
	}
	if len(source) > limit {
		return nil, fmt.Errorf("plugin: %s 源码超过上限 %d 字节", path, limit)
	}
	return source, nil
}

// Key 返回插件 key。
func (l *Loaded) Key() string { return l.manifest.Key }

// Path 返回插件文件路径。
func (l *Loaded) Path() string { return l.path }

// Manifest 返回自述信息的副本。
func (l *Loaded) Manifest() Manifest { return l.manifest }

// Limits 返回限额。
func (l *Loaded) Limits() Limits { return l.limits }

// Bind 把一份配置绑到插件上，得到可调用的实例。
func (l *Loaded) Bind(config map[string]any) *Instance {
	if config == nil {
		config = map[string]any{}
	}
	return &Instance{loaded: l, config: config}
}

// Instance 是绑定配置后的插件。可以并发调用。
type Instance struct {
	loaded *Loaded
	config map[string]any
}

// Key 返回插件 key。
func (i *Instance) Key() string { return i.loaded.manifest.Key }

// Path 返回插件文件路径。
func (i *Instance) Path() string { return i.loaded.path }

// Manifest 返回自述信息。
func (i *Instance) Manifest() Manifest { return i.loaded.manifest }

// Config 返回绑定到实例上的配置。
func (i *Instance) Config() map[string]any { return i.config }

// Has 报告插件是否导出了该钩子。
func (i *Instance) Has(hook Hook) bool {
	_, ok := i.loaded.hooks[hook]
	return ok
}

// Session 是一次调用携带的宿主环境。
type Session struct {
	// Now 是插件内 host.now() 看到的时间。
	Now time.Time
	// Logger 接收插件通过 host.log 写出的日志，为 nil 时丢弃。
	Logger *slog.Logger
}

// Call 调用一个钩子。
//
// input 与返回的 out 都是 JSON。第二个返回值报告插件是否给出了新值：返回
// null 或 undefined 表示不改写，此时 out 等于 input。
//
// 调用超时会中断插件，超时与异常都按错误返回，调用方按「改写失效、原样转发」
// 处理。
func (i *Instance) Call(ctx context.Context, hook Hook, input []byte, session Session) ([]byte, bool, error) {
	handle, ok := i.loaded.hooks[hook]
	if !ok {
		return input, false, fmt.Errorf("%w: %s / %s", ErrHookMissing, i.Key(), hook)
	}
	if len(input) > i.loaded.limits.PayloadBytes {
		return nil, false, fmt.Errorf("%w: 输入 %d 字节超过 %d", ErrPayloadTooLarge, len(input), i.loaded.limits.PayloadBytes)
	}
	select {
	case i.loaded.slots <- struct{}{}:
		defer func() { <-i.loaded.slots }()
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}

	// 每次调用用一个全新的运行时：插件在全新全局环境里求值一次，顶层代码
	// 捕获到的 host 因此总是本次调用的那一份。运行时的创建开销在微秒级，
	// 换掉的是一类只在池化时才出现的串味问题。
	runtime := moejs.NewRuntime(runtimeOptions())
	if err := runtime.SetGlobal("host", i.hostGlobal(session)); err != nil {
		return nil, false, fmt.Errorf("plugin: 注入宿主对象失败: %w", err)
	}
	defer runtime.ReleaseCallData()

	// 计时从求值之前开始：顶层代码由插件编写，和钩子体一样可能失控。
	timeout := i.loaded.limits.Timeout
	timer := time.AfterFunc(timeout, func() {
		runtime.Interrupt(errTimeout{phase: "源码求值或钩子执行", limit: timeout})
	})
	if err := runtime.Load(i.loaded.module); err != nil {
		timer.Stop()
		runtime.ClearInterrupt()
		return nil, false, fmt.Errorf("plugin: 加载模块失败: %w", err)
	}
	argument, err := runtime.ParseJSON(input)
	if err != nil {
		timer.Stop()
		runtime.ClearInterrupt()
		return nil, false, fmt.Errorf("plugin: 解析输入失败: %w", err)
	}

	result, callErr := runtime.Call(handle, argument)
	// 先停表再清中断：反过来会给一个已经结束的调用留下待处理的中断。
	timer.Stop()
	runtime.ClearInterrupt()
	if callErr != nil {
		return nil, false, fmt.Errorf("plugin: %s.%s 执行失败: %w", i.Key(), hook, callErr)
	}
	if _, _, pending := moejs.PromiseResult(result); pending {
		return nil, false, ErrAsyncHook
	}
	if isNullish(result) {
		return input, false, nil
	}
	output, err := runtime.AppendJSON(nil, result)
	if err != nil {
		return nil, false, fmt.Errorf("plugin: 序列化 %s.%s 的输出失败: %w", i.Key(), hook, err)
	}
	if len(output) > i.loaded.limits.PayloadBytes {
		return nil, false, fmt.Errorf("%w: 输出 %d 字节超过 %d", ErrPayloadTooLarge, len(output), i.loaded.limits.PayloadBytes)
	}
	return output, true, nil
}

// hostGlobal 构造插件看到的 host 对象。
//
// 只提供三样东西：本插件的配置、本插件的自述信息、时间与日志。刻意不提供环境
// 变量、网络与文件系统读取 —— 插件拿得到的能力就是这几样。
func (i *Instance) hostGlobal(session Session) map[string]any {
	now := session.Now
	if now.IsZero() {
		now = time.Now()
	}
	seconds := now.Unix()
	logger := session.Logger
	key := i.Key()
	return map[string]any{
		"config": i.config,
		"plugin": map[string]any{
			"key":        key,
			"name":       i.loaded.manifest.Name,
			"version":    i.loaded.manifest.Version,
			"apiVersion": i.loaded.manifest.APIVersion,
		},
		"now": moejs.NativeFunc(func(_ *moejs.Realm, _ moejs.Value, _ []moejs.Value) (moejs.Value, error) {
			return moejs.Int(seconds), nil
		}),
		"log": moejs.NativeFunc(func(realm *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
			if logger == nil {
				return moejs.Undefined(), nil
			}
			text, err := realm.ToString(moejs.Arg(args, 0))
			if err != nil {
				return moejs.Undefined(), err
			}
			logger.Info("插件日志", "plugin", key, "message", text.GoString())
			return moejs.Undefined(), nil
		}),
	}
}

// isNullish 报告值是否是 null 或 undefined。
func isNullish(value moejs.Value) bool {
	kind := value.Type()
	return kind == engine.TypeNull || kind == engine.TypeUndefined
}

// errTimeout 是超时中断的错误值。
type errTimeout struct {
	phase string
	limit time.Duration
}

// Error 实现 error。
func (e errTimeout) Error() string {
	return fmt.Sprintf("plugin: %s超过 %s 被中断", e.phase, e.limit)
}
