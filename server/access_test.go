package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/server"
)

// logBuffer 收集日志输出，供断言读取。写日志与读断言可能不在同一个 goroutine。
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newLoggingServer 装配一个把日志写进内存的服务。级别开到 debug，
// 这样断言只看得到访问日志与 WARN，不会混进别的级别。
func newLoggingServer(t *testing.T, yaml string) (*httptest.Server, *logBuffer) {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	handler, err := server.FromConfig(cfg, logger)
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	t.Cleanup(front.Close)
	return front, logs
}

// parseLines 把日志缓冲解析成一行一条的记录。
func parseLines(t *testing.T, text string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(text), "\n") {
		if raw == "" {
			continue
		}
		line := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("日志不是 JSON 行: %v / %s", err, raw)
		}
		lines = append(lines, line)
	}
	return lines
}

// waitForLog 等到出现 want 条 msg 日志，返回此前的全部日志行。
//
// 处理器返回与访问日志写出在服务端是同步的，但客户端读到响应结束可能早一步，
// 因此这里短暂轮询而不是读一次。
func waitForLog(t *testing.T, logs *logBuffer, msg string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		lines := parseLines(t, logs.String())
		found := 0
		for _, line := range lines {
			if line["msg"] == msg {
				found++
			}
		}
		if found >= want {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("等了 3 秒仍没有等到 %d 条 %q 日志，现有日志:\n%s", want, msg, logs.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// lineWith 取出某个字段等于 want 的那条日志。
func lineWith(t *testing.T, lines []map[string]any, key string, want any) map[string]any {
	t.Helper()
	for _, line := range lines {
		if line[key] == want {
			return line
		}
	}
	t.Fatalf("没有找到 %s 等于 %v 的日志行: %v", key, want, lines)
	return nil
}

// onlyLine 取出唯一的一条 msg 日志，多出或缺少都算失败。
func onlyLine(t *testing.T, lines []map[string]any, msg string) map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range lines {
		if line["msg"] == msg {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%q 日志应当只有 1 条，实际 %d 条: %v", msg, len(found), lines)
	}
	return found[0]
}

// completedLines 只留完成行。
//
// 进入行与完成行的 path、method、client 完全相同，按这些字段取行之前必须先滤掉
// 进入行，否则会取到没有 status 的进入行。
func completedLines(t *testing.T, lines []map[string]any) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range lines {
		if line["msg"] == "请求完成" {
			found = append(found, line)
		}
	}
	return found
}

func stringField(t *testing.T, line map[string]any, key string) string {
	t.Helper()
	value, ok := line[key].(string)
	if !ok {
		t.Fatalf("字段 %s 不是字符串: %v", key, line)
	}
	return value
}

func numberField(t *testing.T, line map[string]any, key string) float64 {
	t.Helper()
	value, ok := line[key].(float64)
	if !ok {
		t.Fatalf("字段 %s 不是数字: %v", key, line)
	}
	return value
}

func assertField(t *testing.T, line map[string]any, key, want string) {
	t.Helper()
	if got := stringField(t, line, key); got != want {
		t.Fatalf("字段 %s 不符: 期望 %q，实际 %q", key, want, got)
	}
}

// TestAccessLogForSameDialectPassthrough 覆盖 issue 的核心抱怨：客户端与渠道都是
// OpenAI 时请求按字节透传，用量不会被解析，这条路径原先在任何级别下都没有日志。
func TestAccessLogForSameDialectPassthrough(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, logs := newLoggingServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	line := onlyLine(t, waitForLog(t, logs, "请求完成", 1), "请求完成")
	assertField(t, line, "level", "INFO")
	assertField(t, line, "method", "POST")
	assertField(t, line, "path", "/v1/chat/completions")
	assertField(t, line, "model", "client-model")
	assertField(t, line, "channel", "upstream")
	assertField(t, line, "client", "127.0.0.1")
	if got := numberField(t, line, "status"); got != http.StatusOK {
		t.Fatalf("状态码字段不符: %v", got)
	}
	if got := numberField(t, line, "attempts"); got != 1 {
		t.Fatalf("尝试条数不符: %v", got)
	}
	if stringField(t, line, "request_id") == "" {
		t.Fatalf("request_id 不能为空: %v", line)
	}
	if _, ok := line["duration"]; !ok {
		t.Fatalf("缺少 duration 字段: %v", line)
	}
	if _, ok := line["prompt_tokens"]; ok {
		t.Fatalf("同方言透传不解析用量，不应出现 token 字段: %v", line)
	}
}

// TestAccessLogForLocalEndpoints 覆盖不经过转发路径的请求：健康检查、模型列表与
// 未知路径原先一条日志都不留。
func TestAccessLogForLocalEndpoints(t *testing.T) {
	front, logs := newLoggingServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
`)
	for _, path := range []string{"/healthz", "/v1/models"} {
		response, err := get(t, front.URL+path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		_ = response.Body.Close()
	}
	response, err := get(t, front.URL+"/no-such-path")
	if err != nil {
		t.Fatalf("请求未知路径失败: %v", err)
	}
	_ = response.Body.Close()

	lines := completedLines(t, waitForLog(t, logs, "请求完成", 3))
	health := lineWith(t, lines, "path", "/healthz")
	assertField(t, health, "level", "INFO")
	assertField(t, health, "method", "GET")
	if got := numberField(t, health, "status"); got != http.StatusOK {
		t.Fatalf("健康检查状态码不符: %v", got)
	}
	if got := numberField(t, health, "attempts"); got != 0 {
		t.Fatalf("健康检查没有尝试过候选，attempts 应当是 0: %v", got)
	}
	assertField(t, lineWith(t, lines, "path", "/v1/models"), "channel", "")
	unknown := lineWith(t, lines, "path", "/no-such-path")
	assertField(t, unknown, "level", "WARN")
	if got := numberField(t, unknown, "status"); got != http.StatusNotFound {
		t.Fatalf("未知路径状态码不符: %v", got)
	}
}

// TestAccessLogForRequestArrival 覆盖请求进入的那一行。
//
// 进入行只带进入时确实知道的字段：model 与 channel 要等路由，status、duration 与
// attempts 要等响应，这些留在完成行里。两行靠 request_id 串起来。
func TestAccessLogForRequestArrival(t *testing.T) {
	front, logs := newLoggingServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
`)
	response, err := get(t, front.URL+"/healthz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = response.Body.Close()

	lines := waitForLog(t, logs, "请求完成", 1)
	arrived := onlyLine(t, lines, "请求进入")
	assertField(t, arrived, "level", "INFO")
	assertField(t, arrived, "method", "GET")
	assertField(t, arrived, "path", "/healthz")
	assertField(t, arrived, "client", "127.0.0.1")
	for _, absent := range []string{"status", "duration", "model", "channel", "attempts"} {
		if _, ok := arrived[absent]; ok {
			t.Fatalf("进入行不该带 %s 字段，它要等路由或响应才知道: %v", absent, arrived)
		}
	}
	done := onlyLine(t, lines, "请求完成")
	if stringField(t, arrived, "request_id") != stringField(t, done, "request_id") {
		t.Fatalf("进入行与完成行的 request_id 不同: %v / %v", arrived, done)
	}
}

// TestStreamingAccessLogKeepsFlusher 覆盖流式路径：请求完成后留一行，并且包装层
// 必须把 http.Flusher 透传出去。
//
// 假上游发出第一个分片后一直等到测试放开才发结束标记。包装层吃掉 Flush 时，
// 第一个分片不会离开服务端的写缓冲，下面的读取会超时，而不是让流式静默退化。
func TestStreamingAccessLogKeepsFlusher(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseNow := func() { once.Do(func() { close(release) }) }
	defer releaseNow()

	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: first\n\n"))
		writer.(http.Flusher).Flush()
		<-release
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		writer.(http.Flusher).Flush()
	})
	front, logs := newLoggingServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		front.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")

	first := make(chan string, 1)
	rest := make(chan string, 1)
	failures := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			failures <- err
			return
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			failures <- fmt.Errorf("状态码不符: %d", response.StatusCode)
			return
		}
		reader := bufio.NewReader(response.Body)
		line, err := reader.ReadString('\n')
		if err != nil {
			failures <- err
			return
		}
		first <- line
		tail, err := io.ReadAll(reader)
		if err != nil {
			failures <- err
			return
		}
		rest <- string(tail)
	}()

	select {
	case line := <-first:
		if !strings.Contains(line, "first") {
			t.Fatalf("第一个分片内容不符: %q", line)
		}
	case err := <-failures:
		t.Fatalf("读取第一个分片失败: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("第一个分片 3 秒内没有到达，包装层可能吃掉了 http.Flusher")
	}
	releaseNow()
	select {
	case tail := <-rest:
		if !strings.Contains(tail, "[DONE]") {
			t.Fatalf("结束标记没有到达: %q", tail)
		}
	case err := <-failures:
		t.Fatalf("读取结束标记失败: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("结束标记 3 秒内没有到达")
	}

	line := onlyLine(t, waitForLog(t, logs, "请求完成", 1), "请求完成")
	assertField(t, line, "level", "INFO")
	assertField(t, line, "path", "/v1/chat/completions")
	if got := numberField(t, line, "status"); got != http.StatusOK {
		t.Fatalf("流式请求状态码不符: %v", got)
	}
	if got := numberField(t, line, "attempts"); got != 1 {
		t.Fatalf("流式请求尝试条数不符: %v", got)
	}
}

// TestAccessLogSharesRequestIDWithWarnings 覆盖失败路径：既有的 WARN 行与完成行
// 必须带同一个 request_id，一次请求的几条日志才串得起来。
func TestAccessLogSharesRequestIDWithWarnings(t *testing.T) {
	front, logs := newLoggingServer(t, `
channels:
  - {name: dead, dialect: openai, base_url: "http://127.0.0.1:1"}
models:
  - {match: "client-model", channel: dead}
`)

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusBadGateway {
		t.Fatalf("候选全部失败应当 502，实际 %d", status)
	}
	lines := waitForLog(t, logs, "请求完成", 1)
	warn := lineWith(t, lines, "msg", "上游请求失败")
	assertField(t, warn, "level", "WARN")
	done := lineWith(t, lines, "msg", "请求完成")
	assertField(t, done, "level", "WARN")
	if got := numberField(t, done, "status"); got != http.StatusBadGateway {
		t.Fatalf("完成行状态码不符: %v", got)
	}
	if got := numberField(t, done, "attempts"); got != 1 {
		t.Fatalf("尝试条数不符: %v", got)
	}
	first := stringField(t, warn, "request_id")
	second := stringField(t, done, "request_id")
	if first == "" {
		t.Fatalf("WARN 行缺少 request_id: %v", warn)
	}
	if first != second {
		t.Fatalf("WARN 行与完成行的 request_id 不同: %q / %q", first, second)
	}
	if got := stringField(t, onlyLine(t, lines, "请求进入"), "request_id"); got != first {
		t.Fatalf("进入行与 WARN 行的 request_id 不同: %q / %q", got, first)
	}
}

// TestAccessLogReportsUsageFields 覆盖用量：拆成具体字段，不打整个结构体，
// 也不打指针。
func TestAccessLogReportsUsageFields(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, logs := newLoggingServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, body := post(t, front.URL+"/v1/messages",
		`{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	line := onlyLine(t, waitForLog(t, logs, "请求完成", 1), "请求完成")
	if got := numberField(t, line, "prompt_tokens"); got != 3 {
		t.Fatalf("prompt_tokens 不符: %v", got)
	}
	if got := numberField(t, line, "completion_tokens"); got != 2 {
		t.Fatalf("completion_tokens 不符: %v", got)
	}
	if got := numberField(t, line, "total_tokens"); got != 5 {
		t.Fatalf("total_tokens 不符: %v", got)
	}
	if _, ok := line["usage"]; ok {
		t.Fatalf("用量应当是拆开的字段，不是整个结构体: %v", line)
	}
	if text := logs.String(); strings.Contains(text, "PromptTokens") || strings.Contains(text, "&{") {
		t.Fatalf("日志里出现了用量结构体: %s", text)
	}
}

// TestAccessLogCountsAttempts 覆盖重试：降级换过渠道之后，完成行记下实际服务的
// 渠道与尝试过的候选条数。
func TestAccessLogCountsAttempts(t *testing.T) {
	_, brokenServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusServiceUnavailable, `{"error":"down"}`)
	})
	_, goodServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, logs := newLoggingServer(t, `
channels:
  - {name: broken, dialect: openai, base_url: "`+brokenServer.URL+`", max_retries: 0}
  - {name: good, dialect: openai, base_url: "`+goodServer.URL+`"}
models:
  - {match: "client-model", channel: broken}
  - {match: "client-model", channel: good}
`)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("回退后应当成功，实际 %d %s", status, body)
	}
	line := onlyLine(t, waitForLog(t, logs, "请求完成", 1), "请求完成")
	assertField(t, line, "channel", "good")
	if got := numberField(t, line, "attempts"); got != 2 {
		t.Fatalf("尝试条数不符: %v", got)
	}
}

// TestAccessLogReportsStreamError 覆盖流式失败路径：上游流中断时 WARN 行与完成行
// 同样带同一个 request_id。
func TestAccessLogReportsStreamError(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`data: {"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: {坏 JSON\n\n"))
		writer.(http.Flusher).Flush()
	})
	front, logs := newLoggingServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, body := post(t, front.URL+"/v1/messages",
		`{"model":"client-model","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	lines := waitForLog(t, logs, "请求完成", 1)
	interrupted := lineWith(t, lines, "msg", "上游流中断")
	done := lineWith(t, lines, "msg", "请求完成")
	if stringField(t, interrupted, "request_id") != stringField(t, done, "request_id") {
		t.Fatalf("中断行与完成行的 request_id 不同: %v / %v", interrupted, done)
	}
}
