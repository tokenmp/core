package config_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
	"gopkg.in/yaml.v3"
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

func TestResolveUpstreamPlaceholder(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - {name: deepseek, dialect: openai, base_url: "https://x"}
  - {name: proxy, dialect: claude, base_url: "https://y"}
models:
  - {match: "deepseek-*", channel: deepseek, upstream: "deepseek/${model}"}
  - {match: "openai/*", channel: proxy, upstream: "proxy/${model}"}
  - {match: "literal", channel: deepseek, upstream: "a$$b"}
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if routes := cfg.Resolve("deepseek-v3"); len(routes) != 1 || routes[0].Upstream != "deepseek/deepseek-v3" {
		t.Fatalf("${model} 应替换成模型名: %+v", routes)
	}
	// 带 / 的模型名要整体替换，不能被当成路径分段。
	if routes := cfg.Resolve("openai/gpt-5"); len(routes) != 1 || routes[0].Upstream != "proxy/openai/gpt-5" {
		t.Fatalf("带 / 的模型名应整体替换: %+v", routes)
	}
	// $ 不紧跟 { 时是普通字符，两个 $ 就是两个美元符号。
	if routes := cfg.Resolve("literal"); len(routes) != 1 || routes[0].Upstream != "a$$b" {
		t.Fatalf("字面 $$ 应原样保留: %+v", routes)
	}
}

func TestValidateUpstreamPlaceholders(t *testing.T) {
	const channel = `
channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`
	ok := []struct {
		name string
		rule string
	}{
		{"${model} 合法", `{match: "m", channel: a, upstream: "pre/${model}"}`},
		{"字面 $$ 是普通字符", `{match: "m", channel: a, upstream: "a$$b"}`},
		{"单个 } 是普通字符", `{match: "m", channel: a, upstream: "a}b"}`},
	}
	for _, item := range ok {
		t.Run(item.name, func(t *testing.T) {
			if _, err := config.Parse(strings.NewReader(channel + "models:\n  - " + item.rule + "\n")); err != nil {
				t.Fatalf("不应报错: %v", err)
			}
		})
	}

	bad := []struct {
		name     string
		rule     string
		keywords []string
	}{
		{
			name:     "未知占位符",
			rule:     `{match: "m", channel: a, upstream: "pre/${unknown}"}`,
			keywords: []string{"models[1]", "不是支持的占位符"},
		},
		{
			name:     "未闭合的 ${",
			rule:     `{match: "m", channel: a, upstream: "pre/${model"}`,
			keywords: []string{"models[1]", "没有配对的"},
		},
	}
	for _, item := range bad {
		t.Run(item.name, func(t *testing.T) {
			// 前面放一条合法规则，用来确认报错指出的下标是出问题的那条。
			yaml := channel + "models:\n  - {match: \"ok\", channel: a}\n  - " + item.rule + "\n"
			_, err := config.Parse(strings.NewReader(yaml))
			if err == nil {
				t.Fatal("应当报错")
			}
			for _, keyword := range item.keywords {
				if !strings.Contains(err.Error(), keyword) {
					t.Fatalf("错误信息应包含 %q，实际: %v", keyword, err)
				}
			}
		})
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

func TestValidatePassHeaders(t *testing.T) {
	// 名单里只放行显式列出的名字，因此禁止项必须在校验期就报错，而不是静默丢掉。
	configWith := func(entries string) string {
		return `
channels:
  - {name: a, dialect: openai, base_url: "https://x", pass_headers: ` + entries + `}
`
	}
	cases := []struct {
		name    string
		entries string
		keyword string
	}{
		{name: "合法名单", entries: "[anthropic-beta, x-stainless-lang, x-app]"},
		{name: "空项", entries: `[anthropic-beta, ""]`, keyword: "不能为空"},
		{name: "含冒号", entries: `[anthropic-beta, "x:a"]`, keyword: "冒号"},
		{name: "含空白", entries: `[anthropic-beta, "x a"]`, keyword: "空白"},
		{name: "禁止 authorization", entries: "[authorization]", keyword: "不可放行"},
		{name: "禁止的大小写变体", entries: "[X-API-KEY]", keyword: "不可放行"},
		{name: "禁止 cookie", entries: "[Cookie]", keyword: "不可放行"},
		{name: "禁止 anthropic-workspace-id", entries: "[anthropic-workspace-id]", keyword: "不可放行"},
		{name: "禁止 content-type", entries: "[Content-Type]", keyword: "不可放行"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			_, err := config.Parse(strings.NewReader(configWith(item.entries)))
			if item.keyword == "" {
				if err != nil {
					t.Fatalf("应当通过: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), item.keyword) {
				t.Fatalf("错误信息应包含 %q，实际: %v", item.keyword, err)
			}
			if !strings.Contains(err.Error(), "pass_headers[") {
				t.Fatalf("错误信息应指到具体那一项，实际: %v", err)
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

// TestChannelBackoffParses 覆盖 issue #27 新增的退避键：三个时长能读出来，
// 写负值在加载时报错并指出 backoff。
func TestChannelBackoffParses(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - name: a
    dialect: openai
    base_url: "https://x"
    backoff: {base: 100ms, max: 3s, max_retry_after: 30s}
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	item, _ := cfg.ChannelByName("a")
	if item.Backoff.Base.Duration() != 100*time.Millisecond ||
		item.Backoff.Max.Duration() != 3*time.Second ||
		item.Backoff.MaxRetryAfter.Duration() != 30*time.Second {
		t.Fatalf("退避参数不符: %+v", item.Backoff)
	}

	_, err = config.Parse(strings.NewReader(`
channels:
  - {name: a, dialect: openai, base_url: "https://x", backoff: {base: -1s}}
`))
	if err == nil || !strings.Contains(err.Error(), "backoff") {
		t.Fatalf("负退避时长应当报错并指出 backoff，实际 %v", err)
	}
}

// TestValidateRetryStatuses 覆盖 issue #28 的两组状态码集合：默认值、渠道级覆盖、
// 解析范围与包含关系。报错要指出是哪个键的哪一项。
func TestValidateRetryStatuses(t *testing.T) {
	valid := []struct {
		name string
		yaml string
	}{
		{
			"不写两组键用默认值",
			`channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`,
		},
		{
			"全局两组在范围内",
			`retry: {in_channel: "408,429", failover: "408,429,500-599"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`,
		},
		{
			"渠道覆盖包含全局渠道内集合",
			`retry: {in_channel: "408,418"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x", failover_statuses: "408,418,429,500-599"}
`,
		},
	}
	for _, item := range valid {
		t.Run(item.name, func(t *testing.T) {
			if _, err := config.Parse(strings.NewReader(item.yaml)); err != nil {
				t.Fatalf("不应报错: %v", err)
			}
		})
	}

	invalid := []struct {
		name     string
		yaml     string
		keywords []string
	}{
		{
			"渠道内集合写了非数字",
			`retry: {in_channel: "abc"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`,
			[]string{"retry.in_channel", "第 1 项"},
		},
		{
			"状态码超出范围",
			`retry: {failover: "600"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`,
			[]string{"retry.failover", "超出 100 到 599"},
		},
		{
			"区间起点大于终点",
			`retry: {failover: "500-400"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`,
			[]string{"retry.failover", "起点大于终点"},
		},
		{
			"全局换候选不是渠道内重试的超集",
			`retry: {in_channel: "418", failover: "500-599"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`,
			[]string{"retry.failover", "418"},
		},
		{
			"渠道覆盖不是渠道内重试的超集",
			`retry: {in_channel: "418"}
` +
				`channels:
  - {name: a, dialect: openai, base_url: "https://x", failover_statuses: "408,429,500-599"}
`,
			[]string{"channels[0].failover_statuses", "418"},
		},
	}
	for _, item := range invalid {
		t.Run(item.name, func(t *testing.T) {
			_, err := config.Parse(strings.NewReader(item.yaml))
			if err == nil {
				t.Fatal("应当报错")
			}
			for _, keyword := range item.keywords {
				if !strings.Contains(err.Error(), keyword) {
					t.Fatalf("错误信息应包含 %q，实际: %v", keyword, err)
				}
			}
		})
	}
}

// TestRetryStatusesDefaults 覆盖默认集合：不写两组键时读出来的是内置值，
// 渠道没覆盖时沿用全局 failover。
func TestRetryStatusesDefaults(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - {name: a, dialect: openai, base_url: "https://x"}
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	inChannel, err := cfg.RetryStatuses()
	if err != nil {
		t.Fatalf("读渠道内集合失败: %v", err)
	}
	if want := []int{408, 429, 500, 502, 503, 504}; !equalInts(inChannel, want) {
		t.Fatalf("默认渠道内集合不符: %v，期望 %v", inChannel, want)
	}
	channel, _ := cfg.ChannelByName("a")
	failover, err := cfg.FailoverStatusesFor(channel)
	if err != nil {
		t.Fatalf("读换候选集合失败: %v", err)
	}
	if failover[0] != 401 || failover[len(failover)-1] != 599 {
		t.Fatalf("默认换候选集合不符: %v", failover)
	}
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// pooled 是一条渠道引用两个账号的最小配置，展开与头合并的用例都用它。
// team-a 不带头，team-b 带一个与渠道同名的头，用来断言账号覆盖渠道。
const pooled = `
accounts:
  - {name: team-a, key_env: KEY_A}
  - name: team-b
    key_env: KEY_B
    headers: {x-shared: team-b, x-account: team-b}

channels:
  - name: anthropic-pool
    dialect: claude
    base_url: https://api.anthropic.com
    headers: {x-shared: channel, x-channel-only: keep}
    auth: {style: x-api-key, accounts: [team-a, team-b]}

models:
  - {match: "claude-*", channel: anthropic-pool, affinity: team, upstream: "pool/${model}"}
`

func TestAccountReferencesExpandIntoChannelsAndRoutes(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(pooled))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if _, ok := cfg.ChannelByName("anthropic-pool"); ok {
		t.Fatal("基础渠道名不应留在展开结果里")
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("两个账号应展开成两条渠道，实际 %d 条", len(cfg.Channels))
	}
	for i, want := range []string{"anthropic-pool/team-a", "anthropic-pool/team-b"} {
		if cfg.Channels[i].Name != want {
			t.Fatalf("第 %d 条渠道名应为 %s，实际 %s", i, want, cfg.Channels[i].Name)
		}
	}
	first := cfg.Channels[0]
	if first.Auth.KeyEnv != "KEY_A" || len(first.Auth.Accounts) != 0 {
		t.Fatalf("展开后只应留下环境变量名，账号引用应清空: %+v", first.Auth)
	}
	if cfg.Channels[1].Auth.KeyEnv != "KEY_B" {
		t.Fatalf("第二个账号的环境变量名不符: %+v", cfg.Channels[1].Auth)
	}
	if len(cfg.Accounts) != 0 {
		t.Fatalf("展开结果里不应留顶层账号: %+v", cfg.Accounts)
	}

	routes := cfg.Resolve("claude-3-5")
	if len(routes) != 2 {
		t.Fatalf("规则应展开成两条候选，实际 %+v", routes)
	}
	for i, want := range []string{"anthropic-pool/team-a", "anthropic-pool/team-b"} {
		if routes[i].Channel != want {
			t.Fatalf("第 %d 条候选应为 %s，实际 %+v", i, want, routes[i])
		}
		if routes[i].Affinity != "team" || routes[i].Upstream != "pool/claude-3-5" {
			t.Fatalf("第 %d 条候选的其余字段应继承规则: %+v", i, routes[i])
		}
	}
}

func TestAccountHeadersMergeOverChannel(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(pooled))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// team-a 没有头，继承渠道的全部头。
	without := cfg.Channels[0].Headers
	if without["x-shared"] != "channel" || without["x-channel-only"] != "keep" {
		t.Fatalf("账号没写头时渠道头应原样保留: %+v", without)
	}
	if _, ok := without["x-account"]; ok {
		t.Fatalf("账号的头不应串到另一个账号上: %+v", without)
	}
	// team-b 覆盖同名头，并且不影响其余渠道头。
	merged := cfg.Channels[1].Headers
	if merged["x-shared"] != "team-b" {
		t.Fatalf("同名的头应取账号的: %+v", merged)
	}
	if merged["x-account"] != "team-b" || merged["x-channel-only"] != "keep" {
		t.Fatalf("合并应保留两边独有的头: %+v", merged)
	}
}

func TestExpandedChannelsDoNotShareHeaderMaps(t *testing.T) {
	// 缺陷回归：两边都没有账号头时，展开出的渠道不能直接共用渠道那份 map，
	// 否则改一条会串到兄弟上。
	cfg, err := config.Parse(strings.NewReader(`
accounts:
  - {name: team-a, key_env: KEY_A}
  - {name: team-b, key_env: KEY_B}
channels:
  - name: pool
    dialect: openai
    base_url: "https://x"
    headers: {x-shared: channel}
    auth: {style: bearer, accounts: [team-a, team-b]}
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfg.Channels) != 2 {
		t.Fatalf("两个账号应展开成两条渠道: %+v", cfg.Channels)
	}
	cfg.Channels[0].Headers["x-added"] = "only-a"
	if _, ok := cfg.Channels[1].Headers["x-added"]; ok {
		t.Fatalf("改一条渠道的头不应串到兄弟渠道: %+v", cfg.Channels[1].Headers)
	}
}

func TestResolveWithoutAccountsIsUnchanged(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - {name: openrouter, dialect: openai, base_url: "https://x"}
  - {name: backup, dialect: openai, base_url: "https://y"}
models:
  - {match: "claude-*", channel: openrouter, affinity: team}
  - {match: "claude-*", channel: backup, affinity: team}
`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(cfg.Channels) != 2 || cfg.Channels[0].Name != "openrouter" || cfg.Channels[1].Name != "backup" {
		t.Fatalf("不写 accounts 时渠道应原样保留: %+v", cfg.Channels)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("不写 accounts 时规则应原样保留: %+v", cfg.Models)
	}
	routes := cfg.Resolve("claude-3-5")
	if len(routes) != 2 || routes[0].Channel != "openrouter" || routes[1].Channel != "backup" {
		t.Fatalf("不写 accounts 时候选与顺序应与手写一致: %+v", routes)
	}
	for _, route := range routes {
		if route.Affinity != "team" {
			t.Fatalf("候选应继承组名: %+v", route)
		}
	}
}

func TestValidateAccountsProblems(t *testing.T) {
	const channel = `channels:
  - {name: c0, dialect: openai, base_url: "https://x"}
`
	cases := []struct {
		name     string
		yaml     string
		keywords []string
	}{
		{
			name: "账号名为空",
			yaml: `accounts:
  - {name: team-a, key_env: KEY_A}
  - {name: "", key_env: KEY_B}
` + channel + `  - {name: c1, dialect: openai, base_url: "https://x", auth: {style: bearer, accounts: [team-a, ""]}}
`,
			keywords: []string{"accounts[1]", "name 不能为空"},
		},
		{
			name: "账号名重复",
			yaml: `accounts:
  - {name: team-a, key_env: KEY_A}
  - {name: team-a, key_env: KEY_B}
` + channel + `  - {name: c1, dialect: openai, base_url: "https://x", auth: {style: bearer, accounts: [team-a]}}
`,
			keywords: []string{"accounts[1]", "账号名 team-a 重复"},
		},
		{
			name: "账号缺少 key_env",
			yaml: `accounts:
  - {name: team-a, key_env: KEY_A}
  - {name: team-b}
` + channel + `  - {name: c1, dialect: openai, base_url: "https://x", auth: {style: bearer, accounts: [team-a, team-b]}}
`,
			keywords: []string{"accounts[1]", "key_env 不能为空"},
		},
		{
			name: "引用了不存在的账号",
			yaml: `accounts:
  - {name: team-a, key_env: KEY_A}
` + channel + `  - {name: c1, dialect: openai, base_url: "https://x", auth: {style: bearer, accounts: [team-a, ghost]}}
`,
			keywords: []string{"channels[1].auth.accounts", "不存在的账号 ghost"},
		},
		{
			name: "accounts 与 key_env 同时写",
			yaml: `accounts:
  - {name: team-a, key_env: KEY_A}
` + channel + `  - {name: c1, dialect: openai, base_url: "https://x", auth: {style: bearer, key_env: KEY_C, accounts: [team-a]}}
`,
			keywords: []string{"channels[1].auth.accounts", "互斥"},
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			_, err := config.Parse(strings.NewReader(item.yaml))
			if err == nil {
				t.Fatal("应当报错")
			}
			for _, keyword := range item.keywords {
				if !strings.Contains(err.Error(), keyword) {
					t.Fatalf("错误信息应包含 %q，实际: %v", keyword, err)
				}
			}
		})
	}
}

// TestUnreferencedAccountIsAllowed 是 issue #38 的回归用例：账号与引用它的渠道写在
// 同一份文件里，「先建账号、再挂到渠道上」必然经过账号还没人引用的中间状态，
// 那个状态必须能通过校验，否则一次只改一处的工具全部卡住。
func TestUnreferencedAccountIsAllowed(t *testing.T) {
	const yaml = `
accounts:
  - {name: team-a, key_env: KEY_A}
  - {name: team-b, key_env: KEY_B}
channels:
  - {name: c0, dialect: openai, base_url: "https://x"}
  - {name: c1, dialect: openai, base_url: "https://x", auth: {style: bearer, accounts: [team-a]}}
`
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("没人引用的账号不该让加载失败: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("展开之后再校验也不该失败: %v", err)
	}
	// 展开只产出被引用的那个账号，没被引用的不产生候选。
	routes := cfg.Resolve("anything")
	if len(routes) != 0 {
		t.Fatalf("这条配置没有模型规则，不该有候选: %+v", routes)
	}
	for _, channel := range cfg.Channels {
		if channel.Name == "c1/team-b" {
			t.Fatalf("没被引用的账号不该展开成渠道: %+v", cfg.Channels)
		}
	}
}

func TestExpandedNameCollisionFailsParse(t *testing.T) {
	// 展开名与手写渠道名撞车只有展开之后才看得出来，靠二次校验抓。
	_, err := config.Parse(strings.NewReader(`
accounts:
  - {name: team-a, key_env: KEY_A}
channels:
  - {name: c0, dialect: openai, base_url: "https://x", auth: {style: bearer, accounts: [team-a]}}
  - {name: c0/team-a, dialect: openai, base_url: "https://x"}
`))
	if err == nil {
		t.Fatal("展开名与手写渠道名撞车时应当报错")
	}
	if !strings.Contains(err.Error(), "重复") {
		t.Fatalf("错误信息应指出渠道名重复，实际: %v", err)
	}
}

func TestExpandedConfigReloads(t *testing.T) {
	// 缺陷回归：展开后若把账号引用留在渠道上，写回的平铺配置会被「与 key_env 互斥」
	// 拦下。展开干净的 Config 应当能序列化后原样加载。
	cfg, err := config.Parse(strings.NewReader(pooled))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	reloaded, err := config.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("展开结果应当能重新加载: %v", err)
	}
	before := cfg.Resolve("claude-3-5")
	after := reloaded.Resolve("claude-3-5")
	if len(before) != len(after) {
		t.Fatalf("重新加载后的候选条数不一致: %d 与 %d", len(before), len(after))
	}
	for i := range before {
		if before[i].Channel != after[i].Channel {
			t.Fatalf("第 %d 条候选不一致: %s 与 %s", i, before[i].Channel, after[i].Channel)
		}
	}
}

func TestExpandOnHandBuiltConfig(t *testing.T) {
	// 程序化构造的配置不经 Parse，Expand 是宿主唯一的入口。
	original := &config.Config{
		Accounts: []config.Account{
			{Name: "team-a", KeyEnv: "KEY_A"},
			{Name: "team-b", KeyEnv: "KEY_B", Headers: map[string]string{"x-account": "team-b"}},
		},
		Channels: []config.Channel{{
			Name:    "pool",
			Dialect: "claude",
			BaseURL: "https://api.anthropic.com",
			Auth:    config.Auth{Style: "x-api-key", Accounts: []string{"team-a", "team-b"}},
		}},
		Models: []config.Model{{Match: "claude-*", Channel: "pool"}},
	}

	flat := original.Expand()
	if len(flat.Accounts) != 0 {
		t.Fatalf("展开结果里不应留顶层账号: %+v", flat.Accounts)
	}
	if len(flat.Channels) != 2 {
		t.Fatalf("两个账号应展开成两条渠道，实际 %d 条", len(flat.Channels))
	}
	for i, want := range []struct {
		name   string
		keyEnv string
	}{
		{"pool/team-a", "KEY_A"},
		{"pool/team-b", "KEY_B"},
	} {
		channel := flat.Channels[i]
		if channel.Name != want.name || channel.Auth.KeyEnv != want.keyEnv {
			t.Fatalf("第 %d 条渠道不符: %+v", i, channel)
		}
		if len(channel.Auth.Accounts) != 0 {
			t.Fatalf("展开后渠道的账号引用应清空: %+v", channel.Auth)
		}
	}
	if flat.Channels[1].Headers["x-account"] != "team-b" {
		t.Fatalf("账号的头应并入展开出的渠道: %+v", flat.Channels[1].Headers)
	}
	if len(flat.Models) != 2 || flat.Models[0].Channel != "pool/team-a" || flat.Models[1].Channel != "pool/team-b" {
		t.Fatalf("模型规则应跟渠道一起展开: %+v", flat.Models)
	}

	// 原对象不改：账号保留，渠道仍是一条且挂着两个引用，规则也仍是一条。
	if len(original.Accounts) != 2 {
		t.Fatalf("原配置的账号不应被改动: %+v", original.Accounts)
	}
	if len(original.Channels) != 1 || original.Channels[0].Name != "pool" {
		t.Fatalf("原配置的渠道不应被改动: %+v", original.Channels)
	}
	if len(original.Channels[0].Auth.Accounts) != 2 || original.Channels[0].Auth.KeyEnv != "" {
		t.Fatalf("原配置的账号引用不应被改动: %+v", original.Channels[0].Auth)
	}
	if len(original.Models) != 1 {
		t.Fatalf("原配置的规则不应被改动: %+v", original.Models)
	}
}

func TestValidateIsIdempotentAfterExpansion(t *testing.T) {
	cfg, err := config.Load(filepath.Join("..", "examples", "config.yaml"))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	channels, models := len(cfg.Channels), len(cfg.Models)
	// Load 之后已是平铺形态，再校验与再展开都不应报错，也不应重复展开。
	if err := cfg.Validate(); err != nil {
		t.Fatalf("展开后的配置应当仍然合法: %v", err)
	}
	if again := cfg.Expand(); len(again.Channels) != channels || len(again.Models) != models {
		t.Fatalf("平铺形态不应再展开: %d 条渠道、%d 条规则", len(again.Channels), len(again.Models))
	}
	if len(cfg.Channels) != channels || len(cfg.Models) != models {
		t.Fatalf("重复校验不应改变展开结果: %d 条渠道、%d 条规则", len(cfg.Channels), len(cfg.Models))
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
	// 引用两个账号的渠道展开成两条候选，接在写死的那条后面。
	claude := cfg.Resolve("claude-3-5")
	want := []string{"openrouter", "openrouter-pool/team-a", "openrouter-pool/team-b"}
	if len(claude) != len(want) {
		t.Fatalf("claude-* 应有三条候选，实际 %+v", claude)
	}
	for i, name := range want {
		if claude[i].Channel != name {
			t.Fatalf("第 %d 条候选应为 %s，实际 %+v", i, name, claude[i])
		}
	}
	if claude[2].Upstream != "claude-3-5" {
		t.Fatalf("未写 upstream 应沿用模型名: %+v", claude[2])
	}
}
