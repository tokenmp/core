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
	DefaultListen        = "127.0.0.1:8787"
	DefaultLogFormat     = "json"
	DefaultLogLevel      = "info"
	DefaultTimeout       = 120 * time.Second
	DefaultPluginDir     = "plugins"
	DefaultMaxBodyBytes  = 32 << 20
	DefaultMaxSourceSize = 1 << 20
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
	Path     string    `yaml:"-"`
	Listen   string    `yaml:"listen"`
	Log      Log       `yaml:"log"`
	Channels []Channel `yaml:"channels"`
	Models   []Model   `yaml:"models"`
	Plugins  Plugins   `yaml:"plugins"`
}

// Log 是日志配置。
type Log struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
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

// Auth 是渠道的鉴权方式。凭据值只从 KeyEnv 指定的环境变量读取。
type Auth struct {
	// Style 取 bearer、x-api-key、query、custom 或 none。
	Style string `yaml:"style"`
	// KeyEnv 是凭据所在的环境变量名。
	KeyEnv string `yaml:"key_env"`
	// Header 是 Style 为 custom 时的请求头名。
	Header string `yaml:"header"`
	// Query 是 Style 为 query 时的查询参数名。
	Query string `yaml:"query"`
}

// Model 是一条模型路由规则。Match 支持 * 通配，同一个模型可以有多条规则，
// 按配置顺序依次尝试。
type Model struct {
	Match    string `yaml:"match"`
	Channel  string `yaml:"channel"`
	Upstream string `yaml:"upstream"`
	Dialect  string `yaml:"dialect"`
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
		}
		routes = append(routes, Route{
			Channel:  channel.Name,
			Upstream: upstream,
			Dialect:  target,
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
		if channel.MaxRetries != nil && *channel.MaxRetries < 0 {
			problems = append(problems, where+": max_retries 不能为负")
		}
		problems = append(problems, checkAuth(where+".auth", channel.Auth, channel.Name)...)
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

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("config: 配置有 %d 处问题:\n  - %s", len(problems), strings.Join(problems, "\n  - "))
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
	if style != authStyleNone && auth.KeyEnv == "" {
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
