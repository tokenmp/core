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
	"unicode/utf8"
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

// TestAccessMiddlewareLogsArrivalBeforeHandlerReturns 覆盖进入行的时序：处理器还在
// 跑、请求尚未完成时，进入行就已经写出。
//
// 这正是进入行存在的理由——只靠完成行时，流式响应跑的几分钟里日志看不出请求正在跑；
// 请求卡住或进程被杀时完成行不再出现，至少还留下这一行。
//
// 日志缓冲不需要加锁：三次读取都排在 channel 之后，handler goroutine 里对应的写入与
// 读取之间因此有 happens-before 关系，把锁去掉之后 -race 仍然通过。以后若在未经
// channel 的位置读它，-race 会把新出现的竞争报出来。
func TestAccessMiddlewareLogsArrivalBeforeHandlerReturns(t *testing.T) {
	logs := &bytes.Buffer{}
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

// TestClipLogValue 覆盖日志字段的截断。
//
// 这些值来自请求，别处约束不了它们：请求体上限是 32 MiB，请求头走 Go 默认的 1 MiB
// （Server 没有设 MaxHeaderBytes），而同一个值在进入行与完成行里各写一次。
func TestClipLogValue(t *testing.T) {
	// 保留前缀的长度是上限减去省略号 —— 省略号算在上限之内。
	prefix := maxLogValueBytes - len(logValueEllipsis)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "空串原样", in: "", want: ""},
		{name: "短的不动", in: "curl/8.5.0", want: "curl/8.5.0"},
		{name: "恰好到上限不动", in: strings.Repeat("a", maxLogValueBytes), want: strings.Repeat("a", maxLogValueBytes)},
		{name: "超一个字节就截", in: strings.Repeat("a", maxLogValueBytes+1), want: strings.Repeat("a", prefix) + logValueEllipsis},
		// 「中」占三个字节且跨在保留前缀的末尾上，应当回退到字符边界，而不是切出半个字符。
		{name: "截断点落在多字节字符中间时回退", in: strings.Repeat("a", prefix-1) + "中" + strings.Repeat("a", 10), want: strings.Repeat("a", prefix-1) + logValueEllipsis},
		// 请求头里允许出现非法 UTF-8（Go 的解析不校验）。这种输入截断后仍会带着非法字节，
		// 这里只要求不 panic、不超过上限，输出交给 slog 转义。
		{name: "非法 UTF-8 原样留在前缀里", in: strings.Repeat("a", 100) + "\xff" + strings.Repeat("a", 300), want: strings.Repeat("a", 100) + "\xff" + strings.Repeat("a", prefix-101) + logValueEllipsis},
		{name: "全是 continuation 字节时退到只剩标记", in: strings.Repeat("\x80", 400), want: logValueEllipsis},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := clipLogValue(testCase.in)
			if got != testCase.want {
				t.Fatalf("截断结果不符：期望 %q，实际 %q", testCase.want, got)
			}
			if len(got) > maxLogValueBytes {
				t.Fatalf("截断之后超过上限：%d 字节", len(got))
			}
			// 输入本身合法时，回退必须保证输出也合法。
			if utf8.ValidString(testCase.in) && !utf8.ValidString(got) {
				t.Fatalf("合法输入截出了非法 UTF-8: %q", got)
			}
		})
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
