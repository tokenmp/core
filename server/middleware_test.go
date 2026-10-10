package server_test

import (
	"context"
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

// seamYAML 是一条渠道加一条规则的最小配置。
func seamYAML(upstreamURL string) string {
	return `
channels:
  - {name: upstream, dialect: openai, base_url: "` + upstreamURL + `"}
models:
  - {match: "client-model", channel: upstream}
`
}

// newSeamServer 装配一个装了宿主中间件与回调的服务，同时收日志与 Outcome。
func newSeamServer(t *testing.T, yaml string, extra ...server.Option) (*httptest.Server, *logBuffer, *outcomeCollector) {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	collector := &outcomeCollector{}
	opts := append([]server.Option{server.WithRequestDone(collector.add)}, extra...)
	handler, err := server.FromConfigWith(cfg, logger, opts...)
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	t.Cleanup(front.Close)
	return front, logs, collector
}

// TestMiddlewareNestsOutsideAccessLog 覆盖中间件的嵌套位置：它包在内核整个处理器（含访问
// 日志）的外面，因此启动时进入行还没写，收尾时两行都写完了。位置换到内侧的话，宿主加进
// context 的值就传不到回调。
func TestMiddlewareNestsOutsideAccessLog(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	cfg, err := config.Parse(strings.NewReader(seamYAML(upstreamServer.URL)))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var arrivalPending, completionDone bool
	handler, err := server.FromConfigWith(cfg, logger, server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				arrivalPending = !strings.Contains(logs.String(), "请求进入")
				next.ServeHTTP(writer, request)
				completionDone = strings.Contains(logs.String(), "请求完成")
			})
		}))
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	front := httptest.NewServer(handler.Handler())
	defer front.Close()

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if !arrivalPending {
		t.Fatal("中间件启动时进入行不应当已经写出：中间件装到了访问日志的内侧")
	}
	if !completionDone {
		t.Fatal("中间件收尾时完成行应当已经写出：中间件装到了访问日志的内侧")
	}
}

// testTagKey 是宿主加进 context 的自取键。
type testTagKey struct{}

// TestOnRequestDoneStateReachesCallback 是上一条的正式形态：中间件加进 context 的状态
// 必须在回调里读到。
func TestOnRequestDoneStateReachesCallback(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	var (
		mu       sync.Mutex
		gotTag   string
		got      server.Outcome
		hadTag   bool
		callback bool
	)
	front, _, _ := newSeamServer(t, seamYAML(upstreamServer.URL),
		server.WithMiddleware(
			func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					ctx := context.WithValue(request.Context(), testTagKey{}, "宿主放的")
					next.ServeHTTP(writer, request.WithContext(ctx))
				})
			}),
		server.WithRequestDone(func(ctx context.Context, outcome server.Outcome) {
			mu.Lock()
			defer mu.Unlock()
			callback = true
			got = outcome
			if tag, ok := ctx.Value(testTagKey{}).(string); ok {
				hadTag = true
				gotTag = tag
			}
		}))

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}

	// 等回调跑完：响应对客户端可见可能早于服务端收尾。
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := callback
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("回调 3 秒内没有执行")
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if !hadTag || gotTag != "宿主放的" {
		t.Fatalf("中间件加进 context 的状态没到回调: had=%t tag=%q", hadTag, gotTag)
	}
	if got.Channel != "upstream" || len(got.Attempts) != 1 || got.Status != http.StatusOK {
		t.Fatalf("回调拿到的 Outcome 不全: %+v", got)
	}
	if got.RequestID == "" {
		t.Fatalf("request_id 不能为空: %+v", got)
	}
}

// TestMiddlewareCanChangeRequestAndResponse 覆盖「操作这些数据」：中间件能换掉请求体
// （内核从 request.Body 读，因此换过之后参与路由的就是新的），也能加回给客户端的响应头。
//
// 直接改进来的请求头不会传到上游：上游请求头来自渠道配置与 Auth，内核不从请求里抄。
// 要按请求给上游加头得走 server.WithUpstreamHeader，见 upstream_test.go。
func TestMiddlewareCanChangeRequestAndResponse(t *testing.T) {
	const replaced = `{"model":"client-model","messages":[{"role":"user","content":"宿主换过的"}]}`
	upstreamRecord, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, outcomes := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_ = request.Body.Close()
				request.Body = io.NopCloser(strings.NewReader(replaced))
				request.ContentLength = int64(len(replaced))
				request.Header.Set("X-Direct", "宿主直接改的")
				writer.Header().Set("X-Host-Reply", "宿主加的")
				next.ServeHTTP(writer, request)
			})
		}))

	status, header, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"原始内容"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if got := header.Get("X-Host-Reply"); got != "宿主加的" {
		t.Fatalf("回给客户端的响应头没生效: %q", got)
	}
	if got := string(upstreamRecord.last(t).Body); got != replaced {
		t.Fatalf("上游收到的请求体应当是中间件换过的那个:\n期望 %s\n实际 %s", replaced, got)
	}
	if got := upstreamRecord.last(t).Header.Get("X-Direct"); got != "" {
		t.Fatalf("直接改进来的请求头不该传到上游: X-Direct=%q", got)
	}
	if got := outcomes.wait(t, 1)[0]; got.Status != http.StatusOK {
		t.Fatalf("Outcome 不符: %+v", got)
	}
}

// TestMiddlewareSkippingNextSkipsKernel 覆盖外侧布局的结构代价：宿主在中间件里不调 next
// （例如拒绝请求）时内核整个不参与，没有日志也没有回调，那条请求由宿主自己记账。
func TestMiddlewareSkippingNextSkipsKernel(t *testing.T) {
	front, logs, outcomes := newSeamServer(t, seamYAML("https://example.com"), server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.WriteHeader(http.StatusForbidden)
			})
		}))

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusForbidden {
		t.Fatalf("宿主拒绝请求时状态码按它写的来: %d", status)
	}
	// 回调不该触发。它是同步的，等一小段确认确实没有。
	time.Sleep(50 * time.Millisecond)
	if n := len(outcomes.outcomes); n != 0 {
		t.Fatalf("宿主不调 next 时不该有回调，实际 %d 条", n)
	}
	if text := logs.String(); strings.Contains(text, "请求进入") || strings.Contains(text, "请求完成") {
		t.Fatalf("宿主不调 next 时内核不该写访问日志: %s", text)
	}
}

// TestMiddlewareKeepsStreamingFlush 覆盖包装 http.ResponseWriter 时最容易踩的一处：
// 包装层必须把 Flush 转发下去。这里按推荐写法包一层，流式响应照旧逐片到达。
func TestMiddlewareKeepsStreamingFlush(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: first\n\n"))
		writer.(http.Flusher).Flush()
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		writer.(http.Flusher).Flush()
	})
	front, _, _ := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				next.ServeHTTP(&flushForwardingWriter{ResponseWriter: writer}, request)
			})
		}))

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d", status)
	}
	if text := string(body); !strings.Contains(text, "first") || !strings.Contains(text, "[DONE]") {
		t.Fatalf("流式内容不全: %q", text)
	}
}

// flushForwardingWriter 是宿主包装 http.ResponseWriter 时的推荐写法：把 Flush 转下去。
type flushForwardingWriter struct {
	http.ResponseWriter
}

func (w *flushForwardingWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// TestWithoutAccessLogSilencesCoreLines 覆盖关掉内核日志：两行都不写，但 request_id
// 仍然生成，其余 WARN 行照旧带着它，Outcome 也不受影响。
func TestWithoutAccessLogSilencesCoreLines(t *testing.T) {
	front, logs, outcomes := newSeamServer(t, `
channels:
  - {name: dead, dialect: openai, base_url: "http://127.0.0.1:1"}
models:
  - {match: "client-model", channel: dead}
`, server.WithoutAccessLog())

	status, _, _ := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusBadGateway {
		t.Fatalf("候选全部失败应当 502: %d", status)
	}
	lines := waitForLog(t, logs, "上游请求失败", 1)
	if text := logs.String(); strings.Contains(text, "请求进入") || strings.Contains(text, "请求完成") {
		t.Fatalf("关掉访问日志之后不该有这两行: %s", text)
	}
	warn := lineWith(t, lines, "msg", "上游请求失败")
	if stringField(t, warn, "request_id") == "" {
		t.Fatalf("request_id 仍然要生成，其余 WARN 行靠它串起来: %v", warn)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.RequestID == "" || outcome.Status != http.StatusBadGateway {
		t.Fatalf("Outcome 不受关日志影响: %+v", outcome)
	}
}

// TestSeamServerStillServesWithoutMiddleware 确认不装中间件时一切照旧。
func TestSeamServerStillServesWithoutMiddleware(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, logs, outcomes := newSeamServer(t, seamYAML(upstreamServer.URL))

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	lines := waitForLog(t, logs, "请求完成", 1)
	if len(completedLines(t, lines)) != 1 {
		t.Fatalf("完成行应当只有一条: %v", lines)
	}
	if got := outcomes.wait(t, 1)[0]; got.Channel != "upstream" {
		t.Fatalf("Outcome 不符: %+v", got)
	}
	response, err := get(t, front.URL+"/healthz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}
