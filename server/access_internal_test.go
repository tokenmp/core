package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// syncBuffer 是带锁的日志缓冲。处理器在另一个 goroutine 里写日志，直接读
// bytes.Buffer 会构成数据竞争，-race 下会被报出来。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(data)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAccessMiddlewareLogsArrivalBeforeHandlerReturns 覆盖进入行的时序：处理器还在
// 跑、请求尚未完成时，进入行就已经写出。
//
// 这正是进入行存在的理由——只靠完成行时，流式响应跑的几分钟里日志看不出请求正在跑，
// 请求卡住或进程被杀时留下的是一条永远不会出现的完成行。
func TestAccessMiddlewareLogsArrivalBeforeHandlerReturns(t *testing.T) {
	logs := &syncBuffer{}
	service := &Server{logger: slog.New(slog.NewJSONHandler(logs, nil)), now: time.Now}

	entered := make(chan struct{})
	release := make(chan struct{})
	handler := service.withAccessLog(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release // 处理器停在这里，请求还没有完成
		writer.WriteHeader(http.StatusOK)
	}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/slow", nil))
	}()

	<-entered
	if text := logs.String(); !strings.Contains(text, `"msg":"请求进入"`) {
		t.Fatalf("处理器尚未返回时应当已经写出进入行: %s", text)
	}
	if text := logs.String(); strings.Contains(text, `"msg":"请求完成"`) {
		t.Fatalf("处理器尚未返回时不该有完成行: %s", text)
	}

	close(release)
	<-done
	if text := logs.String(); !strings.Contains(text, `"msg":"请求完成"`) {
		t.Fatalf("处理器返回后应当写出完成行: %s", text)
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
