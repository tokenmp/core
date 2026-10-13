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
	"sync/atomic"
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

func (c *outcomeCollector) add(_ context.Context, outcome server.Outcome) {
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

// newOutcomeServer 装配一个同时收日志与 Outcome 的服务，extra 用来注入 Options。
func newOutcomeServer(t *testing.T, yaml string, extra ...server.Option) (*httptest.Server, *outcomeCollector) {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	logger := slog.New(slog.NewJSONHandler(&logBuffer{}, nil))
	collector := &outcomeCollector{}
	opts := append([]server.Option{server.WithRequestDone(collector.add)}, extra...)
	handler, err := server.FromConfigWith(cfg, logger, opts...)
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	t.Cleanup(front.Close)
	return front, collector
}

// fixedStep 是固定步长时钟每次读前进的量。
const fixedStep = 5 * time.Millisecond

// withFixedStepClock 把内核的时间源换成每次读都前进固定步长的时钟。
//
// 它是给「两次读数的差算对了没有」这类断言用的：这个时钟一定能分辨出一次进程内往返，
// 断言不必再看本机的时钟粒度，因此在任何平台上都稳定。真实耗时由此不可测，
// 要测真实耗时就得用本机的时钟，而那正是做不到严格断言的地方。
//
// 用原子自增而不是普通计数：内核的读时刻点分布在请求自己的 goroutine 上，
// 将来并行跑用例时会撞上 -race。
func withFixedStepClock() server.Option {
	var ticks atomic.Int64
	base := time.Unix(0, 0)
	return func(options *server.Options) {
		options.Now = func() time.Time { return base.Add(time.Duration(ticks.Add(1)) * fixedStep) }
	}
}

// withFrozenClock 把内核的时间源固定成同一个时刻：它报出的每个时长都恰好是 0。
//
// 这是粗粒度时钟的极端形态，用它来钉住 0 的解释：耗时读成 0 只说明这段等待量不出来，
// 不说明请求失败或没写响应体。本机时钟分辨不出一次进程内往返时，得到的正是同一个值，
// 因此这条断言在任何平台上都成立、也不依赖本机时钟的粒度。
func withFrozenClock() server.Option {
	frozen := time.Unix(1700000000, 0)
	return func(options *server.Options) {
		options.Now = func() time.Time { return frozen }
	}
}

// TestOutcomeForBufferedRequest 逐项核对非流式请求交出的原始量。
//
// 用固定步长时钟：这条用例要断言首字节时刻确实被取到了（差落在步长上），
// 而这在本机时钟上做不到——一次进程内往返在粗粒度平台上会量成 0。
func TestOutcomeForBufferedRequest(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, withFixedStepClock())

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
	// 首字节时间要落在总时长之内，且两个时长都落在固定步长上：非流式响应一次写完，
	// 两次读之间只隔几个时刻点。这是内核确实取到了这两个时刻的直接证据。
	if outcome.FirstByte < 0 || outcome.FirstByte > outcome.Duration {
		t.Fatalf("首字节时间不合理: first=%v duration=%v", outcome.FirstByte, outcome.Duration)
	}
	if outcome.FirstByte%fixedStep != 0 || outcome.Duration%fixedStep != 0 {
		t.Fatalf("固定步长时钟下两个时长都应当是步长的整数倍: first=%v duration=%v",
			outcome.FirstByte, outcome.Duration)
	}
	// 写出字节数就是客户端收到的响应体长度。
	if outcome.BytesWritten != int64(len(payload)) {
		t.Fatalf("写出字节数不符: %d，客户端收到 %d", outcome.BytesWritten, len(payload))
	}
	if len(outcome.Attempts) != 1 {
		t.Fatalf("应当只有一次候选尝试: %+v", outcome.Attempts)
	}
	attempt := outcome.Attempts[0]
	if attempt.Channel != "upstream" || attempt.Status != http.StatusOK || attempt.Duration < 0 {
		t.Fatalf("候选尝试记录不符: %+v", attempt)
	}
	if attempt.Retries != 0 {
		t.Fatalf("没有配重试时 Retries 应当是 0: %+v", attempt)
	}
	if outcome.Usage == nil {
		t.Fatalf("跨方言转发解析得出用量，不该是 nil: %+v", outcome)
	}
	if outcome.Usage.PromptTokens != 3 || outcome.Usage.CompletionTokens != 2 || outcome.Usage.TotalTokens != 5 {
		t.Fatalf("用量不符: %+v", outcome.Usage)
	}
}

// TestOutcomeUsageForStreamingPassthrough 覆盖同方言流式透传：上游在流末给出用量时，
// 客户端收到的字节不变，宿主的 Outcome 与账目行也拿到 token 数。
//
// 用量本来就在上游分片里，只是以前宿主没读。这里同时钉住两件事：旁路扫描不改变报文，
// 以及记账走的是与跨方言相同的 recordUsage。
func TestOutcomeUsageForStreamingPassthrough(t *testing.T) {
	chunks := []string{
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"你"}}]}`,
		`{"id":"1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":226,"total_tokens":268}}`,
	}
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = writer.Write([]byte("data: " + chunk + "\n\n"))
		}
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	})
	front, logs, outcomes := newSeamServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	// 旁路扫描只读不写，报文必须逐字节保留。
	expected := "data: " + chunks[0] + "\n\ndata: " + chunks[1] + "\n\ndata: [DONE]\n\n"
	if string(body) != expected {
		t.Fatalf("流式透传未被逐字节保留:\n期望 %q\n实际 %q", expected, body)
	}

	outcome := outcomes.wait(t, 1)[0]
	if outcome.Usage == nil {
		t.Fatalf("流末给出用量时 Outcome.Usage 不该是 nil: %+v", outcome)
	}
	if outcome.Usage.PromptTokens != 42 || outcome.Usage.CompletionTokens != 226 || outcome.Usage.TotalTokens != 268 {
		t.Fatalf("用量不符: %+v", outcome.Usage)
	}

	line := onlyLine(t, waitForLog(t, logs, "请求完成", 1), "请求完成")
	if got := numberField(t, line, "prompt_tokens"); got != 42 {
		t.Fatalf("账目行 prompt_tokens 不符: %v（完成行 %v）", got, line)
	}
	if got := numberField(t, line, "completion_tokens"); got != 226 {
		t.Fatalf("账目行 completion_tokens 不符: %v（完成行 %v）", got, line)
	}
	if got := numberField(t, line, "total_tokens"); got != 268 {
		t.Fatalf("账目行 total_tokens 不符: %v（完成行 %v）", got, line)
	}
}

// TestOutcomeWithoutUsageWhenUpstreamOmitsIt 覆盖上游流末不给用量：旁路扫描不到就
// 保持 Usage 为 nil，账目行也不出现 token 字段。
func TestOutcomeWithoutUsageWhenUpstreamOmitsIt(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你"}}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	})
	front, logs, outcomes := newSeamServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Usage != nil {
		t.Fatalf("上游没给用量时 Usage 应当是 nil: %+v", outcome.Usage)
	}
	line := onlyLine(t, waitForLog(t, logs, "请求完成", 1), "请求完成")
	if _, ok := line["prompt_tokens"]; ok {
		t.Fatalf("没有用量时账目行不该出现 token 字段: %v", line)
	}
}

// TestOutcomeUsageForResponsesStreamingPassthrough 覆盖 /v1/responses 这条同方言
// 透传：用量挂在 response.completed 分片的 response.usage 下。
//
// Responses 的原始用量用 input_tokens/output_tokens，而 recordUsage 读的是
// prompt_tokens/completion_tokens，这里只钉住取到的是同一份用量（total_tokens），
// 字段形状的差异不在本次改动范围内。
func TestOutcomeUsageForResponsesStreamingPassthrough(t *testing.T) {
	event := `{"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}}`
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: response.completed\ndata: " + event + "\n\n"))
	})
	front, _, outcomes := newSeamServer(t, `
channels:
  - {name: upstream, dialect: openai_responses, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, body := post(t, front.URL+"/v1/responses",
		`{"model":"client-model","stream":true,"input":"你好"}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Usage == nil {
		t.Fatalf("Responses 流给出用量时 Usage 不该是 nil: %+v", outcome)
	}
	if outcome.Usage.TotalTokens != 33 {
		t.Fatalf("Responses 用量不符: %+v", outcome.Usage)
	}
	// Responses 把提示词与补全报在 input_tokens 与 output_tokens 上，记账要把它
	// 与 Chat 那套 prompt_tokens 与 completion_tokens 合成一组数，否则 tps 会算成 0。
	if outcome.Usage.PromptTokens != 11 || outcome.Usage.CompletionTokens != 22 {
		t.Fatalf("Responses 的提示词与补全 token 数应当归一到 PromptTokens 与 CompletionTokens: %+v", outcome.Usage)
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
	// 首字节时间要早于总时长。这里的差是真实等待：上游发完第一个分片后停住不放，
	// 总时长因此比首字节时间长出一个可观的量，任何时钟粒度都分辨得出。非零这条
	// 不在这里断言：一次进程内往返在粗粒度平台上会量成 0，那条语义由固定步长
	// 时钟的两条用例钉住。
	if outcome.FirstByte < 0 || outcome.FirstByte >= outcome.Duration {
		t.Fatalf("首字节时间应当明显早于总时长: first=%v duration=%v",
			outcome.FirstByte, outcome.Duration)
	}
	if outcome.BytesWritten != int64(len(payload)) {
		t.Fatalf("写出字节数不符: %d，客户端收到 %d", outcome.BytesWritten, len(payload))
	}
}

// TestOutcomeFirstByteIsBodyNotHeader 覆盖首字节时间的测量点：上游先刷出响应头、过一会
// 儿才发第一个分片时，FirstByte 必须反映分片到达，而不是响应头写出。
//
// 记成响应头的话，这个值当 TTFT 交给宿主会系统性偏小。
func TestOutcomeFirstByteIsBodyNotHeader(t *testing.T) {
	const gap = 200 * time.Millisecond
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		// 响应头已经出去了，第一个分片要等一会儿才写。
		time.Sleep(gap)
		_, _ = writer.Write([]byte("data: first\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		writer.(http.Flusher).Flush()
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d", status)
	}

	outcome := outcomes.wait(t, 1)[0]
	if outcome.FirstByte < gap/2 {
		t.Fatalf("FirstByte 记成了响应头写出的时刻：%v，而响应头与第一个分片之间隔了 %v",
			outcome.FirstByte, gap)
	}
	if outcome.FirstByte > outcome.Duration {
		t.Fatalf("FirstByte 不该超过 Duration: first=%v duration=%v", outcome.FirstByte, outcome.Duration)
	}
}

// TestOutcomeCountsEveryAttempt 覆盖重试：降级换过渠道之后，每次尝试的耗时与状态
// 都在 Outcome 里，不只一个条数。
//
// 用固定步长时钟：两次尝试各是一段真实的进程内往返，真实时钟在粗粒度平台上会把
// 它们量成 0，「耗时应为正」这条断言在那里站不住。固定步长下这个值必然为正，
// 断言与平台无关。
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
`, withFixedStepClock())

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
		// 固定步长时钟下这个值必然是正的，且落在步长上；具体是几步由内核读时刻的
		// 次数决定，写死步数会让这条用例在无关改动下变红。
		if attempt.Duration <= 0 || attempt.Duration%fixedStep != 0 {
			t.Fatalf("第 %d 次尝试的耗时应为正且落在时钟步长上: %v", index+1, attempt.Duration)
		}
		if attempt.Retries != 0 {
			t.Fatalf("max_retries: 0 时第 %d 次尝试的 Retries 应当是 0: %+v", index+1, attempt)
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
	// 状态 0 分得出原因：连不上、转换失败与渠道不存在是三件事。
	if outcome.Attempts[0].Failure != server.FailureConnect {
		t.Fatalf("连接失败应当标成 %q，实际 %q", server.FailureConnect, outcome.Attempts[0].Failure)
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

// TestFromConfigWithSkipsNilOption 覆盖 nil 选项：宿主用变量拼选项时递进来 nil，
// 不该让装配炸掉。
func TestFromConfigWithSkipsNilOption(t *testing.T) {
	cfg, err := config.Parse(strings.NewReader(`
channels:
  - {name: upstream, dialect: openai, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: upstream}
`))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	if _, err := server.FromConfigWith(cfg, slog.New(slog.DiscardHandler), nil); err != nil {
		t.Fatalf("nil 选项应当被跳过: %v", err)
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

// TestOutcomeZeroDurationMeansUnmeasurable 把 0 的解释固定下来：内核的时间源分辨不出这段
// 等待时，Duration 与 FirstByte 是 0，而这次请求照样有结果——状态码与写出的字节数都在。
//
// 这条是 #44 那处语义混淆的固定点。粗粒度时钟平台上，0 既可能是「这段等待短于一个刻度」，
// 也可能是「整个请求没有写出过响应体」，光看两条时长区分不出；要分辨得看同一个
// server.Outcome 里的 BytesWritten——它由内核的写出路径记录，与时间源无关。
//
// 这条对照同时是这条用例自己的把关点：字节数为 0 的实现（把响应体吞掉、或压根没写出去）
// 会在这里红，而只断言「时长 0 + 状态 200」是拦不住它的：那正是这条用例期待的那组值，
// 那样的实现与真货同值，用例就成了共犯。
func TestOutcomeZeroDurationMeansUnmeasurable(t *testing.T) {
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "https://127.0.0.1:1"}
models:
  - {match: "client-model", channel: upstream}
`, withFrozenClock())

	status, _, payload := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusBadGateway {
		t.Fatalf("上游连不上时应当回 502: %d %s", status, payload)
	}

	outcome := outcomes.wait(t, 1)[0]
	if outcome.Duration != 0 || outcome.FirstByte != 0 {
		t.Fatalf("时钟不分时两个时长都应当是 0: duration=%v first=%v",
			outcome.Duration, outcome.FirstByte)
	}
	// 时长由时间源算，字节数由内核的写出路径记录，两笔数互不相干。字节数等于客户端收到的
	// 长度、且不为 0，说明这次确实写出去过响应体，那两个 0 只可能是量不出来。
	if outcome.BytesWritten != int64(len(payload)) || outcome.BytesWritten == 0 {
		t.Fatalf("错误响应也是响应体，照样写了出去: %d，客户端收到 %d",
			outcome.BytesWritten, len(payload))
	}
	// 尝试记录是第三笔数：一次尝试、没拿到上游响应、耗时读成 0。这里必须看清
	// 「没拿到响应」这一项，光看耗时与状态分不出真失败与「记成一次瞬时成功」。
	if len(outcome.Attempts) != 1 || outcome.Attempts[0].Status != 0 || outcome.Attempts[0].Duration != 0 {
		t.Fatalf("尝试记录应当只有一条、没拿到上游响应、耗时 0: %+v", outcome.Attempts)
	}
	if outcome.Attempts[0].Failure != server.FailureConnect {
		t.Fatalf("尝试的失败原因应当是连不上: %+v", outcome.Attempts[0])
	}
}

// TestOutcomeDurationsFollowInjectedClock 覆盖耗时字段的取值来源：内核报出的每个时长
// 都落在时间源的步长上。
//
// 这是「Duration 与 FirstByte 是按内核的时间源算的」那条口径的回归位：内核改用别的时间源
// （例如直接读 time.Now 绕开 Options.Now）时，这些值会落到步长之外，这条用例就会红。
// 与前一条一样，它不依赖本机时钟的分辨率。
func TestOutcomeDurationsFollowInjectedClock(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, withFixedStepClock())

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	outcome := outcomes.wait(t, 1)[0]
	if outcome.Duration%fixedStep != 0 || outcome.FirstByte%fixedStep != 0 {
		t.Fatalf("两个时长都应当落在时钟步长上: first=%v duration=%v",
			outcome.FirstByte, outcome.Duration)
	}
	if outcome.FirstByte < 0 || outcome.FirstByte > outcome.Duration {
		t.Fatalf("首字节时间不合理: first=%v duration=%v", outcome.FirstByte, outcome.Duration)
	}
	if len(outcome.Attempts) != 1 {
		t.Fatalf("应当只有一次候选尝试: %+v", outcome.Attempts)
	}
	// 尝试耗时是「选定候选」到「拿到响应头」的两次读，中间隔着一次进程内往返。
	// 它的具体值由内核读时刻的次数决定，只断言落在步长上，写死步数会让这条用例
	// 在无关改动下变红。
	if attempt := outcome.Attempts[0]; attempt.Duration < 0 || attempt.Duration%fixedStep != 0 {
		t.Fatalf("尝试耗时应当落在时钟步长上: %v", attempt.Duration)
	}
}
