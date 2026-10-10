package server_test

import (
	"context"
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

// outcomeCollector 收集回调交出的 Outcome。回调在服务端的请求 goroutine 上跑，
// 与读断言的 goroutine 不是同一个。
type outcomeCollector struct {
	mu       sync.Mutex
	outcomes []server.Outcome
}

func (c *outcomeCollector) add(outcome server.Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outcomes = append(c.outcomes, outcome)
}

// wait 等到收集到 want 条 Outcome。客户端读到响应结束可能早于服务端收尾，
// 因此这里短暂轮询而不是读一次。
func (c *outcomeCollector) wait(t *testing.T, want int) []server.Outcome {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c.mu.Lock()
		got := append([]server.Outcome(nil), c.outcomes...)
		c.mu.Unlock()
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("等了 3 秒仍只有 %d 条 Outcome，要 %d 条", len(got), want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// newOutcomeServer 装配一个同时收日志与 Outcome 的服务。
func newOutcomeServer(t *testing.T, yaml string) (*httptest.Server, *outcomeCollector) {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(&logBuffer{}, nil))
	collector := &outcomeCollector{}
	handler, err := server.FromConfig(cfg, logger, server.WithRequestDone(collector.add))
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	t.Cleanup(front.Close)
	return front, collector
}

// TestOutcomeForBufferedRequest 逐项核对非流式请求交出的原始量。
func TestOutcomeForBufferedRequest(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		front.URL+"/v1/messages",
		strings.NewReader(`{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "claude-cli/2.1.287 (external, cli)")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	payload, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}

	outcome := outcomes.wait(t, 1)[0]
	if outcome.RequestID == "" {
		t.Fatalf("RequestID 不能为空: %+v", outcome)
	}
	if outcome.Method != http.MethodPost || outcome.Path != "/v1/messages" {
		t.Fatalf("方法与路径不符: %+v", outcome)
	}
	if outcome.Client != "127.0.0.1" || outcome.UserAgent != "claude-cli/2.1.287 (external, cli)" {
		t.Fatalf("客户端标识不符: %+v", outcome)
	}
	if outcome.Status != http.StatusOK {
		t.Fatalf("状态码不符: %d", outcome.Status)
	}
	if outcome.Model != "client-model" || outcome.Channel != "upstream" {
		t.Fatalf("模型与渠道不符: %+v", outcome)
	}
	if outcome.Streaming {
		t.Fatalf("这个请求不是流式: %+v", outcome)
	}
	// 首字节时间要落在总时长之内，且非零。
	if outcome.FirstByte <= 0 || outcome.FirstByte > outcome.Duration {
		t.Fatalf("首字节时间不合理: first=%v duration=%v", outcome.FirstByte, outcome.Duration)
	}
	// 写出字节数就是客户端收到的响应体长度。
	if outcome.BytesWritten != int64(len(payload)) {
		t.Fatalf("写出字节数不符: %d，客户端收到 %d", outcome.BytesWritten, len(payload))
	}
	if len(outcome.Attempts) != 1 {
		t.Fatalf("应当只有一次候选尝试: %+v", outcome.Attempts)
	}
	attempt := outcome.Attempts[0]
	if attempt.Channel != "upstream" || attempt.Status != http.StatusOK || attempt.Duration <= 0 {
		t.Fatalf("候选尝试记录不符: %+v", attempt)
	}
	if outcome.Usage == nil {
		t.Fatalf("跨方言转发解析得出用量，不该是 nil: %+v", outcome)
	}
	if outcome.Usage.PromptTokens != 3 || outcome.Usage.CompletionTokens != 2 || outcome.Usage.TotalTokens != 5 {
		t.Fatalf("用量不符: %+v", outcome.Usage)
	}
}

// TestOutcomeWithoutUsageForPassthrough 覆盖同方言透传：不解析用量，Usage 为 nil，
// 宿主据此知道这次没有账目可记。
func TestOutcomeWithoutUsageForPassthrough(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d", status)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Usage != nil {
		t.Fatalf("同方言透传不解析用量，Usage 应当是 nil: %+v", outcome.Usage)
	}
}

// TestOutcomeForStreamingRequest 覆盖流式请求：首字节时间明显早于总时长，
// 这正是宿主算 TTFT 与 TPS 需要的两段。
func TestOutcomeForStreamingRequest(t *testing.T) {
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
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	received := make(chan string, 1)
	go func() {
		status, _, body := post(t, front.URL+"/v1/chat/completions",
			`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
		if status != http.StatusOK {
			received <- ""
			return
		}
		received <- string(body)
	}()

	// 上游把第一个分片发出来之后停在 release 上，总时长因此明显长于首字节时间。
	time.Sleep(150 * time.Millisecond)
	releaseNow()
	payload := <-received
	if payload == "" {
		t.Fatal("流式响应内容为空")
	}

	outcome := outcomes.wait(t, 1)[0]
	if !outcome.Streaming {
		t.Fatalf("这个请求是流式: %+v", outcome)
	}
	if outcome.FirstByte <= 0 || outcome.FirstByte >= outcome.Duration {
		t.Fatalf("首字节时间应当明显早于总时长: first=%v duration=%v",
			outcome.FirstByte, outcome.Duration)
	}
	if outcome.BytesWritten != int64(len(payload)) {
		t.Fatalf("写出字节数不符: %d，客户端收到 %d", outcome.BytesWritten, len(payload))
	}
}

// TestOutcomeCountsEveryAttempt 覆盖重试：降级换过渠道之后，每次尝试的耗时与状态
// 都在 Outcome 里，不只一个条数。
func TestOutcomeCountsEveryAttempt(t *testing.T) {
	_, brokenServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusServiceUnavailable, `{"error":"down"}`)
	})
	_, goodServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: broken, dialect: openai, base_url: "`+brokenServer.URL+`", max_retries: 0}
  - {name: good, dialect: openai, base_url: "`+goodServer.URL+`"}
models:
  - {match: "client-model", channel: broken}
  - {match: "client-model", channel: good}
`)

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("回退后应当成功: %d", status)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Channel != "good" {
		t.Fatalf("服务的渠道应当是 good: %s", outcome.Channel)
	}
	if len(outcome.Attempts) != 2 {
		t.Fatalf("应当记两次尝试: %+v", outcome.Attempts)
	}
	if outcome.Attempts[0].Channel != "broken" || outcome.Attempts[0].Status != http.StatusServiceUnavailable {
		t.Fatalf("第一次尝试不符: %+v", outcome.Attempts[0])
	}
	if outcome.Attempts[1].Channel != "good" || outcome.Attempts[1].Status != http.StatusOK {
		t.Fatalf("第二次尝试不符: %+v", outcome.Attempts[1])
	}
	for index, attempt := range outcome.Attempts {
		if attempt.Duration <= 0 {
			t.Fatalf("第 %d 次尝试的耗时应为正: %v", index+1, attempt.Duration)
		}
	}
}

// TestOutcomeWhenEveryCandidateFails 覆盖全部候选失败：状态码 502，渠道为空，
// 尝试列表里每个状态都是 0（连上游响应都没拿到）。
func TestOutcomeWhenEveryCandidateFails(t *testing.T) {
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: dead, dialect: openai, base_url: "http://127.0.0.1:1"}
models:
  - {match: "client-model", channel: dead}
`)

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusBadGateway {
		t.Fatalf("候选全部失败应当 502: %d", status)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Status != http.StatusBadGateway {
		t.Fatalf("状态码不符: %d", outcome.Status)
	}
	if outcome.Channel != "" {
		t.Fatalf("没有渠道服务成功时 Channel 应当为空: %q", outcome.Channel)
	}
	if len(outcome.Attempts) != 1 || outcome.Attempts[0].Status != 0 {
		t.Fatalf("连接失败时状态应当记 0: %+v", outcome.Attempts)
	}
}

// TestOutcomeForLocalEndpoint 覆盖不经过转发路径的请求：仍然是每条请求一次回调。
func TestOutcomeForLocalEndpoint(t *testing.T) {
	front, outcomes := newOutcomeServer(t, `
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

	outcome := outcomes.wait(t, 1)[0]
	if outcome.Path != "/healthz" || outcome.Status != http.StatusOK {
		t.Fatalf("健康检查的 Outcome 不符: %+v", outcome)
	}
	if len(outcome.Attempts) != 0 {
		t.Fatalf("没有经过转发路径，尝试列表应当为空: %+v", outcome.Attempts)
	}
	if outcome.Usage != nil {
		t.Fatalf("本地端点没有用量: %+v", outcome.Usage)
	}
}

// TestOutcomeCollectsOnePerRequest 确认一次请求一次回调，不重不漏。
func TestOutcomeCollectsOnePerRequest(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	const count = 5
	for index := 0; index < count; index++ {
		status, _, body := post(t, front.URL+"/v1/chat/completions",
			fmt.Sprintf(`{"model":"client-model","messages":[{"role":"user","content":"第 %d 次"}]}`, index))
		if status != http.StatusOK {
			t.Fatalf("第 %d 次请求失败: %d %s", index+1, status, body)
		}
	}
	got := outcomes.wait(t, count)
	if len(got) != count {
		t.Fatalf("回调次数不符: %d", len(got))
	}
	seen := make(map[string]bool, count)
	for _, outcome := range got {
		if seen[outcome.RequestID] {
			t.Fatalf("request_id 重复: %s", outcome.RequestID)
		}
		seen[outcome.RequestID] = true
	}
}
