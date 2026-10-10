package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestMiddlewareNestsInsideAccessLog 覆盖中间件的嵌套位置：内核的进入行在它之前写出，
// 完成行在它之后写出。位置换到外侧的话 context 里就没有访问记录，OutcomeFromContext 会失败。
func TestMiddlewareNestsInsideAccessLog(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	cfg, err := config.Parse(strings.NewReader(seamYAML(upstreamServer.URL)))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var arrivedWritten, completionPending bool
	handler, err := server.FromConfigWith(cfg, logger, server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				arrivedWritten = strings.Contains(logs.String(), "请求进入")
				next.ServeHTTP(writer, request)
				completionPending = !strings.Contains(logs.String(), "请求完成")
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
	if !arrivedWritten {
		t.Fatal("中间件启动时进入行应当已经写出：中间件装到了访问日志的外侧")
	}
	if !completionPending {
		t.Fatal("中间件收尾时完成行不应当已经写出：中间件装到了访问日志的外侧")
	}
}

// TestMiddlewareReadsOutcomeFromContext 覆盖这个缺口的主要用途：宿主在自己中间件里
// next 返回之后读到内核的内部数据，不必去解析日志。
func TestMiddlewareReadsOutcomeFromContext(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	var (
		got    server.Outcome
		usable bool
	)
	front, _, _ := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				next.ServeHTTP(writer, request)
				got, usable = server.OutcomeFromContext(request.Context())
			})
		}))

	status, _, body := post(t, front.URL+"/v1/messages",
		`{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if !usable {
		t.Fatal("中间件里应当能读到本次请求的 Outcome")
	}
	if got.RequestID == "" || got.Channel != "upstream" || got.Status != http.StatusOK {
		t.Fatalf("内部数据不全: %+v", got)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Status != http.StatusOK {
		t.Fatalf("候选尝试没读到: %+v", got.Attempts)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 5 {
		t.Fatalf("用量没读到: %+v", got.Usage)
	}
	if got.FirstByte <= 0 || got.Duration <= 0 {
		t.Fatalf("时长没读到: first=%v duration=%v", got.FirstByte, got.Duration)
	}
	if got.BytesWritten != int64(len(body)) {
		t.Fatalf("写出字节数不符: %d，客户端收到 %d", got.BytesWritten, len(body))
	}
}

// TestMiddlewareCanChangeRequestAndResponse 覆盖「操作这些数据」：中间件能换掉请求体
// （内核从 request.Body 读，因此换过之后参与路由的就是新的），也能加回给客户端的响应头。
//
// 改请求头不会传到上游：上游请求头来自渠道配置，内核不从进来的请求里抄。
func TestMiddlewareCanChangeRequestAndResponse(t *testing.T) {
	const replaced = `{"model":"client-model","messages":[{"role":"user","content":"宿主换过的"}]}`
	upstreamRecord, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, _, outcomes := newSeamServer(t, seamYAML(upstreamServer.URL), server.WithMiddleware(
		func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				// 内核从这里读请求体，换掉之后走的就是新内容。
				_ = request.Body.Close()
				request.Body = io.NopCloser(strings.NewReader(replaced))
				request.ContentLength = int64(len(replaced))
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
	if got := outcomes.wait(t, 1)[0]; got.Status != http.StatusOK {
		t.Fatalf("Outcome 不符: %+v", got)
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
	// 等 WARN 行写出来再断言，否则会有竞态。
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

// TestOutcomeFromContextOutsideRequest 覆盖不在请求范围内时取不到数据。
func TestOutcomeFromContextOutsideRequest(t *testing.T) {
	if got, ok := server.OutcomeFromContext(context.Background()); ok {
		t.Fatalf("不在请求范围内应当取不到: %+v", got)
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
	// 中间件没装时 OutcomeFromContext 在 handler 之外自然取不到。
	response, err := get(t, front.URL+"/healthz")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
}
