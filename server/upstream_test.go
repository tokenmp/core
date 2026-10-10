package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
