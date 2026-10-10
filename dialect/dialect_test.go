package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/tokenmp/core/dialect"
)

func TestNamesAndParse(t *testing.T) {
	names := dialect.Names()
	if len(names) != 4 {
		t.Fatalf("应有四种方言，实际 %d", len(names))
	}
	for _, name := range names {
		parsed, ok := dialect.Parse(strings.ToUpper(string(name)))
		if !ok || parsed != name {
			t.Fatalf("解析 %s 失败", name)
		}
		format := name.RelayFormat()
		back, ok := dialect.FromRelayFormat(format)
		if !ok || back != name {
			t.Fatalf("%s 的 RelayFormat 往返失败: %s", name, format)
		}
	}
	if _, ok := dialect.Parse("anthropic"); ok {
		t.Fatal("未知识别出了方言")
	}
	if _, ok := dialect.FromRelayFormat(types.RelayFormatEmbedding); ok {
		t.Fatal("非文本格式不应映射成方言")
	}
}

func TestUpstreamPath(t *testing.T) {
	cases := []struct {
		name     dialect.Name
		model    string
		stream   bool
		expected string
	}{
		{dialect.OpenAIChat, "gpt-4", false, "/v1/chat/completions"},
		{dialect.OpenAIChat, "gpt-4", true, "/v1/chat/completions"},
		{dialect.OpenAIResponses, "gpt-4", false, "/v1/responses"},
		{dialect.ClaudeMessages, "claude-3", true, "/v1/messages"},
		{dialect.GeminiGenerate, "gemini-2.5-pro", false, "/v1beta/models/gemini-2.5-pro:generateContent"},
		{dialect.GeminiGenerate, "gemini-2.5-pro", true, "/v1beta/models/gemini-2.5-pro:streamGenerateContent?alt=sse"},
	}
	for _, item := range cases {
		path, err := item.name.UpstreamPath(item.model, item.stream)
		if err != nil {
			t.Fatalf("%s: %v", item.name, err)
		}
		if path != item.expected {
			t.Fatalf("%s: 期望 %s，实际 %s", item.name, item.expected, path)
		}
	}
	if _, err := dialect.GeminiGenerate.UpstreamPath("", false); err == nil {
		t.Fatal("Gemini 缺少模型名应当报错")
	}
}

func TestDecodeStreamEventSkipsNonContent(t *testing.T) {
	if _, ok, err := dialect.DecodeStreamEvent(dialect.OpenAIChat, []byte("[DONE]")); ok || err != nil {
		t.Fatalf("结束标记应被跳过，实际 ok=%v err=%v", ok, err)
	}
	if _, ok, err := dialect.DecodeStreamEvent(dialect.ClaudeMessages, []byte(`{"type":"ping"}`)); ok || err != nil {
		t.Fatalf("心跳应被跳过，实际 ok=%v err=%v", ok, err)
	}
	if _, ok, err := dialect.DecodeStreamEvent(dialect.OpenAIChat, nil); ok || err != nil {
		t.Fatalf("空负载应被跳过，实际 ok=%v err=%v", ok, err)
	}
	value, ok, err := dialect.DecodeStreamEvent(dialect.OpenAIChat,
		[]byte(`{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"x"}}]}`))
	if err != nil || !ok {
		t.Fatalf("正常分片应当被解析: ok=%v err=%v", ok, err)
	}
	if err := dialect.CheckPayload(dialect.OpenAIChat, value); err != nil {
		t.Fatalf("分片类型不符: %v", err)
	}
}

func TestModelOfAndStreamIntent(t *testing.T) {
	model, err := dialect.ModelOf(dialect.OpenAIChat, []byte(`{"model":"gpt-4"}`))
	if err != nil || model != "gpt-4" {
		t.Fatalf("取模型名失败: %q %v", model, err)
	}
	if _, missingErr := dialect.ModelOf(dialect.ClaudeMessages, []byte(`{}`)); missingErr == nil {
		t.Fatal("缺少 model 应当报错")
	}
	geminiModel, geminiErr := dialect.ModelOf(dialect.GeminiGenerate, []byte(`{}`))
	if geminiErr != nil || geminiModel != "" {
		t.Fatalf("Gemini 的模型名在路径上，应返回空串: %q %v", geminiModel, geminiErr)
	}
	stream, err := dialect.StreamIntent(dialect.OpenAIChat, []byte(`{"stream":true}`), false)
	if err != nil || !stream {
		t.Fatalf("应当识别出流式: %v %v", stream, err)
	}
	stream, err = dialect.StreamIntent(dialect.OpenAIChat, []byte(`{}`), true)
	if err != nil || !stream {
		t.Fatalf("请求体没写 stream 时应退回提示值: %v %v", stream, err)
	}
	stream, err = dialect.StreamIntent(dialect.GeminiGenerate, []byte(`{}`), true)
	if err != nil || !stream {
		t.Fatalf("Gemini 应直接用提示值: %v %v", stream, err)
	}
}

func TestEncodeStreamEventCarriesEventName(t *testing.T) {
	value, ok, err := dialect.DecodeStreamEvent(dialect.ClaudeMessages,
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`))
	if err != nil || !ok {
		t.Fatalf("解析失败: %v %v", ok, err)
	}
	event, err := dialect.EncodeStreamEvent(dialect.ClaudeMessages, value)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if event.Event != "content_block_delta" {
		t.Fatalf("事件名应来自 DTO 的 type 字段，实际 %q", event.Event)
	}
	if !json.Valid(event.Data) {
		t.Fatalf("事件负载不是合法 JSON: %s", event.Data)
	}
}

func TestValidateModelName(t *testing.T) {
	for _, good := range []string{"gemini-2.5-pro", "gemini-2.0-flash-exp", "models_x", "a.b", "A1"} {
		if err := dialect.ValidateModelName(good); err != nil {
			t.Fatalf("%q 应当合法: %v", good, err)
		}
	}
	for _, bad := range []string{"", "a/b", "a?b=1", "a#b", "a%2fb", "../x", "a b", "a:b", "..", strings.Repeat("x", 300)} {
		if err := dialect.ValidateModelName(bad); err == nil {
			t.Fatalf("%q 应当被拒绝", bad)
		}
	}
	if _, err := dialect.GeminiGenerate.UpstreamPath("../../x", false); err == nil {
		t.Fatal("写进路径的模型名必须先过校验")
	}
	if _, err := dialect.GeminiGenerate.UpstreamPath("gemini-2.5-pro", true); err != nil {
		t.Fatalf("合法模型名不应报错: %v", err)
	}
}

func TestCheckPayloadRejectsMismatch(t *testing.T) {
	request, err := dialect.DecodeRequest(dialect.OpenAIChat, []byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if err := dialect.CheckPayload(dialect.OpenAIChat, request); err != nil {
		t.Fatalf("类型应当匹配: %v", err)
	}
	if err := dialect.CheckPayload(dialect.ClaudeMessages, request); err == nil {
		t.Fatal("跨方言类型应当被判为不匹配")
	}
}
