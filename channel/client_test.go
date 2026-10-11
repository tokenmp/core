package channel_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokenmp/core/channel"
	"github.com/tokenmp/core/dialect"
)

func TestSpecValidate(t *testing.T) {
	base := channel.Spec{
		Name:    "a",
		Dialect: dialect.OpenAIChat,
		BaseURL: "https://api.example.com",
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("合法渠道不应报错: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*channel.Spec)
		keyword string
	}{
		{"缺名字", func(s *channel.Spec) { s.Name = "" }, "name"},
		{"方言未知", func(s *channel.Spec) { s.Dialect = "anthropic" }, "dialect"},
		{"协议不对", func(s *channel.Spec) { s.BaseURL = "ftp://x" }, "http://"},
		{
			"基址带路径却没写 path",
			func(s *channel.Spec) { s.BaseURL = "https://openrouter.ai/api/v1" },
			"path",
		},
		{
			"Gemini 不允许覆盖路径",
			func(s *channel.Spec) {
				s.Dialect = dialect.GeminiGenerate
				s.Path = "/v1beta/models/x:generateContent"
			},
			"path",
		},
		{"负重试", func(s *channel.Spec) { s.MaxRetries = -1 }, "max_retries"},
		{"重试次数过大", func(s *channel.Spec) { s.MaxRetries = 99 }, "max_retries"},
		{
			"基址带查询串",
			func(s *channel.Spec) { s.BaseURL = "https://api.example.com?token=x" },
			"查询串",
		},
		{
			"请求头含换行",
			func(s *channel.Spec) { s.Headers = map[string]string{"X-Test": "a\r\nX-Evil: 1"} },
			"换行",
		},
		{"鉴权缺 key_env", func(s *channel.Spec) { s.Auth = channel.Auth{Style: "bearer"} }, "key_env"},
		{"custom 缺 header", func(s *channel.Spec) { s.Auth = channel.Auth{Style: "custom", KeyEnv: "K"} }, "header"},
		{"query 缺参数名", func(s *channel.Spec) { s.Auth = channel.Auth{Style: "query", KeyEnv: "K"} }, "query"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			spec := base
			item.mutate(&spec)
			err := spec.Validate()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), item.keyword) {
				t.Fatalf("错误信息应包含 %q，实际 %v", item.keyword, err)
			}
		})
	}
}

func TestClientRequiresCredential(t *testing.T) {
	_, err := channel.NewClient(channel.Spec{
		Name:    "a",
		Dialect: dialect.OpenAIChat,
		BaseURL: "https://api.example.com",
		Auth:    channel.Auth{Style: "bearer", KeyEnv: "TOKENMP_TEST_MISSING_KEY"},
	})
	if err == nil {
		t.Fatal("凭据缺失应当报错")
	}
	if !strings.Contains(err.Error(), "TOKENMP_TEST_MISSING_KEY") {
		t.Fatalf("错误信息应指出环境变量名: %v", err)
	}
}

// authCase 是一种鉴权样式的期望位置。
type authCase struct {
	style  string
	header string
	query  string
}

func TestClientAppliesAuthStyles(t *testing.T) {
	cases := []authCase{
		{style: "bearer"},
		{style: "x-api-key"},
		{style: "custom", header: "X-Custom-Key"},
		{style: "query", query: "key"},
		{style: "none"},
	}
	for _, item := range cases {
		t.Run(item.style, func(t *testing.T) {
			var gotHeader, gotQuery string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				gotHeader = request.Header.Get(headerName(item))
				gotQuery = request.URL.Query().Get(item.query)
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write([]byte(`{}`))
			}))
			defer server.Close()

			t.Setenv("TOKENMP_TEST_KEY", "secret-value")
			client, err := channel.NewClient(channel.Spec{
				Name:    "a",
				Dialect: dialect.OpenAIChat,
				BaseURL: server.URL,
				Auth:    channel.Auth{Style: item.style, KeyEnv: "TOKENMP_TEST_KEY", Header: item.header, Query: item.query},
			})
			if err != nil {
				t.Fatalf("创建客户端失败: %v", err)
			}
			response, err := client.Send(context.Background(), channel.Request{Body: []byte(`{}`)})
			if err != nil {
				t.Fatalf("请求失败: %v", err)
			}
			defer func() { _ = response.Body.Close() }()

			switch item.style {
			case "bearer":
				if gotHeader != "Bearer secret-value" {
					t.Fatalf("Authorization 头不符: %q", gotHeader)
				}
			case "x-api-key":
				if gotHeader != "secret-value" {
					t.Fatalf("x-api-key 头不符: %q", gotHeader)
				}
			case "custom":
				if gotHeader != "secret-value" {
					t.Fatalf("自定义头不符: %q", gotHeader)
				}
			case "query":
				if gotQuery != "secret-value" {
					t.Fatalf("查询参数不符: %q", gotQuery)
				}
			case "none":
				if gotHeader != "" {
					t.Fatalf("不鉴权时不应带鉴权头: %q", gotHeader)
				}
			}
		})
	}
}

// headerName 返回该样式使用的头名。
func headerName(item authCase) string {
	switch item.style {
	case "bearer":
		return "Authorization"
	case "x-api-key":
		return "x-api-key"
	default:
		return item.header
	}
}

// TestClientRequestHeaderOverridesSpecHeaders 钉住两条来源的先后：Spec.Headers 是渠道级的
// 默认值，Request.Header 是请求级的值，同名时后者覆盖前者，线上只出现一条。
func TestClientRequestHeaderOverridesSpecHeaders(t *testing.T) {
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		got = request.Header.Clone()
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name:    "a",
		Dialect: dialect.OpenAIChat,
		BaseURL: server.URL,
		Headers: map[string]string{"X-Title": "from-config", "X-Stays": "keep"},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	response, err := client.Send(context.Background(), channel.Request{
		Body:   []byte(`{}`),
		Header: http.Header{"X-Title": {"from-request"}},
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if values := got.Values("X-Title"); len(values) != 1 || values[0] != "from-request" {
		t.Fatalf("请求级的同名头应当覆盖渠道配置: X-Title=%v", values)
	}
	if value := got.Get("X-Stays"); value != "keep" {
		t.Fatalf("渠道配置里没被覆盖的头应当照旧: X-Stays=%q", value)
	}
}

func TestClientBasePathAndGeminiPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name:    "gemini",
		Dialect: dialect.GeminiGenerate,
		BaseURL: server.URL,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	response, err := client.Send(context.Background(), channel.Request{Model: "gemini-2.5-pro", Stream: true})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("Gemini 路径不符: %s", gotPath)
	}
}

func TestClientRetriesRetryableStatus(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, MaxRetries: 2,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	response, err := client.Send(context.Background(), channel.Request{Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("重试后应当成功，实际 %d", response.StatusCode)
	}
	if attempts.Load() != 2 {
		t.Fatalf("应当重试一次，实际请求 %d 次", attempts.Load())
	}
}

func TestClientDoesNotRetryClientError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":"bad"}`))
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	response, err := client.Send(context.Background(), channel.Request{Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码不符: %d", response.StatusCode)
	}
	if attempts.Load() != 1 {
		t.Fatalf("400 不应重试，实际请求 %d 次", attempts.Load())
	}
}

// TestClientReportsEveryRetry 覆盖 issue #25 要的口径：每次渠道内重试都通过
// OnRetry 交出来，Attempt 从 1 开始，Status 是触发重试的状态码，Wait 是退避时长。
func TestClientReportsEveryRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, MaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var infos []channel.RetryInfo
	response, err := client.Send(context.Background(), channel.Request{
		Body:    []byte(`{}`),
		OnRetry: func(info channel.RetryInfo) { infos = append(infos, info) },
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("重试耗尽时应当把 429 原样返回，实际 %d", response.StatusCode)
	}
	if attempts.Load() != 4 {
		t.Fatalf("max_retries: 3 时上游应收到 4 次请求，实际 %d", attempts.Load())
	}
	wantWaits := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}
	if len(infos) != len(wantWaits) {
		t.Fatalf("应当回调 %d 次，实际 %d: %+v", len(wantWaits), len(infos), infos)
	}
	for index, info := range infos {
		if info.Attempt != index+1 {
			t.Fatalf("第 %d 次回调的 Attempt 不符: %d", index+1, info.Attempt)
		}
		if info.Status != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次回调的 Status 不符: %d", index+1, info.Status)
		}
		if info.Wait != wantWaits[index] {
			t.Fatalf("第 %d 次回调的 Wait 不符: %v，期望 %v", index+1, info.Wait, wantWaits[index])
		}
	}
}

// TestClientWithoutRetriesDoesNotCallBack 覆盖 max_retries: 0：一次重试都不发，
// 回调一次都不触发。
func TestClientWithoutRetriesDoesNotCallBack(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, MaxRetries: 0,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	called := 0
	response, err := client.Send(context.Background(), channel.Request{
		Body:    []byte(`{}`),
		OnRetry: func(channel.RetryInfo) { called++ },
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if attempts.Load() != 1 {
		t.Fatalf("max_retries: 0 时上游只应收到 1 次请求，实际 %d", attempts.Load())
	}
	if called != 0 {
		t.Fatalf("max_retries: 0 时不该触发重试回调，实际 %d 次", called)
	}
}

// TestClientReportsRetryAfterConnectError 覆盖请求没建立的情况：没有状态码可报，
// Status 为 0，回调照常触发。
func TestClientReportsRetryAfterConnectError(t *testing.T) {
	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: "http://127.0.0.1:1", MaxRetries: 1,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var infos []channel.RetryInfo
	response, err := client.Send(context.Background(), channel.Request{
		Body:    []byte(`{}`),
		OnRetry: func(info channel.RetryInfo) { infos = append(infos, info) },
	})
	if err == nil {
		_ = response.Body.Close()
		t.Fatal("连不上上游时应当报错")
	}
	if len(infos) != 1 || infos[0].Attempt != 1 || infos[0].Status != 0 {
		t.Fatalf("连接失败的重试回调不符: %+v", infos)
	}
}

// TestClientHonorsRetryAfter 覆盖 issue #27 的核心：上游回 429 且带
// Retry-After 时，两次请求之间至少隔上游要求的时长，回调报出的也是这个值。
func TestClientHonorsRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			writer.Header().Set("Retry-After", "1")
			writer.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, MaxRetries: 2,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var infos []channel.RetryInfo
	started := time.Now()
	response, err := client.Send(context.Background(), channel.Request{
		Body:    []byte(`{}`),
		OnRetry: func(info channel.RetryInfo) { infos = append(infos, info) },
	})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("重试后应当成功，实际 %d", response.StatusCode)
	}
	if len(infos) != 1 || infos[0].Wait != time.Second {
		t.Fatalf("回调的 Wait 应当是 1s（Retry-After 大于指数退避），实际 %+v", infos)
	}
	if elapsed < time.Second {
		t.Fatalf("两次请求之间应当至少隔 1 秒，实际 %s", elapsed)
	}
}

// TestClientStopsWhenRetryAfterExceedsLimit 覆盖 #27 的上限口径：上游要求的等待
// 超过 max_retry_after 时不再重试，把 429 原样交回调用方，也不触发重试回调。
// 把它缩短成上限再重试是错的，那会让请求早于上游允许的时刻发出。
func TestClientStopsWhenRetryAfterExceedsLimit(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Retry-After", "600")
		writer.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name:       "a",
		Dialect:    dialect.OpenAIChat,
		BaseURL:    server.URL,
		MaxRetries: 3,
		Backoff:    channel.Backoff{MaxRetryAfter: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	called := 0
	response, err := client.Send(context.Background(), channel.Request{
		Body:    []byte(`{}`),
		OnRetry: func(channel.RetryInfo) { called++ },
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("超限时应当把 429 原样交回，实际 %d", response.StatusCode)
	}
	if attempts.Load() != 1 {
		t.Fatalf("超限时不应再发请求，实际 %d 次", attempts.Load())
	}
	if called != 0 {
		t.Fatalf("超限时不应触发重试回调，实际 %d 次", called)
	}
}

// TestClientUsesConfiguredBackoff 覆盖 #27 的另一半：退避基础值与上限可配。
// 配了 base、max 之后单次退避就是按这两个值算出来的。
func TestClientUsesConfiguredBackoff(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name:       "a",
		Dialect:    dialect.OpenAIChat,
		BaseURL:    server.URL,
		MaxRetries: 2,
		Backoff:    channel.Backoff{Base: 10 * time.Millisecond, Max: 15 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	var infos []channel.RetryInfo
	response, err := client.Send(context.Background(), channel.Request{
		Body:    []byte(`{}`),
		OnRetry: func(info channel.RetryInfo) { infos = append(infos, info) },
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	want := []time.Duration{10 * time.Millisecond, 15 * time.Millisecond}
	if len(infos) != len(want) {
		t.Fatalf("应当回调 %d 次，实际 %+v", len(want), infos)
	}
	for index, info := range infos {
		if info.Wait != want[index] {
			t.Fatalf("第 %d 次回调的 Wait 不符: %v，期望 %v", index+1, info.Wait, want[index])
		}
	}
}

func TestSSEReaderParsesEvents(t *testing.T) {
	input := ": 注释\n" +
		"event: message_start\n" +
		"data: {\"a\":1}\n\n" +
		"data: 第一行\n" +
		"data: 第二行\n\n" +
		"id: 7\nretry: 100\n\n" +
		"data: [DONE]\n\n"
	reader := channel.NewReader(strings.NewReader(input))
	var events []channel.Event
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		events = append(events, event)
	}
	if len(events) != 3 {
		t.Fatalf("应当读出 3 条事件，实际 %d: %+v", len(events), events)
	}
	if events[0].Name != "message_start" || string(events[0].Data) != `{"a":1}` {
		t.Fatalf("第一条事件不符: %+v", events[0])
	}
	if string(events[1].Data) != "第一行\n第二行" {
		t.Fatalf("多行 data 拼接不符: %q", events[1].Data)
	}
	if !dialect.IsDoneMarker(events[2].Data) {
		t.Fatalf("结束标记不符: %q", events[2].Data)
	}
}

func TestSSEWriterRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	writer := channel.NewWriter(&buffer)
	if err := writer.Write(channel.Event{Name: "content_block_delta", Data: []byte(`{"x":1}`)}); err != nil {
		t.Fatalf("写出失败: %v", err)
	}
	if err := writer.WriteRaw([]byte("[DONE]")); err != nil {
		t.Fatalf("写出结束标记失败: %v", err)
	}
	expected := "event: content_block_delta\ndata: {\"x\":1}\n\ndata: [DONE]\n\n"
	if buffer.String() != expected {
		t.Fatalf("输出不符:\n期望 %q\n实际 %q", expected, buffer.String())
	}
}

func TestSSEReaderKeepsLineWithoutTrailingNewline(t *testing.T) {
	// 上游的最后一行常常没有收尾换行符。读行时若把行连同 io.EOF 一起丢掉，
	// 那一段内容就消失了，下游看到的是一个被截断却像正常结束的流。
	cases := []struct {
		name  string
		input string
		want  []string
	}{
		{"最后一行无换行", "data: 第一行\ndata: 第二行", []string{"第一行\n第二行"}},
		{"最后一行有换行", "data: 第一行\ndata: 第二行\n", []string{"第一行\n第二行"}},
		{"单行无换行", "data: 只有一行", []string{"只有一行"}},
		{"坏 JSON 也要读出来", "data: {\"a\":1}\n\ndata: {坏", []string{`{"a":1}`, "{坏"}},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			reader := channel.NewReader(strings.NewReader(item.input))
			var got []string
			for {
				event, err := reader.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("读取失败: %v", err)
				}
				got = append(got, string(event.Data))
			}
			if len(got) != len(item.want) {
				t.Fatalf("事件数不符: 期望 %d，实际 %d (%q)", len(item.want), len(got), got)
			}
			for index := range got {
				if got[index] != item.want[index] {
					t.Fatalf("第 %d 条事件不符: 期望 %q，实际 %q", index+1, item.want[index], got[index])
				}
			}
		})
	}
}

func TestSSEReaderBoundsLineLength(t *testing.T) {
	reader := channel.NewReader(strings.NewReader(strings.Repeat("x", 1<<20)))
	reader.SetMaxEventBytes(1024)
	if _, err := reader.Next(); !errors.Is(err, channel.ErrEventTooLarge) {
		t.Fatalf("超长单行应当被拦下，实际 %v", err)
	}
}

func TestCopyRawPreservesBytes(t *testing.T) {
	payload := "data: {\"a\":  1}\n\ndata: [DONE]\n\n"
	var buffer bytes.Buffer
	flushed := 0
	written, err := channel.CopyRaw(&buffer, strings.NewReader(payload), func() { flushed++ })
	if err != nil {
		t.Fatalf("搬运失败: %v", err)
	}
	if written != int64(len(payload)) || buffer.String() != payload {
		t.Fatalf("字节未被保留: %q", buffer.String())
	}
	if flushed == 0 {
		t.Fatal("应当调用刷新回调")
	}
}

func TestSetRejectsDuplicates(t *testing.T) {
	_, err := channel.NewSet([]channel.Spec{
		{Name: "a", Dialect: dialect.OpenAIChat, BaseURL: "https://x"},
		{Name: "a", Dialect: dialect.OpenAIChat, BaseURL: "https://x"},
	})
	if err == nil {
		t.Fatal("重名渠道应当报错")
	}
}

func TestNonStreamingBodySurvivesSendReturn(t *testing.T) {
	// 上游先送响应头再送体，且体大于传输层缓冲区。整体超时必须覆盖到读体，
	// 否则 Send 一返回连接就被取消，调用方读到的是一段截断的响应。
	payload := bytes.Repeat([]byte("x"), 256<<10)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = writer.Write(payload)
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	response, err := client.Send(context.Background(), channel.Request{Body: []byte(`{}`)})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("响应体不完整: 期望 %d 字节，实际 %d", len(payload), len(body))
	}
}

func TestQueryAuthCredentialNotLeakedInError(t *testing.T) {
	const secret = "SUPERSECRETVALUE"
	t.Setenv("TOKENMP_TEST_KEY", secret)

	// 占一个端口再放掉，保证连接一定会被拒绝。
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	address := listener.Addr().String()
	if closeErr := listener.Close(); closeErr != nil {
		t.Fatalf("释放端口失败: %v", closeErr)
	}

	client, err := channel.NewClient(channel.Spec{
		Name:    "a",
		Dialect: dialect.OpenAIChat,
		BaseURL: "http://" + address,
		Auth:    channel.Auth{Style: channel.StyleQuery, Query: "key", KeyEnv: "TOKENMP_TEST_KEY"},
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	response, err := client.Send(context.Background(), channel.Request{Body: []byte(`{}`)})
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("连接被拒绝时应当报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("错误信息里出现了凭据: %v", err)
	}
}

func TestSendRespectsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		time.Sleep(300 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := channel.NewClient(channel.Spec{
		Name: "a", Dialect: dialect.OpenAIChat, BaseURL: server.URL, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	response, err := client.Send(ctx, channel.Request{Body: []byte(`{}`)})
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("context 取消后应当报错")
	}
}
