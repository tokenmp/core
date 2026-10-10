package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAccessWriterForwardsFlush 覆盖最容易踩的一处：包装层吃掉 Flush 会让流式响应
// 静默退化成不刷新。channel 与 server 里都用忽略失败的类型断言取刷新器，因此这里
// 直接对着包装层断言，不依赖端到端时序。
func TestAccessWriterForwardsFlush(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &accessWriter{ResponseWriter: recorder, record: &accessRecord{}}
	flusher, ok := http.ResponseWriter(writer).(http.Flusher)
	if !ok {
		t.Fatal("包装层没有转发 http.Flusher：流式响应会静默退化成不刷新")
	}
	if recorder.Flushed {
		t.Fatal("还没有调 Flush，底层不应处于已刷新状态")
	}
	flusher.Flush()
	if !recorder.Flushed {
		t.Fatal("Flush 没有到达底层")
	}
}

// TestAccessWriterRecordsImplicitStatus 覆盖处理器不调 WriteHeader 直接写 body：
// net/http 会按 200 写响应头，记录也要按 200 记。
func TestAccessWriterRecordsImplicitStatus(t *testing.T) {
	record := &accessRecord{}
	writer := &accessWriter{ResponseWriter: httptest.NewRecorder(), record: record}
	if _, err := writer.Write([]byte("ok")); err != nil {
		t.Fatalf("写出失败: %v", err)
	}
	if record.status != http.StatusOK {
		t.Fatalf("隐式状态码应当是 200，实际 %d", record.status)
	}
}

// TestAccessMiddlewareLogsPanic 覆盖处理器 panic：这条请求确实失败了，不能按
// 「没写响应头就是 200」记成成功。
func TestAccessMiddlewareLogsPanic(t *testing.T) {
	logs := &bytes.Buffer{}
	service := &Server{logger: slog.New(slog.NewJSONHandler(logs, nil)), now: time.Now}
	handler := service.withAccessLog(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("故意失败")
	}))
	func() {
		defer func() { _ = recover() }()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/boom", nil))
	}()
	text := logs.String()
	if !strings.Contains(text, `"status":500`) || !strings.Contains(text, `"level":"WARN"`) {
		t.Fatalf("处理器 panic 时应当留一条 500 的 WARN 访问日志: %s", text)
	}
	if !strings.Contains(text, `"request_id"`) {
		t.Fatalf("访问日志缺少 request_id: %s", text)
	}
}
