package config_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
)

const sample = `
listen: 127.0.0.1:9999
log:
  format: text
  level: debug
channels:
  - name: openrouter
    dialect: openai
    base_url: https://openrouter.ai
    auth:
      style: bearer
      key_env: OPENROUTER_API_KEY
  - name: local
    dialect: claude
    base_url: http://127.0.0.1:8000
    timeout: 30s
models:
  - match: "gpt-*"
    channel: openrouter
    upstream: openai/gpt-5
  - match: "claude-*"
    channel: local
  - match: "gpt-4o"
    channel: local
    dialect: openai_responses
plugins:
  dir: plugins
  list:
    - key: echo
      enabled: true
      order: 10
      config:
        tag: local
    - key: off
      enabled: false
`

func TestParseAppliesDefaultsAndValidates(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Fatalf("listen 解析错误: %s", cfg.Listen)
	}
	local, ok := cfg.ChannelByName("local")
	if !ok {
		t.Fatal("找不到 local 渠道")
	}
	if local.Timeout.Duration() != 30*time.Second {
		t.Fatalf("timeout 解析错误: %s", local.Timeout)
	}
	openrouter, _ := cfg.ChannelByName("openrouter")
	if openrouter.Timeout.Duration() != config.DefaultTimeout {
		t.Fatalf("未写 timeout 应取默认值，实际 %s", openrouter.Timeout)
	}
	if openrouter.MaxRetries == nil || *openrouter.MaxRetries != 0 {
		t.Fatal("未写 max_retries 应默认为 0")
	}
}

func TestResolveKeepsConfigOrder(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	gpt := cfg.Resolve("gpt-4o")
	if len(gpt) != 2 {
		t.Fatalf("gpt-4o 应有两条候选，实际 %d", len(gpt))
	}
	if gpt[0].Channel != "openrouter" || gpt[0].Upstream != "openai/gpt-5" {
		t.Fatalf("第一条候选不符: %+v", gpt[0])
	}
	if gpt[1].Channel != "local" || gpt[1].Dialect != dialect.OpenAIResponses {
		t.Fatalf("第二条候选不符: %+v", gpt[1])
	}
	if gpt[1].Upstream != "gpt-4o" {
		t.Fatalf("未写 upstream 时应沿用模型名，实际 %s", gpt[1].Upstream)
	}
	if routes := cfg.Resolve("unknown-model"); len(routes) != 0 {
		t.Fatalf("无匹配时应返回空，实际 %+v", routes)
	}
}

func TestEnabledPluginsSortedByOrder(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	enabled := cfg.EnabledPlugins()
	if len(enabled) != 1 || enabled[0].Key != "echo" {
		t.Fatalf("启用列表不符: %+v", enabled)
	}
	if cfg.PluginConfig("echo")["tag"] != "local" {
		t.Fatalf("插件配置读取错误: %+v", cfg.PluginConfig("echo"))
	}
	if len(cfg.PluginConfig("missing")) != 0 {
		t.Fatal("未配置插件应返回空 map")
	}
}

func TestKnownFieldsRejectsTypos(t *testing.T) {
	_, err := config.Parse(strings.NewReader("listne: 127.0.0.1:1\n"))
	if err == nil {
		t.Fatal("拼错的字段应当报错")
	}
	if !strings.Contains(err.Error(), "listne") {
		t.Fatalf("错误信息应指出字段名: %v", err)
	}
}

func TestValidateCollectsProblems(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		keyword string
	}{
		{
			name: "渠道名重复",
			yaml: `
channels:
  - {name: a, dialect: openai, base_url: "https://x"}
  - {name: a, dialect: openai, base_url: "https://x"}
`,
			keyword: "重复",
		},
		{
			name: "方言未知",
			yaml: `
channels:
  - {name: a, dialect: anthropic, base_url: "https://x"}
`,
			keyword: "不是支持的方言",
		},
		{
			name: "鉴权缺少环境变量名",
			yaml: `
channels:
  - {name: a, dialect: openai, base_url: "https://x", auth: {style: bearer}}
`,
			keyword: "key_env",
		},
		{
			name: "模型引用不存在的渠道",
			yaml: `
channels:
  - {name: a, dialect: openai, base_url: "https://x"}
models:
  - {match: "gpt-*", channel: b}
`,
			keyword: "不存在的渠道",
		},
		{
			name: "base_url 不是 http",
			yaml: `
channels:
  - {name: a, dialect: openai, base_url: "ftp://x"}
`,
			keyword: "http://",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			_, err := config.Parse(strings.NewReader(item.yaml))
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), item.keyword) {
				t.Fatalf("错误信息应包含 %q，实际: %v", item.keyword, err)
			}
		})
	}
}

func TestMatchWildcards(t *testing.T) {
	cases := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"gpt-*", "gpt-4o", true},
		{"gpt-*", "openai/gpt-5", false},
		{"*", "any/model", true},
		{"openai/*", "openai/gpt-5", true},
		{"claude-?-5", "claude-4-5", true},
		{"claude-?-5", "claude-40-5", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{"a*b*c", "a-x-b-y-c", true},
		{"a*b*c", "a-x-c-y-b", false},
	}
	for _, item := range cases {
		if got := config.Match(item.pattern, item.name); got != item.want {
			t.Fatalf("Match(%q, %q) = %v，期望 %v", item.pattern, item.name, got, item.want)
		}
	}
	if _, err := config.CompileMatch("   "); err == nil {
		t.Fatal("空匹配式应当报错")
	}
}

func TestDurationFormats(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - {name: a, dialect: openai, base_url: "https://x", timeout: 45}
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	channel, _ := cfg.ChannelByName("a")
	if channel.Timeout.Duration() != 45*time.Second {
		t.Fatalf("纯数字应按秒解释，实际 %s", channel.Timeout)
	}
	if _, err := config.Parse(strings.NewReader(`
channels:
  - {name: a, dialect: openai, base_url: "https://x", timeout: 很快}
`)); err == nil {
		t.Fatal("无法解析的时长应当报错")
	}
}

func TestExampleConfigStaysValid(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "examples", "config.yaml"))
	if err != nil {
		t.Fatalf("示例配置应当可用: %v", err)
	}
	if cfg.Path == "" {
		t.Fatal("Load 应当填入配置文件路径")
	}
	if !filepath.IsAbs(cfg.Plugins.Dir) || !strings.HasSuffix(cfg.Plugins.Dir, filepath.Join("examples", "plugins")) {
		t.Fatalf("相对插件目录应当以配置文件所在目录为基准并归一成绝对路径，实际 %s", cfg.Plugins.Dir)
	}
	if routes := cfg.Resolve("gpt-4o"); len(routes) != 1 || routes[0].Channel != "openrouter" {
		t.Fatalf("示例配置的路由不符: %+v", routes)
	}
}
