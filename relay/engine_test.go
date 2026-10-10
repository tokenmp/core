package relay_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/relay"
)

// 每个方言一份最小可用的请求与响应样本，用于遍历转换矩阵。
var requestSamples = map[dialect.Name]string{
	dialect.OpenAIChat:      `{"model":"client-model","messages":[{"role":"user","content":"你好"}],"max_tokens":64}`,
	dialect.OpenAIResponses: `{"model":"client-model","input":"你好","max_output_tokens":64}`,
	dialect.ClaudeMessages:  `{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"你好"}]}`,
	dialect.GeminiGenerate:  `{"contents":[{"role":"user","parts":[{"text":"你好"}]}],"generationConfig":{"maxOutputTokens":64}}`,
}

var responseSamples = map[dialect.Name]string{
	dialect.OpenAIChat: `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream-model",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
	dialect.OpenAIResponses: `{"id":"resp_1","object":"response","created_at":1,"model":"upstream-model","status":"completed",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"你好","annotations":[]}]}],` +
		`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`,
	dialect.ClaudeMessages: `{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model",` +
		`"content":[{"type":"text","text":"你好"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`,
	dialect.GeminiGenerate: `{"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
}

// streamSamples 是各方言上游流的分片序列。
var streamSamples = map[dialect.Name][]string{
	dialect.OpenAIChat: {
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"upstream-model",` +
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"upstream-model",` +
			`"choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}]}`,
	},
	dialect.OpenAIResponses: {
		`{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"model":"upstream-model","status":"in_progress","output":[]}}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"你好"}`,
		`{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"model":"upstream-model","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`,
	},
	dialect.ClaudeMessages: {
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","content":[],"usage":{"input_tokens":3,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	},
	dialect.GeminiGenerate: {
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"你好"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`,
	},
}

// pairs 返回所有 from != to 的有序方言组合。
func pairs() [][2]dialect.Name {
	var result [][2]dialect.Name
	for _, from := range dialect.Names() {
		for _, to := range dialect.Names() {
			if from != to {
				result = append(result, [2]dialect.Name{from, to})
			}
		}
	}
	return result
}

func TestPrepareRequestConvertsEveryPair(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	for _, pair := range pairs() {
		from, to := pair[0], pair[1]
		t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
			prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
				From:          from,
				To:            to,
				Body:          []byte(requestSamples[from]),
				OriginModel:   "client-model",
				UpstreamModel: "upstream-model",
			})
			if err != nil {
				t.Fatalf("转换失败: %v", err)
			}
			if prepared.Passthrough {
				t.Fatal("不同方言不应走到透传")
			}
			if len(prepared.Body) == 0 {
				t.Fatal("转换结果为空")
			}
			if !json.Valid(prepared.Body) {
				t.Fatalf("转换结果不是合法 JSON: %s", prepared.Body)
			}
			decoded, err := dialect.DecodeRequest(to, prepared.Body)
			if err != nil {
				t.Fatalf("转换结果无法按目标方言解析: %v", err)
			}
			if err := dialect.CheckPayload(to, decoded); err != nil {
				t.Fatalf("转换结果类型不符: %v", err)
			}
			if prepared.Result == nil || prepared.Result.To != to.RelayFormat() {
				t.Fatalf("转换结果目标方言不符: %+v", prepared.Result)
			}
		})
	}
}

func TestPrepareResponseConvertsEveryPair(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	for _, pair := range pairs() {
		from, to := pair[0], pair[1]
		t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
			prepared, err := engine.PrepareResponse(context.Background(), relay.Response{
				From:          from,
				To:            to,
				Body:          []byte(responseSamples[from]),
				OriginModel:   "client-model",
				UpstreamModel: "upstream-model",
			})
			if err != nil {
				t.Fatalf("转换失败: %v", err)
			}
			if prepared.Passthrough {
				t.Fatal("不同方言不应走到透传")
			}
			if !json.Valid(prepared.Body) {
				t.Fatalf("转换结果不是合法 JSON: %s", prepared.Body)
			}
			decoded, err := dialect.DecodeResponse(to, prepared.Body)
			if err != nil {
				t.Fatalf("转换结果无法按目标方言解析: %v", err)
			}
			if err := dialect.CheckPayload(to, decoded); err != nil {
				t.Fatalf("转换结果类型不符: %v", err)
			}
			if prepared.Usage == nil {
				t.Fatal("转换结果缺少用量")
			}
		})
	}
}

func TestStreamConvertsEveryPair(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	ctx := context.Background()
	for _, pair := range pairs() {
		from, to := pair[0], pair[1]
		t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
			stream, err := engine.NewStream(from, to, "client-model", "upstream-model", relay.StreamOptions{
				Model:        "client-model",
				Created:      1,
				IncludeUsage: true,
			})
			if err != nil {
				t.Fatalf("创建会话失败: %v", err)
			}
			var produced int
			for _, chunk := range streamSamples[from] {
				events, convertErr := stream.Convert(ctx, []byte(chunk))
				if convertErr != nil {
					t.Fatalf("转换分片失败: %v\n分片: %s", convertErr, chunk)
				}
				for _, event := range events {
					if !json.Valid(event.Data) {
						t.Fatalf("事件负载不是合法 JSON: %s", event.Data)
					}
					produced++
				}
			}
			final, err := stream.Finalize(ctx)
			if err != nil {
				t.Fatalf("收尾失败: %v", err)
			}
			produced += len(final)
			if produced == 0 {
				t.Fatal("整个流没有产生任何事件")
			}
		})
	}
}

func TestSameDialectIsPassthrough(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	for _, name := range dialect.Names() {
		body := []byte(requestSamples[name])
		prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
			From: name, To: name, Body: body, OriginModel: "client-model",
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !prepared.Passthrough {
			t.Fatalf("%s: 同方言应当是透传", name)
		}
		if string(prepared.Body) != string(body) {
			t.Fatalf("%s: 透传改动了字节", name)
		}
		if _, err := engine.NewStream(name, name, "m", "m", relay.StreamOptions{}); err == nil {
			t.Fatalf("%s: 同方言不应创建转换会话", name)
		}
	}
}

func TestPrepareRequestDetectsStream(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.OpenAIChat,
		To:            dialect.ClaudeMessages,
		Body:          []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		OriginModel:   "m",
		UpstreamModel: "m",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !prepared.Stream {
		t.Fatal("请求体声明了 stream，应当被识别")
	}
	var converted struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(prepared.Body, &converted); err != nil {
		t.Fatalf("解析转换结果失败: %v", err)
	}
	if converted.Stream == nil || !*converted.Stream {
		t.Fatalf("Claude 目标请求应带 stream: true，实际 %s", prepared.Body)
	}
}

func TestClaudeTargetGetsDefaultMaxTokens(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.OpenAIChat,
		To:            dialect.ClaudeMessages,
		Body:          []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		OriginModel:   "m",
		UpstreamModel: "m",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	var converted struct {
		MaxTokens *uint `json:"max_tokens"`
	}
	if err := json.Unmarshal(prepared.Body, &converted); err != nil {
		t.Fatalf("解析转换结果失败: %v", err)
	}
	if converted.MaxTokens == nil {
		t.Fatalf("转 Claude 必须补 max_tokens，实际 %s", prepared.Body)
	}
	if *converted.MaxTokens != relay.DefaultClaudeMaxTokens {
		t.Fatalf("默认 max_tokens 应为 %d，实际 %d", relay.DefaultClaudeMaxTokens, *converted.MaxTokens)
	}
}

func TestModelSuffixParsing(t *testing.T) {
	withoutSuffix := relay.NewEngine(relay.Options{})
	if base, state := withoutSuffix.ParseModel("claude-sonnet-4-5-thinking"); base != "claude-sonnet-4-5-thinking" || state != nil {
		t.Fatalf("未打开尾缀识别时不应改动模型名: %q %+v", base, state)
	}
	withSuffix := relay.NewEngine(relay.Options{ModelSuffix: true})
	base, state := withSuffix.ParseModel("claude-sonnet-4-5-thinking")
	if base != "claude-sonnet-4-5" {
		t.Fatalf("基础模型名应为 claude-sonnet-4-5，实际 %q", base)
	}
	if state == nil {
		t.Fatal("应当解析出推理状态")
	}
}

func TestPreparedDiagnosticsCarrySteps(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.ClaudeMessages,
		To:            dialect.GeminiGenerate,
		Body:          []byte(requestSamples[dialect.ClaudeMessages]),
		OriginModel:   "client-model",
		UpstreamModel: "upstream-model",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if len(prepared.Result.Steps) == 0 {
		t.Fatal("转换结果应当带路径")
	}
	if prepared.Result.Quality == "" {
		t.Fatal("转换结果应当带质量等级")
	}
	if !strings.Contains(string(prepared.Result.From), "claude") {
		t.Fatalf("来源方言不符: %s", prepared.Result.From)
	}
}
