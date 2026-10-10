package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/plugin"
	"github.com/tokenmp/core/server"
)

// upstream 是一个假上游，记录收到的请求并按预设脚本响应。
type upstream struct {
	mu       sync.Mutex
	requests []recordedRequest
	handler  func(writer http.ResponseWriter, request *http.Request)
}

// recordedRequest 是一次上游调用的留档。
type recordedRequest struct {
	Path string
	Body []byte
}

// record 记录一次调用。
func (u *upstream) record(request *http.Request) []byte {
	body, _ := io.ReadAll(request.Body)
	u.mu.Lock()
	u.requests = append(u.requests, recordedRequest{Path: request.URL.Path, Body: body})
	u.mu.Unlock()
	return body
}

// last 返回最近一次调用的留档。
func (u *upstream) last(t *testing.T) recordedRequest {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		t.Fatal("上游没有收到任何请求")
	}
	return u.requests[len(u.requests)-1]
}

// count 返回收到的请求数。
func (u *upstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.requests)
}

// newUpstream 启动假上游。
func newUpstream(t *testing.T, handler func(writer http.ResponseWriter, request *http.Request)) (*upstream, *httptest.Server) {
	t.Helper()
	record := &upstream{handler: handler}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		record.record(request)
		record.handler(writer, request)
	}))
	t.Cleanup(server.Close)
	return record, server
}

// writeJSON 写出一个 JSON 响应。
func writeJSON(writer http.ResponseWriter, status int, body string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(body))
}

// newServer 按 YAML 与插件目录装配服务。
func newServer(t *testing.T, yaml string, pluginDir string) *httptest.Server {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if pluginDir != "" {
		cfg.Plugins.Dir = pluginDir
	}
	handler, err := server.FromConfig(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	t.Cleanup(front.Close)
	return front
}

// get 发一次 GET 请求。
func get(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	return http.DefaultClient.Do(request)
}

// post 发一次请求并读回响应。
func post(t *testing.T, url, body string) (int, http.Header, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return response.StatusCode, response.Header, payload
}

const openAIChatResponse = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

func TestHealthAndModels(t *testing.T) {
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
`, "")
	response, err := get(t, front.URL+"/healthz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("健康检查状态码不符: %d", response.StatusCode)
	}

	response, err = get(t, front.URL+"/v1/models")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatalf("解析模型列表失败: %v", err)
	}
	if len(listing.Data) != 1 || listing.Data[0].ID != "client-model" {
		t.Fatalf("模型列表不符: %+v", listing)
	}
}

func TestPassthroughPreservesBytes(t *testing.T) {
	// 未建模字段与空白都要逐字节保留，因此这里刻意用非规范的 JSON 写法。
	requestBody := `{"model":"client-model",  "messages":[{"role":"user","content":"你好"}], "unknown_field":{"a":1}}`
	responseBody := `{"id":"x",  "choices":[], "unmodeled":true}`
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, responseBody)
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, "")

	status, _, body := post(t, front.URL+"/v1/chat/completions", requestBody)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if string(body) != responseBody {
		t.Fatalf("响应未被逐字节保留:\n期望 %s\n实际 %s", responseBody, body)
	}
	if got := record.last(t).Body; string(got) != requestBody {
		t.Fatalf("请求未被逐字节保留:\n期望 %s\n实际 %s", requestBody, got)
	}
}

func TestCrossDialectConversion(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, "")

	status, _, body := post(t, front.URL+"/v1/messages",
		`{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	var converted struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &converted); err != nil {
		t.Fatalf("响应不是 Claude 形状: %v / %s", err, body)
	}
	if converted.Type != "message" || len(converted.Content) == 0 || converted.Content[0].Text != "你好" {
		t.Fatalf("转换结果不符: %s", body)
	}
}

func TestUpstreamPathFollowsChannelDialect(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream, upstream: "upstream-model"}
`, "")

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
}

func TestGeminiPathRouting(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]}}]}`)
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: gemini, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "gemini-2.5-pro", channel: upstream}
`, "")

	status, _, body := post(t, front.URL+"/v1beta/models/gemini-2.5-pro:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"你好"}]}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if path := record.last(t).Path; path != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("上游路径不符: %s", path)
	}

	status, _, _ = post(t, front.URL+"/v1beta/models/gemini-2.5-pro:unknownAction", `{}`)
	if status != http.StatusNotFound {
		t.Fatalf("未知动作应当 404，实际 %d", status)
	}
}

func TestUnknownModelReturnsNotFound(t *testing.T) {
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "known", channel: upstream}
`, "")
	status, _, body := post(t, front.URL+"/v1/chat/completions", `{"model":"unknown","messages":[]}`)
	if status != http.StatusNotFound {
		t.Fatalf("应当 404，实际 %d %s", status, body)
	}
	if !bytes.Contains(body, []byte("model_not_found")) {
		t.Fatalf("错误码不符: %s", body)
	}
}

func TestFallbackToNextRoute(t *testing.T) {
	_, brokenServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusServiceUnavailable, `{"error":"down"}`)
	})
	good, goodServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front := newServer(t, `
channels:
  - {name: broken, dialect: openai, base_url: "`+brokenServer.URL+`", max_retries: 0}
  - {name: good, dialect: openai, base_url: "`+goodServer.URL+`"}
models:
  - {match: "client-model", channel: broken}
  - {match: "client-model", channel: good}
`, "")

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("回退后应当成功，实际 %d %s", status, body)
	}
	if good.count() != 1 {
		t.Fatalf("第二个渠道应当被调用一次，实际 %d", good.count())
	}
}

func TestUpstreamErrorIsPassedThrough(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, "")
	status, _, body := post(t, front.URL+"/v1/messages",
		`{"model":"client-model","max_tokens":8,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("状态码应当透传，实际 %d", status)
	}
	if !strings.Contains(string(body), "rate limited") {
		t.Fatalf("错误体应当透传: %s", body)
	}
}

const echoPlugin = `export const meta = { key: "echo", name: "回显", version: "0.1.0", apiVersion: "v1" };

export function onRequest(input) {
  input.body.messages.push({ role: "system", content: "tag=" + host.config.tag });
  return { body: input.body };
}

export function onResponse(input) {
  input.body.plugin = host.plugin.key;
  return { body: input.body };
}

export function onEvent(input) {
  if (input.data && input.data.choices) {
    input.data.choices[0].delta.content = input.data.choices[0].delta.content + "|插件";
  }
  return { data: input.data };
}

export function onStreamEnd(input) {
  return { events: input.events };
}
`

// writeEchoPlugin 在临时目录里放一个插件，返回插件目录。
func writeEchoPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "echo", plugin.PluginFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("创建插件目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(echoPlugin), 0o600); err != nil {
		t.Fatalf("写入插件失败: %v", err)
	}
	return dir
}

func TestRequestAndResponsePlugins(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	pluginDir := writeEchoPlugin(t)
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
plugins:
  dir: plugins
  list:
    - {key: echo, enabled: true, order: 1, config: {tag: local}}
`, pluginDir)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if !strings.Contains(string(record.last(t).Body), "tag=local") {
		t.Fatalf("请求钩子未生效: %s", record.last(t).Body)
	}
	if !strings.Contains(string(body), `"plugin":"echo"`) {
		t.Fatalf("响应钩子未生效: %s", body)
	}
}

func TestStreamingPassthrough(t *testing.T) {
	chunks := []string{
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"你"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"好"}}]}`,
	}
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = writer.Write([]byte("data: " + chunk + "\n\n"))
		}
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, "")

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	expected := "data: " + chunks[0] + "\n\ndata: " + chunks[1] + "\n\ndata: [DONE]\n\n"
	if string(body) != expected {
		t.Fatalf("流式透传未被逐字节保留:\n期望 %q\n实际 %q", expected, body)
	}
}

func TestStreamingConversion(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		events := []string{
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
			`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`,
		}
		for _, event := range events {
			_, _ = writer.Write([]byte("data: " + event + "\n\n"))
		}
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	})
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, "")

	status, header, body := post(t, front.URL+"/v1/messages",
		`{"model":"client-model","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if !strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("内容类型不符: %s", header.Get("Content-Type"))
	}
	text := string(body)
	if !strings.Contains(text, "event: message_start") {
		t.Fatalf("未产出 Claude 事件: %s", text)
	}
	if !strings.Contains(text, "content_block_delta") || !strings.Contains(text, "你") {
		t.Fatalf("未产出内容事件: %s", text)
	}
	if !strings.Contains(text, "message_stop") {
		t.Fatalf("未产出终止事件: %s", text)
	}
}

func TestEventPluginRewritesStreamChunk(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你"}}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	})
	pluginDir := writeEchoPlugin(t)
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
plugins:
  dir: plugins
  list:
    - {key: echo, enabled: true, config: {tag: local}}
`, pluginDir)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if !strings.Contains(string(body), "|插件") {
		t.Fatalf("事件钩子未生效: %s", body)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("结束标记应当保留: %s", body)
	}
}

func TestPluginFailureKeepsForwarding(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	dir := t.TempDir()
	path := filepath.Join(dir, "boom", plugin.PluginFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	source := `export const meta = { key: "boom", name: "爆炸", version: "1", apiVersion: "v1" };
export function onRequest() { throw new Error("故意失败"); }`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("写入插件失败: %v", err)
	}
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
plugins:
  dir: plugins
  list:
    - {key: boom, enabled: true}
`, dir)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("插件失败不应影响转发，实际 %d %s", status, body)
	}
}

func TestMissingEnabledPluginFailsAssembly(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
plugins:
  dir: plugins
  list:
    - {key: nope, enabled: true}
`))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	cfg.Plugins.Dir = t.TempDir()
	if _, err := server.FromConfig(cfg, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("启用但缺失的插件应当让装配失败")
	}
}

func TestRejectsInvalidRequestBody(t *testing.T) {
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
`, "")
	status, _, body := post(t, front.URL+"/v1/chat/completions", `{"model":`)
	if status != http.StatusBadRequest {
		t.Fatalf("应当 400，实际 %d %s", status, body)
	}
	status, _, _ = post(t, front.URL+"/v1/chat/completions", `{"messages":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("缺少 model 应当 400，实际 %d", status)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	front := newServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
`, "")
	response, err := get(t, front.URL+"/v1/chat/completions")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("应当 405，实际 %d", response.StatusCode)
	}
}
