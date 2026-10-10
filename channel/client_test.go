package channel_test

import (
	"bytes"
	"context"
	"errors"
	"io"
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
