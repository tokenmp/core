package relay_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/relay"
)

// 上游的缓存提示有三种：OpenAI 的 prompt_cache_key、Claude 的 cache_control、
// Gemini 的 cachedContent。这里把「跨方言转换后还剩什么」实测一遍并钉住。
//
// 这张表是 RelayKit 的行为，不是本模块的契约。RelayKit 升级后表变了要当回事：
// 要么是预期内的改进，要么是它的回归。改动转换行为后这张表必须仍然全绿。
func TestCacheHintSurvival(t *testing.T) {
	// 每种方言一份带满缓存提示的请求。
	bodies := map[dialect.Name]string{
		dialect.OpenAIChat: `{"model":"m","max_tokens":64,"prompt_cache_key":"sess-chat",
			"messages":[{"role":"system","content":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}]},
			{"role":"user","content":"你好"}]}`,
		dialect.OpenAIResponses: `{"model":"m","prompt_cache_key":"sess-resp",
			"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"sys"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]}]}`,
		dialect.ClaudeMessages: `{"model":"m","max_tokens":64,"cache_control":{"type":"ephemeral"},
			"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],
			"messages":[{"role":"user","content":[{"type":"text","text":"你好","cache_control":{"type":"ephemeral"}}]}]}`,
		dialect.GeminiGenerate: `{"contents":[{"role":"user","parts":[{"text":"你好"}]}],
			"systemInstruction":{"parts":[{"text":"sys"}]},"cachedContent":"cachedContents/abc"}`,
	}

	tests := []struct {
		from, to       dialect.Name
		promptCacheKey bool
		cacheControl   bool
		cachedContent  bool
		why            string
	}{
		{
			from: dialect.OpenAIChat, to: dialect.OpenAIResponses,
			promptCacheKey: true,
			why:            "两个 OpenAI 方言之间有专门的字段搬运",
		},
		{
			from: dialect.OpenAIResponses, to: dialect.OpenAIChat,
			promptCacheKey: true,
			why:            "同上，反方向也有",
		},
		{
			from: dialect.ClaudeMessages, to: dialect.OpenAIChat,
			cacheControl: true,
			why:          "只有正文块上的那一处被带过去，顶层自动缓存与 system 块上的会被丢掉",
		},
		{
			from: dialect.OpenAIChat, to: dialect.ClaudeMessages,
			why: "Claude 上游不认 prompt_cache_key，转换也不补 cache_control",
		},
		{
			from: dialect.OpenAIChat, to: dialect.GeminiGenerate,
			why: "Gemini 用 cachedContent 引用显式缓存，两种提示对它都没有意义",
		},
		{
			from: dialect.OpenAIResponses, to: dialect.ClaudeMessages,
			why: "两个方向都不认对方的缓存字段",
		},
		{
			from: dialect.OpenAIResponses, to: dialect.GeminiGenerate,
			why: "同上",
		},
		{
			from: dialect.ClaudeMessages, to: dialect.OpenAIResponses,
			why: "顶层 cache_control 没有对应的目标字段",
		},
		{
			from: dialect.ClaudeMessages, to: dialect.GeminiGenerate,
			why: "同上",
		},
		{
			from: dialect.GeminiGenerate, to: dialect.OpenAIChat,
			why: "cachedContent 是服务端缓存资源的引用，没有等价物",
		},
		{
			from: dialect.GeminiGenerate, to: dialect.OpenAIResponses,
			why: "同上",
		},
		{
			from: dialect.GeminiGenerate, to: dialect.ClaudeMessages,
			why: "同上",
		},
	}

	engine := relay.NewEngine(relay.Options{})
	for _, test := range tests {
		name := string(test.from) + "_to_" + string(test.to)
		t.Run(name, func(t *testing.T) {
			prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
				From:          test.from,
				To:            test.to,
				Body:          []byte(bodies[test.from]),
				OriginModel:   "m",
				UpstreamModel: "m",
			})
			if err != nil {
				t.Fatalf("转换失败: %v", err)
			}
			var parsed any
			if err := json.Unmarshal(prepared.Body, &parsed); err != nil {
				t.Fatalf("目标报文不是 JSON: %v\n%s", err, prepared.Body)
			}
			assertHintCount(t, parsed, "prompt_cache_key", test.promptCacheKey, test.why)
			assertHintCount(t, parsed, "cache_control", test.cacheControl, test.why)
			assertHintCount(t, parsed, "cachedContent", test.cachedContent, test.why)
		})
	}
}

// assertHintCount 断言某个键在目标报文里出现或完全不出现。
func assertHintCount(t *testing.T, parsed any, key string, want bool, why string) {
	t.Helper()
	count := countKeyOccurrences(parsed, key)
	if want && count == 0 {
		t.Fatalf("%s 应当保留，实际被丢弃（%s）", key, why)
	}
	if !want && count != 0 {
		t.Fatalf("%s 不应当出现，实际出现 %d 次（%s）", key, count, why)
	}
}

// countKeyOccurrences 数一个键在报文里出现了几次。
func countKeyOccurrences(value any, key string) int {
	switch typed := value.(type) {
	case map[string]any:
		count := 0
		for name, item := range typed {
			if name == key {
				count++
			}
			count += countKeyOccurrences(item, key)
		}
		return count
	case []any:
		count := 0
		for _, item := range typed {
			count += countKeyOccurrences(item, key)
		}
		return count
	default:
		return 0
	}
}
