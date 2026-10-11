// Package config 定义 core 的配置文件模型、加载与校验。
//
// 配置只描述事实，不承载行为：它不知道渠道客户端与插件宿主的存在，装配由 server
// 层完成。凭据一律只写环境变量名，配置文件里不出现密钥。
package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tokenmp/core/dialect"
)

// 默认值。只在这里写一次，Load 与文档都以它为准。
const (
	DefaultListen       = "127.0.0.1:8787"
	DefaultLogFormat    = "json"
	DefaultLogLevel     = "info"
	DefaultTimeout      = 120 * time.Second
	DefaultPluginDir    = "plugins"
	DefaultMaxBodyBytes = 32 << 20
	// DefaultMaxTokens 是源请求未带 max_tokens 时给 Claude 补的值。它与
	// relay.DefaultClaudeMaxTokens 是同一个数：分层红线不允许 config 导入 relay，
	// 只能各写一份，改一处要同时改另一处。
	DefaultMaxTokens = 4096
	// MaxRetries 是渠道重试次数的上限，与 channel.MaxRetries 保持一致。
	MaxRetries = 10
	// 渠道亲和的参数默认值。
	DefaultAffinityTableSize = 8192
	DefaultAffinityTTL       = 10 * time.Minute
	// DefaultAffinityMinPrefixBytes 是前缀的字节下限。按中英文混排估算，4096 字节
	// 大致是一千出头到两千 tokens，落在上游最小可缓存长度（按模型 512 到 4096
	// tokens）的下半段。它是个启发式，估不准时应当按实际模型调高。
	DefaultAffinityMinPrefixBytes = 4096
)

// authStyleNone 表示不鉴权。单列成常量是因为它在校验里有独立分支。
const authStyleNone = "none"

// 日志格式、级别与鉴权方式的合法取值。
var (
	logFormats = []string{"json", "text"}
	logLevels  = []string{"debug", "info", "warn", "error"}
	authStyles = []string{"bearer", "x-api-key", "query", "custom", authStyleNone}
)

// Config 是一份完整的服务配置。
type Config struct {
	// Path 是配置文件的绝对路径，由 Load 填入。Parse 不填。
	Path   string `yaml:"-"`
	Listen string `yaml:"listen"`
	Log    Log    `yaml:"log"`
	// Behavior 是 defaults 段，放进程级的行为默认值。
	//
	// 字段名不叫 Defaults：Config.Defaults() 已经是公开方法，同名字段与它冲突。
	// YAML 键固定写作 defaults，那是配置文件里的名字，不随 Go 侧命名走。
	Behavior Behavior `yaml:"defaults"`
	// Accounts 是加载期的顶层账号清单，Expand 之后为空。
	Accounts []Account `yaml:"accounts"`
	Channels []Channel `yaml:"channels"`
	Models   []Model   `yaml:"models"`
	Plugins  Plugins   `yaml:"plugins"`
	Affinity Affinity  `yaml:"affinity"`
}

// Behavior 是进程级的行为默认值，YAML 里写成 defaults 段。
//
// 两个数都是「装配时读一次、运行期不再变」的进程级参数，因此不塞进渠道或模型
// 这些既有段：请求体上限属于整个进程，补 max_tokens 是内核的部署级行为。用指针
// 是为了把「没写」与「显式写成 0」区分开，0 不是合法值，校验会拦下。
//
// 内核还有 tool_loss_policy、model_suffix、thinking_adapter 三个行为开关，
// 这里没有对应的键：前两个会改变「哪个模型名参与匹配」，打开之后 models[].upstream
// 与 ${model} 的语义跟着变，要另写一节语义说明；tool_loss_policy 等「要不要允许
// reject」的口径定了再上。在它们落地之前，宿主仍可在 Go 侧构造 relay.Options
// 注入内核。
type Behavior struct {
	// MaxBodyBytes 是客户端请求体的字节上限。
	MaxBodyBytes *int `yaml:"max_body_bytes"`
	// DefaultMaxTokens 是源请求未带 max_tokens 时给 Claude 补的值。
	DefaultMaxTokens *int `yaml:"default_max_tokens"`
}

// Affinity 是渠道亲和的参数。哪些渠道参与亲和由 Model.Affinity 的组名决定，
// 这里只放参数。
//
// 三个字段都是可选的，不写就用本文件里的默认常量。用指针是为了把「没写」与
// 「显式写成 0」区分开：最小前缀长度写成 0 表示不做这个判断。
type Affinity struct {
	// TableSize 是前缀链记忆的条数上限。不大于零时没有会话标识的客户端不参与
	// 亲和。
	TableSize *int `yaml:"table_size"`
	// TTL 是前缀链记忆的寿命，按上游缓存的寿命设。
	TTL Duration `yaml:"ttl"`
	// MinPrefixBytes 是前缀的字节下限，短于它时不做亲和。
	MinPrefixBytes *int `yaml:"min_prefix_bytes"`
}

// Log 是日志配置。
type Log struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

// Account 是一个上游账号。账号不是渠道，渠道在 auth.accounts 里按名字引用它，
// 加载时展开成多条渠道。单列一层是因为同一把凭据常要配给多种方言或同一上游的多个
// 入口，写成渠道就得把基址、路径、超时逐条抄一遍。
//
// Headers 是账号级的附加请求头，用于上游按 workspace 或 organization 隔离的场景。
type Account struct {
	Name    string            `yaml:"name"`
	KeyEnv  string            `yaml:"key_env"`
	Headers map[string]string `yaml:"headers"`
}

// Channel 是一个上游渠道。BaseURL 是渠道基址，Path 为空时按方言取默认路径。
type Channel struct {
	Name       string            `yaml:"name"`
	Dialect    string            `yaml:"dialect"`
	BaseURL    string            `yaml:"base_url"`
	Path       string            `yaml:"path"`
	Auth       Auth              `yaml:"auth"`
	Timeout    Duration          `yaml:"timeout"`
	MaxRetries *int              `yaml:"max_retries"`
	Headers    map[string]string `yaml:"headers"`
}

// Auth 是渠道的鉴权方式。凭据值只从 KeyEnv 指定的环境变量读取，或由 Accounts
// 引用的顶层账号给出。
type Auth struct {
	// Style 取 bearer、x-api-key、query、custom 或 none。
	Style string `yaml:"style"`
	// KeyEnv 是凭据所在的环境变量名。
	KeyEnv string `yaml:"key_env"`
	// Accounts 是引用的顶层账号名，与 KeyEnv 互斥。它也是加载期字段：Expand
	// 把账号的 KeyEnv 填进展开出的每条渠道，这里随之清空，平铺形态因而与手写
	// 的等价配置完全一致。
	Accounts []string `yaml:"accounts"`
	// Header 是 Style 为 custom 时的请求头名。
	Header string `yaml:"header"`
	// Query 是 Style 为 query 时的查询参数名。
	Query string `yaml:"query"`
}

// Model 是一条模型路由规则。Match 支持 * 通配，同一个模型可以有多条规则，
// 按配置顺序依次尝试。
//
// Upstream 是发往上游的模型名，可以写 ${model} 占位符，替换成客户端请求里
// 经插件改写后真正参与匹配的那个模型名。写法与校验规则见 Validate 与
// docs/config.md。
//
// Affinity 是渠道亲和的组名。命中该模型的规则里，组名相同的候选组成一个池，
// 池里的渠道被当作可互换的；同一段会话的连续请求会在池内粘住一条，池外的候选
// 按配置顺序排在池后面兜底。留空表示不参与。
type Model struct {
	Match    string `yaml:"match"`
	Channel  string `yaml:"channel"`
	Upstream string `yaml:"upstream"`
	Dialect  string `yaml:"dialect"`
	Affinity string `yaml:"affinity"`
}

// Plugin 是一个插件的启用状态与顺序。
type Plugin struct {
	Key     string         `yaml:"key"`
	Enabled bool           `yaml:"enabled"`
	Order   int            `yaml:"order"`
	Config  map[string]any `yaml:"config"`
}

// Plugins 是插件目录与插件清单。
type Plugins struct {
	// Dir 是插件目录，相对路径以配置文件所在目录为基准。
	Dir  string   `yaml:"dir"`
	List []Plugin `yaml:"list"`
}

// Route 是解析后的一条候选路由。
type Route struct {
	Channel  string
	Upstream string
	Dialect  dialect.Name
	// Affinity 取自语规则，是这条候选所属的亲和组名，空串表示不参与。
	Affinity string
}

// ChannelByName 按名字取渠道配置。
func (c *Config) ChannelByName(name string) (Channel, bool) {
	for _, channel := range c.Channels {
		if channel.Name == name {
			return channel, true
		}
	}
	return Channel{}, false
}

// Resolve 返回该模型名的候选路由，按配置顺序排列。没有匹配项时返回空切片。
func (c *Config) Resolve(model string) []Route {
	var routes []Route
	for _, rule := range c.Models {
		if !Match(rule.Match, model) {
			continue
		}
		channel, ok := c.ChannelByName(rule.Channel)
		if !ok {
			continue
		}
		name := channel.Dialect
		if rule.Dialect != "" {
			name = rule.Dialect
		}
		target, ok := dialect.Parse(name)
		if !ok {
			continue
		}
		upstream := rule.Upstream
		if upstream == "" {
			upstream = model
		} else {
			// 占位符只认 ${model}，未知写法在 Validate 里已经拦下；
			// 这里替换后其余字符原样保留。
			upstream = strings.ReplaceAll(upstream, "${model}", model)
		}
		routes = append(routes, Route{
			Channel:  channel.Name,
			Upstream: upstream,
			Dialect:  target,
			Affinity: rule.Affinity,
		})
	}
	return routes
}

// EnabledPlugins 返回启用中的插件，按 order 升序排列；order 相同时保持配置顺序。
func (c *Config) EnabledPlugins() []Plugin {
	enabled := make([]Plugin, 0, len(c.Plugins.List))
	for _, item := range c.Plugins.List {
		if item.Enabled {
			enabled = append(enabled, item)
		}
	}
	sort.SliceStable(enabled, func(i, j int) bool { return enabled[i].Order < enabled[j].Order })
	return enabled
}

// PluginConfig 返回指定插件的配置对象，未配置时返回空 map。
func (c *Config) PluginConfig(key string) map[string]any {
	for _, item := range c.Plugins.List {
		if item.Key == key {
			if item.Config == nil {
				return map[string]any{}
			}
			return item.Config
		}
	}
	return map[string]any{}
}

// Defaults 填入未写的默认值。校验前调用。
func (c *Config) Defaults() {
	if strings.TrimSpace(c.Listen) == "" {
		c.Listen = DefaultListen
	}
	if strings.TrimSpace(c.Log.Format) == "" {
		c.Log.Format = DefaultLogFormat
	}
	if strings.TrimSpace(c.Log.Level) == "" {
		c.Log.Level = DefaultLogLevel
	}
	for i := range c.Channels {
		channel := &c.Channels[i]
		if channel.Timeout == 0 {
			channel.Timeout = Duration(DefaultTimeout)
		}
		if channel.MaxRetries == nil {
			zero := 0
			channel.MaxRetries = &zero
		}
	}
	if strings.TrimSpace(c.Plugins.Dir) == "" {
		c.Plugins.Dir = DefaultPluginDir
	}
}

// Validate 校验配置。返回的错误说明第几条配置出了什么问题。
func (c *Config) Validate() error {
	var problems []string
	problems = append(problems, checkEnum("log.format", c.Log.Format, logFormats)...)
	problems = append(problems, checkEnum("log.level", c.Log.Level, logLevels)...)

	accounts := make(map[string]bool, len(c.Accounts))
	referenced := make(map[string]bool, len(c.Accounts))
	for i, account := range c.Accounts {
		where := fmt.Sprintf("accounts[%d]", i)
		if account.Name == "" {
			problems = append(problems, where+": name 不能为空")
		} else if accounts[account.Name] {
			problems = append(problems, where+": 账号名 "+account.Name+" 重复")
		}
		accounts[account.Name] = true
		if account.KeyEnv == "" {
			problems = append(problems, where+": key_env 不能为空")
		}
	}

	seen := make(map[string]bool, len(c.Channels))
	for i, channel := range c.Channels {
		where := fmt.Sprintf("channels[%d]", i)
		if channel.Name == "" {
			problems = append(problems, where+": name 不能为空")
		} else if seen[channel.Name] {
			problems = append(problems, where+": 渠道名 "+channel.Name+" 重复")
		}
		seen[channel.Name] = true
		if channel.Dialect == "" {
			problems = append(problems, where+": dialect 不能为空")
		} else if _, ok := dialect.Parse(channel.Dialect); !ok {
			problems = append(problems, fmt.Sprintf("%s: dialect %q 不是支持的方言", where, channel.Dialect))
		}
		if channel.BaseURL == "" {
			problems = append(problems, where+": base_url 不能为空")
		} else if !strings.HasPrefix(channel.BaseURL, "http://") && !strings.HasPrefix(channel.BaseURL, "https://") {
			problems = append(problems, where+": base_url 必须是 http:// 或 https:// 开头")
		}
		if channel.Timeout <= 0 {
			problems = append(problems, where+": timeout 必须为正")
		}
		if channel.MaxRetries != nil && (*channel.MaxRetries < 0 || *channel.MaxRetries > MaxRetries) {
			problems = append(problems, fmt.Sprintf("%s: max_retries 须在 0 到 %d 之间", where, MaxRetries))
		}
		problems = append(problems, checkAuth(where+".auth", channel.Auth, channel.Name)...)
		for _, name := range channel.Auth.Accounts {
			if !accounts[name] {
				problems = append(problems, fmt.Sprintf("%s.auth.accounts: 引用了不存在的账号 %s", where, name))
			}
			referenced[name] = true
		}
		// 纯内容判断：展开后的渠道两样只剩 KeyEnv，因此平铺形态不会触发。
		if len(channel.Auth.Accounts) > 0 && channel.Auth.KeyEnv != "" {
			problems = append(problems, where+".auth.accounts: 与 auth.key_env 互斥，只能写一个")
		}
	}

	for i, account := range c.Accounts {
		if account.Name != "" && !referenced[account.Name] {
			problems = append(problems, fmt.Sprintf("accounts[%d]: 账号 %s 没有被任何渠道引用", i, account.Name))
		}
	}

	for i, rule := range c.Models {
		where := fmt.Sprintf("models[%d]", i)
		if rule.Match == "" {
			problems = append(problems, where+": match 不能为空")
		} else if _, err := CompileMatch(rule.Match); err != nil {
			problems = append(problems, where+": "+err.Error())
		}
		if rule.Channel == "" {
			problems = append(problems, where+": channel 不能为空")
		} else if !seen[rule.Channel] {
			problems = append(problems, where+": 引用了不存在的渠道 "+rule.Channel)
		}
		if rule.Dialect != "" {
			if _, ok := dialect.Parse(rule.Dialect); !ok {
				problems = append(problems, fmt.Sprintf("%s: dialect %q 不是支持的方言", where, rule.Dialect))
			}
		}
		problems = append(problems, checkUpstream(where, rule.Upstream)...)
	}

	keys := make(map[string]bool, len(c.Plugins.List))
	for i, item := range c.Plugins.List {
		where := fmt.Sprintf("plugins.list[%d]", i)
		if item.Key == "" {
			problems = append(problems, where+": key 不能为空")
		} else if keys[item.Key] {
			problems = append(problems, where+": 插件 key "+item.Key+" 重复")
		}
		keys[item.Key] = true
	}

	problems = append(problems, checkBehavior(c.Behavior)...)
	problems = append(problems, checkAffinity(c.Affinity)...)

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("config: 配置有 %d 处问题:\n  - %s", len(problems), strings.Join(problems, "\n  - "))
}

// checkUpstream 校验 upstream 里的占位符写法。
//
// $ 只在紧跟 { 时进入占位符形式，其余位置都是普通字符，因此 $$ 就是两个美元
// 符号，单独的 } 也没有特殊含义。进入占位符后只认 ${model}：${ 开了头却没有
// 配对的 } 报错，配对了但名字不是 model 也报错。两种错误都带上 models 下标。
// 不做捕获占位符，理由见 docs/config.md 的「条款」一节。
func checkUpstream(where, upstream string) []string {
	var problems []string
	for i := 0; i < len(upstream); i++ {
		if upstream[i] != '$' || i+1 >= len(upstream) || upstream[i+1] != '{' {
			continue
		}
		rest := upstream[i+2:]
		end := strings.IndexByte(rest, '}')
		if end < 0 {
			problems = append(problems, fmt.Sprintf("%s: upstream %q 里的 ${ 没有配对的 }", where, upstream))
			break
		}
		if name := rest[:end]; name != "model" {
			problems = append(problems, fmt.Sprintf("%s: upstream %q 里的 ${%s} 不是支持的占位符，只支持 ${model}", where, upstream, name))
		}
		// 跳过 ${name}，避免同一段被重复扫描。
		i += len("${") + end
	}
	return problems
}

// checkBehavior 校验行为默认值。两个数都是可选的，给了就得为正。
func checkBehavior(behavior Behavior) []string {
	var problems []string
	if behavior.MaxBodyBytes != nil && *behavior.MaxBodyBytes <= 0 {
		problems = append(problems, "defaults.max_body_bytes 必须为正")
	}
	if behavior.DefaultMaxTokens != nil && *behavior.DefaultMaxTokens <= 0 {
		problems = append(problems, "defaults.default_max_tokens 必须为正")
	}
	return problems
}

// checkAffinity 校验亲和参数。取值都是可选的，给了就得合理。
func checkAffinity(affinity Affinity) []string {
	var problems []string
	if affinity.TableSize != nil && *affinity.TableSize < 0 {
		problems = append(problems, "affinity.table_size 不能为负")
	}
	if affinity.TTL < 0 {
		problems = append(problems, "affinity.ttl 不能为负")
	}
	if affinity.MinPrefixBytes != nil && *affinity.MinPrefixBytes < 0 {
		problems = append(problems, "affinity.min_prefix_bytes 不能为负")
	}
	return problems
}

// checkAuth 校验鉴权配置。凭据值只能来自环境变量，因此这里不接受字面量密钥。
func checkAuth(where string, auth Auth, channel string) []string {
	var problems []string
	style := auth.Style
	if style == "" {
		// 不写 style 等于不鉴权，便于指向本地假上游。
		style = authStyleNone
	}
	if !inEnum(style, authStyles) {
		return []string{fmt.Sprintf("%s: style %q 不是支持的鉴权方式", where, style)}
	}
	// 凭据来源有两条：KeyEnv 直接给环境变量名，或 accounts 引用顶层账号。后者的
	// 环境变量名在展开时逐条填入，因此这里不再要求 KeyEnv。
	if style != authStyleNone && auth.KeyEnv == "" && len(auth.Accounts) == 0 {
		problems = append(problems, fmt.Sprintf("%s: 渠道 %s 的鉴权需要 key_env 指定环境变量名", where, channel))
	}
	if style == "custom" && auth.Header == "" {
		problems = append(problems, where+": style 为 custom 时必须给出 header")
	}
	if style == "query" && auth.Query == "" {
		problems = append(problems, where+": style 为 query 时必须给出 query")
	}
	return problems
}

// inEnum 报告取值是否在允许集合内。
func inEnum(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// checkEnum 返回取值不在允许集合内的错误说明，取值合法时返回空。
func checkEnum(field, value string, allowed []string) []string {
	if inEnum(value, allowed) {
		return nil
	}
	return []string{fmt.Sprintf("%s: %q 不在允许取值 %s 内", field, value, strings.Join(allowed, "、"))}
}

// Expand 返回展开顶层账号之后的副本，原对象不改。
//
// 顶层账号是加载期形态，因此这个方法是那条约定的唯一入口：Load 与 Parse 解析
// 完自己会调一次，从它们拿到的 Config 已经是平铺的。程序化构造带账号的 Config
// 之后要自己再调一次，否则展开出的渠道没被建出来，渠道的 Auth.KeyEnv 是空的。
//
// 展开按内容判断是否还有账号数据：顶层 accounts 为空且没有渠道写 auth.accounts
// 时直接返回副本。判断基于字段内容而不是调用历史，所以平铺形态再展开不会让
// 候选数按账号数相乘，同一个对象重复调用结果也一致。
//
// 展开会清掉账号层的痕迹：顶层 accounts 与每条渠道的 auth.accounts 都不保留，
// 所以返回的 Config 能直接序列化后重新加载。
func (c *Config) Expand() *Config {
	flat := *c
	if len(flat.Accounts) == 0 && !referencesAccounts(flat.Channels) {
		return &flat
	}
	byName := make(map[string]Account, len(flat.Accounts))
	for _, account := range flat.Accounts {
		byName[account.Name] = account
	}
	flat.Accounts = nil
	// 基础渠道名到展开名的映射。模型规则按基础名查回来，没有账号的渠道不在表里。
	expandedNames := make(map[string][]string, len(flat.Channels))
	channels := make([]Channel, 0, len(flat.Channels))
	for _, channel := range flat.Channels {
		if len(channel.Auth.Accounts) == 0 {
			channels = append(channels, channel)
			continue
		}
		names := make([]string, 0, len(channel.Auth.Accounts))
		for _, name := range channel.Auth.Accounts {
			account, ok := byName[name]
			if !ok {
				// 不存在的引用在 Validate 里已经报错，Parse 走不到这里。
				// 公开类型仍可能被直接构造，缺账号时跳过比展开成空凭据安全。
				continue
			}
			item := channel
			item.Name = channel.Name + "/" + account.Name
			item.Auth.KeyEnv = account.KeyEnv
			item.Auth.Accounts = nil
			item.Headers = mergeHeaders(channel.Headers, account.Headers)
			channels = append(channels, item)
			names = append(names, item.Name)
		}
		if len(names) > 0 {
			expandedNames[channel.Name] = names
		}
	}
	flat.Channels = channels
	if len(expandedNames) == 0 {
		return &flat
	}
	models := make([]Model, 0, len(flat.Models))
	for _, rule := range flat.Models {
		names, ok := expandedNames[rule.Channel]
		if !ok {
			models = append(models, rule)
			continue
		}
		for _, name := range names {
			item := rule
			item.Channel = name
			models = append(models, item)
		}
	}
	flat.Models = models
	return &flat
}

// referencesAccounts 报告有没有渠道写 auth.accounts。它是展开幂等判断的一半：
// 只看字段内容，不做调用历史。
func referencesAccounts(channels []Channel) bool {
	for _, channel := range channels {
		if len(channel.Auth.Accounts) > 0 {
			return true
		}
	}
	return false
}

// mergeHeaders 合并渠道与账号的请求头，同名时账号覆盖渠道。
//
// 口径是「越具体越晚写」，与 channel/client.go 里先写 spec.Headers 再写请求头的
// 覆盖顺序一致。只要任一侧有头就新建 map：展开出的多条渠道若共用同一个 map，
// 宿主给一条加头会串到它兄弟上。两侧都为空时才返回 nil，不给展开结果塞空 map。
func mergeHeaders(channel, account map[string]string) map[string]string {
	if len(channel) == 0 && len(account) == 0 {
		return nil
	}
	merged := make(map[string]string, len(channel)+len(account))
	for name, value := range channel {
		merged[name] = value
	}
	for name, value := range account {
		merged[name] = value
	}
	return merged
}

// MaxBodyBytes 返回请求体的字节上限。没写 defaults.max_body_bytes 时返回
// DefaultMaxBodyBytes，因此不配置时的行为与逐字段写死默认值的版本一致。
func (c *Config) MaxBodyBytes() int64 {
	if c.Behavior.MaxBodyBytes == nil {
		return DefaultMaxBodyBytes
	}
	return int64(*c.Behavior.MaxBodyBytes)
}

// DefaultMaxTokens 返回源请求未带 max_tokens 时给 Claude 补的值。没写
// defaults.default_max_tokens 时返回 DefaultMaxTokens。
func (c *Config) DefaultMaxTokens() int {
	if c.Behavior.DefaultMaxTokens == nil {
		return DefaultMaxTokens
	}
	return *c.Behavior.DefaultMaxTokens
}

// AffinityTableSize 返回前缀链记忆的条数上限。
func (c *Config) AffinityTableSize() int {
	if c.Affinity.TableSize == nil {
		return DefaultAffinityTableSize
	}
	return *c.Affinity.TableSize
}

// AffinityTTL 返回前缀链记忆的寿命。
func (c *Config) AffinityTTL() time.Duration {
	if c.Affinity.TTL <= 0 {
		return DefaultAffinityTTL
	}
	return c.Affinity.TTL.Duration()
}

// AffinityMinPrefixBytes 返回前缀的字节下限。返回零表示不做这个判断。
func (c *Config) AffinityMinPrefixBytes() int {
	if c.Affinity.MinPrefixBytes == nil {
		return DefaultAffinityMinPrefixBytes
	}
	return *c.Affinity.MinPrefixBytes
}
