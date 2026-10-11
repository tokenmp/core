package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/server"
)

// postWithHeaders 发一次 POST，extra 里的头一并带上。
func postWithHeaders(t *testing.T, url, body string, extra map[string]string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range extra {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return response.StatusCode, payload
}

// postWithHeaderValues 发一次 POST，同一个头名的多个值都会带上，用来覆盖多值头的转发。
func postWithHeaderValues(t *testing.T, url, body string, extra http.Header) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, values := range extra {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return response.StatusCode, payload
}

// passHeadersYAML 是一条渠道带客户端头放行名单的最小配置。
func passHeadersYAML(upstreamURL, entries string) string {
	return `
channels:
  - name: upstream
    dialect: openai
    base_url: "` + upstreamURL + `"
    pass_headers: ` + entries + `
models:
  - {match: "client-model", channel: upstream}
`
}

// withUpstreamHeaders 返回一段中间件，按顺序挂上给定的上游头。
func withUpstreamHeaders(headers [][2]string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			ctx := request.Context()
			for _, pair := range headers {
				ctx = server.WithUpstreamHeader(ctx, pair[0], pair[1])
			}
			next.ServeHTTP(writer, request.WithContext(ctx))
		})
	}
}

// TestUpstreamHeaderReachesUpstream 覆盖新入口的正路：宿主在中间件里设的头到了上游，
// 而客户端自带的 Authorization 没有被抄上去。
//
// 这两条要一起断言：只证明「宿主设的头到了」不能排除「内核把客户端请求头整份抄了」，
// 后者会把客户端的鉴权头一起带给上游。
func TestUpstreamHeaderReachesUpstream(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		withUpstreamHeaders([][2]string{
			{"X-Trace-ID", "trace-7"},
			{"X-Host-Note", "宿主加的"},
		})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{"Authorization": "Bearer client-secret"})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if value := got.Get("X-Trace-ID"); value != "trace-7" {
		t.Fatalf("宿主设的上游头没到: X-Trace-ID=%q", value)
	}
	if value := got.Get("X-Host-Note"); value != "宿主加的" {
		t.Fatalf("宿主设的上游头没到: X-Host-Note=%q", value)
	}
	if value := got.Get("Authorization"); value != "" {
		t.Fatalf("客户端自带的 Authorization 漏给了上游: %q", value)
	}
}

// priorityYAML 是带渠道级请求头的配置，用来钉住同名的优先级。
func priorityYAML(upstreamURL string) string {
	return `
channels:
  - name: upstream
    dialect: openai
    base_url: "` + upstreamURL + `"
    headers:
      X-Title: from-config
      X-Stays: keep
models:
  - {match: "client-model", channel: upstream}
`
}

// TestUpstreamHeaderOverridesChannelConfig 钉住同名头的优先级：宿主按请求设的值覆盖
// 渠道配置的值，且只发一条；宿主没碰的渠道头照旧上去。
func TestUpstreamHeaderOverridesChannelConfig(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, priorityYAML(upstreamServer.URL), server.WithMiddleware(
		withUpstreamHeaders([][2]string{{"X-Title", "from-host"}})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if values := got.Values("X-Title"); len(values) != 1 || values[0] != "from-host" {
		t.Fatalf("宿主按请求设的头应当覆盖渠道配置里的同名头: X-Title=%v", values)
	}
	if value := got.Get("X-Stays"); value != "keep" {
		t.Fatalf("渠道配置里已有的头被无声覆盖: X-Stays=%q", value)
	}
}

// TestUpstreamHeaderSameNameLastWins 钉住同一个名字设两次时的语义：后一次生效，
// 与 http.Header.Set 一致。
func TestUpstreamHeaderSameNameLastWins(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		withUpstreamHeaders([][2]string{{"X-Trace-ID", "first"}, {"x-trace-id", "second"}})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if values := got.Values("X-Trace-ID"); len(values) != 1 || values[0] != "second" {
		t.Fatalf("同名头重复设置应当后一次生效: X-Trace-ID=%v", values)
	}
}

// TestUpstreamHeaderInvalidDropped 覆盖非法输入：含换行的头被丢弃并记 WARN，
// 转发照常完成，不带坏头也不把整条请求带成 502。
func TestUpstreamHeaderInvalidDropped(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, logs, _ := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		withUpstreamHeaders([][2]string{
			{"X-Ok", "fine"},
			{"X-Evil", "a\r\nX-Injected: 1"},
			{"", "无名"},
		})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if value := got.Get("X-Ok"); value != "fine" {
		t.Fatalf("合法的头不该受影响: X-Ok=%q", value)
	}
	if value := got.Get("X-Injected"); value != "" {
		t.Fatalf("含换行的取值不该被拆成两个头: X-Injected=%q", value)
	}
	if !strings.Contains(logs.String(), "丢弃非法的上游请求头") {
		t.Fatalf("丢弃非法头应当记一行 WARN: %s", logs.String())
	}
}

// TestUpstreamHeaderCannotDisplaceChannelCredential 钉住例外：渠道的鉴权头由 Auth 最后
// 写入，宿主按请求设的同名头不生效，凭据始终来自渠道。
func TestUpstreamHeaderCannotDisplaceChannelCredential(t *testing.T) {
	t.Setenv("TOKENMP_TEST_KEY", "channel-secret")
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, `
channels:
  - name: upstream
    dialect: openai
    base_url: "`+upstreamServer.URL+`"
    auth: {style: bearer, key_env: TOKENMP_TEST_KEY}
models:
  - {match: "client-model", channel: upstream}
`, server.WithMiddleware(
		withUpstreamHeaders([][2]string{{"Authorization", "Bearer host-secret"}})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if value := got.Get("Authorization"); value != "Bearer channel-secret" {
		t.Fatalf("鉴权头应当由渠道凭据决定: Authorization=%q", value)
	}
}

// TestPassHeadersReachUpstream 覆盖放行名单的正路：名单里的头按请求到达上游，名单外的
// 照旧不上去。
func TestPassHeadersReachUpstream(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, passHeadersYAML(upstreamServer.URL,
		"[anthropic-beta, x-stainless-lang, x-app]"))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{
			"Anthropic-Beta":   "user-profiles-2025-01-01",
			"X-Stainless-Lang": "js",
			"X-App":            "cli",
			"X-Not-Listed":     "不该上去",
		})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	for name, want := range map[string]string{
		"Anthropic-Beta":   "user-profiles-2025-01-01",
		"X-Stainless-Lang": "js",
		"X-App":            "cli",
	} {
		if value := got.Get(name); value != want {
			t.Fatalf("名单里的头没到上游: %s=%q，期望 %q", name, value, want)
		}
	}
	if value := got.Get("X-Not-Listed"); value != "" {
		t.Fatalf("名单外的头不该上去: X-Not-Listed=%q", value)
	}
}

// TestPassHeadersForbiddenNotForwarded 钉住不可放行的头即使客户端带着也不上去。
// Content-Type 单独看一下：内核按渠道给请求体转换后的形状补值，不是没有这个头。
func TestPassHeadersForbiddenNotForwarded(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, passHeadersYAML(upstreamServer.URL, "[x-app]"))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{
			"Authorization":          "Bearer client-secret",
			"X-Api-Key":              "client-key",
			"Cookie":                 "session=abc",
			"Anthropic-Workspace-Id": "workspace-7",
			"Content-Type":           "text/plain",
			"X-App":                  "cli",
		})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	for _, name := range []string{"Authorization", "X-Api-Key", "Cookie", "Anthropic-Workspace-Id"} {
		if value := got.Get(name); value != "" {
			t.Fatalf("不可放行的头不该上去: %s=%q", name, value)
		}
	}
	if value := got.Get("Content-Type"); value != "application/json" {
		t.Fatalf("请求体已转换，Content-Type 应当由内核给: %q", value)
	}
	if value := got.Get("X-App"); value != "cli" {
		t.Fatalf("名单里的头没到上游: X-App=%q", value)
	}
}

// TestPassHeadersUnsetForwardsNothing 是不写 pass_headers 的对照组：行为与本功能
// 落地之前相同，客户端头一个都不上去，渠道头照旧。
func TestPassHeadersUnsetForwardsNothing(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, `
channels:
  - name: upstream
    dialect: openai
    base_url: "`+upstreamServer.URL+`"
    headers:
      X-Channel: from-config
models:
  - {match: "client-model", channel: upstream}
`)

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{"Anthropic-Beta": "beta-1", "X-Custom": "client"})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if value := got.Get("Anthropic-Beta"); value != "" {
		t.Fatalf("没写名单时客户端头不该上去: Anthropic-Beta=%q", value)
	}
	if value := got.Get("X-Custom"); value != "" {
		t.Fatalf("没写名单时客户端头不该上去: X-Custom=%q", value)
	}
	if value := got.Get("X-Channel"); value != "from-config" {
		t.Fatalf("渠道头不该受影响: X-Channel=%q", value)
	}
}

// TestPassHeaderHostWins 钉住优先级：名字同时在放行名单与宿主头里时，宿主按请求设的赢。
func TestPassHeaderHostWins(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, passHeadersYAML(upstreamServer.URL, "[x-trace-id]"),
		server.WithMiddleware(withUpstreamHeaders([][2]string{{"X-Trace-Id", "from-host"}})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{"X-Trace-Id": "from-client"})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if values := got.Values("X-Trace-Id"); len(values) != 1 || values[0] != "from-host" {
		t.Fatalf("宿主设的同名头应当赢: X-Trace-Id=%v", values)
	}
}

// TestPassHeaderChannelConfigWins 钉住另一层优先级：渠道 headers 比客户端转发的具体。
func TestPassHeaderChannelConfigWins(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, `
channels:
  - name: upstream
    dialect: openai
    base_url: "`+upstreamServer.URL+`"
    headers:
      X-Title: from-config
    pass_headers: [x-title]
models:
  - {match: "client-model", channel: upstream}
`)

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		map[string]string{"X-Title": "from-client"})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	if values := got.Values("X-Title"); len(values) != 1 || values[0] != "from-config" {
		t.Fatalf("渠道 headers 应当比客户端转发的具体: X-Title=%v", values)
	}
}

// TestPassHeaderMultipleValues 覆盖多值头：同一个头名两个值都原样带上。
func TestPassHeaderMultipleValues(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, _ := newSeamServer(t, passHeadersYAML(upstreamServer.URL, "[x-multi]"))

	status, body := postWithHeaderValues(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`,
		http.Header{"X-Multi": {"first", "second"}})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	got := record.last(t).Header
	values := got.Values("X-Multi")
	if len(values) != 2 || values[0] != "first" || values[1] != "second" {
		t.Fatalf("多值头应当原样带上: X-Multi=%v", values)
	}
}

// TestPassHeaderInvalidNameRejectedAtAssembly 钉住头名合法性的分层：config.Validate
// 只挡明显写错的，RFC 7230 token 的完整判断在装配时报错，服务启动失败。x(bad) 里的
// 括号不属于 token 字符集，但 config 不拦它。
func TestPassHeaderInvalidNameRejectedAtAssembly(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(passHeadersYAML("http://127.0.0.1:1", "[x(bad)]")))
	if err != nil {
		t.Fatalf("config 不该拦下这一项: %v", err)
	}
	if _, err := server.FromConfigWith(cfg, slog.New(slog.DiscardHandler)); err == nil ||
		!strings.Contains(err.Error(), "不是合法的 HTTP 头名") {
		t.Fatalf("装配应当报错: %v", err)
	}
}

// TestPassHeaderForbiddenRejectedAtAssembly 钉住禁止名单在装配期也拦得住：绕过
// config.Validate 的程序化配置同样不能把 authorization 放进放行名单。
func TestPassHeaderForbiddenRejectedAtAssembly(t *testing.T) {
	cfg := &config.Config{
		Channels: []config.Channel{{
			Name:        "upstream",
			Dialect:     "openai",
			BaseURL:     "http://127.0.0.1:1",
			PassHeaders: []string{"authorization"},
		}},
	}
	if _, err := server.FromConfigWith(cfg, slog.New(slog.DiscardHandler)); err == nil ||
		!strings.Contains(err.Error(), "不可放行") {
		t.Fatalf("装配应当拒绝禁止放行的头: %v", err)
	}
}

// TestUpstreamHeaderInvalidNameDropped 覆盖名字与取值的字符边界：名字里带空格这类
// 非法 token（以及取值里的控制字符）会让 Go 的传输层直接报错拒绝写请求，那条请求
// 就变成 502 了。这里要求它们与含换行的情形一样被丢掉、只记一行 WARN，转发照常完成。
func TestUpstreamHeaderInvalidNameDropped(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, logs, _ := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		withUpstreamHeaders([][2]string{
			{"X-Ok", "fine"},
			{"X-Bad Name", "v"},
			{"X-Ctrl", "a\x00b"},
		})))

	status, body := postWithHeaders(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`, nil)
	if status != http.StatusOK {
		t.Fatalf("写错一个头名不该把整条请求带成 502: %d %s", status, body)
	}

	got := record.last(t).Header
	if value := got.Get("X-Ok"); value != "fine" {
		t.Fatalf("合法的头不该受影响: X-Ok=%q", value)
	}
	if value := got.Get("X-Bad Name"); value != "" {
		t.Fatalf("非法的头名不该上去: %q", value)
	}
	if !strings.Contains(logs.String(), "丢弃非法的上游请求头") {
		t.Fatalf("丢弃非法头应当记一行 WARN: %s", logs.String())
	}
}
