package relay_test

import (
	"bytes"
	"context"
	"encoding/json"
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
			report := prepared.Report()
			if report.To != to || report.From != from {
				t.Fatalf("转换报告方言不符: %+v", report)
			}
			if report.Converter == "" {
				t.Fatalf("转换报告应当带转换器标识: %+v", report)
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

func TestSameDialectResponseKeepsBytesAndReportsUsage(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	cases := []struct {
		name      string
		dialect   dialect.Name
		body      string
		wantUsage bool
	}{
		{"openai with usage", dialect.OpenAIChat, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`, true},
		{"gemini with usageMetadata", dialect.GeminiGenerate, `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15}}`, true},
		{"openai without usage", dialect.OpenAIChat, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := engine.PrepareResponse(context.Background(), relay.Response{
				From: tc.dialect, To: tc.dialect, Body: []byte(tc.body), OriginModel: "m",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !prepared.Passthrough || string(prepared.Body) != tc.body {
				t.Fatalf("同方言响应应原样透传: passthrough=%v body=%s", prepared.Passthrough, prepared.Body)
			}
			if (prepared.Usage != nil) != tc.wantUsage {
				t.Fatalf("用量存在性不符: got %v, want %v", prepared.Usage != nil, tc.wantUsage)
			}
			if tc.wantUsage && (prepared.Usage.PromptTokens != 10 || prepared.Usage.CompletionTokens != 5) {
				t.Fatalf("用量不符: %+v", prepared.Usage)
			}
		})
	}
}

func TestSameDialectRewritesOnlyModelField(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	// 非规范 JSON：多余空白、未建模字段与 1e5 写法都要原样保留，
	// 只有 model 的值被定点替换。
	body := `{"model":"m1",  "messages":[{"role":"user","content":"hi"}], "unknown_field":{"a":1}, "unknown_num":1e5}`
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.OpenAIChat,
		To:            dialect.OpenAIChat,
		Body:          []byte(body),
		OriginModel:   "m1",
		UpstreamModel: "UPSTREAM/NAME",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !prepared.Passthrough {
		t.Fatal("同方言应当仍是透传")
	}
	want := `{"model":"UPSTREAM/NAME",  "messages":[{"role":"user","content":"hi"}], "unknown_field":{"a":1}, "unknown_num":1e5}`
	if string(prepared.Body) != want {
		t.Fatalf("只应改 model 一个字段:\n期望 %s\n实际 %s", want, prepared.Body)
	}
}

func TestSameDialectKeepsBytesWhenModelUnchanged(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	body := `{"model":"m1",  "messages":[], "unknown_num":1e5}`
	// 未写 upstream 时上层传入空串，名字相同时传入同一个名字，两条路径都不能动字节。
	for _, upstream := range []string{"", "m1"} {
		prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
			From: dialect.OpenAIChat, To: dialect.OpenAIChat, Body: []byte(body),
			OriginModel: "m1", UpstreamModel: upstream,
		})
		if err != nil {
			t.Fatalf("转换失败: %v", err)
		}
		if string(prepared.Body) != body {
			t.Fatalf("upstream=%q 时字节应完全不变，实际 %s", upstream, prepared.Body)
		}
	}
}

func TestGeminiTargetSkipsModelRewrite(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	// Gemini 的模型名在路径上，由 channel 拼，报文里不该出现上游名。
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From: dialect.OpenAIChat, To: dialect.GeminiGenerate,
		Body:        []byte(requestSamples[dialect.OpenAIChat]),
		OriginModel: "m1", UpstreamModel: "UPSTREAM-NAME",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if bytes.Contains(prepared.Body, []byte("UPSTREAM-NAME")) {
		t.Fatalf("Gemini 目标不应把上游名写进报文: %s", prepared.Body)
	}
}

func TestCrossDialectRewritesTopLevelModel(t *testing.T) {
	engine := relay.NewEngine(relay.Options{})
	// 其余三个方言的目标报文都在顶层有 model 字段，转换器自己用不用上游名都无所谓。
	for _, to := range []dialect.Name{dialect.OpenAIChat, dialect.OpenAIResponses, dialect.ClaudeMessages} {
		prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
			From: dialect.ClaudeMessages, To: to,
			Body:        []byte(requestSamples[dialect.ClaudeMessages]),
			OriginModel: "m1", UpstreamModel: "UPSTREAM-NAME",
		})
		if err != nil {
			t.Fatalf("%s: 转换失败: %v", to, err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(prepared.Body, &decoded); err != nil {
			t.Fatalf("%s: 解析失败: %v", to, err)
		}
		if decoded["model"] != "UPSTREAM-NAME" {
			t.Fatalf("%s: 顶层 model 应为上游名，实际 %v", to, decoded["model"])
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
	report := prepared.Report()
	if len(report.Steps) == 0 {
		t.Fatal("转换报告应当带路径")
	}
	if report.Quality == "" {
		t.Fatal("转换报告应当带质量等级")
	}
	if report.From != dialect.ClaudeMessages {
		t.Fatalf("来源方言不符: %s", report.From)
	}
	if report.Steps[0].From != dialect.ClaudeMessages {
		t.Fatalf("第一跳的来源方言不符: %+v", report.Steps[0])
	}
}
