package relay_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/relay"
)

// TestMediaResolverIsReachableThroughCore 确认调用方只靠 core 就能注册媒体解析能力，
// 不必自己去依赖 RelayKit。
//
// SetMediaResolver 是进程级设置，因此这个测试不与其他测试并行。
func TestMediaResolverIsReachableThroughCore(t *testing.T) {
	var got relay.MediaSource
	relay.SetMediaResolver(relay.MediaResolver{
		GetBase64Data: func(_ context.Context, source relay.MediaSource) (string, string, error) {
			got = source
			return "aGVsbG8=", "image/png", nil
		},
	})

	engine := relay.NewEngine(relay.Options{})
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.OpenAIChat,
		To:            dialect.ClaudeMessages,
		Body:          []byte(imageRequest),
		OriginModel:   "client-model",
		UpstreamModel: "upstream-model",
	})
	if err != nil {
		t.Fatalf("转换失败: %v", err)
	}
	if !got.IsURL() {
		t.Fatalf("图片应当以 URL 形式交给宿主，实际 %+v", got)
	}
	if got.URL != "https://example.com/a.png" {
		t.Fatalf("来源地址不符: %q", got.URL)
	}
	if !strings.Contains(string(prepared.Body), "aGVsbG8=") {
		t.Fatalf("宿主给出的内容没有被写进目标请求: %s", prepared.Body)
	}
}

// TestMediaResolverErrorIsReported 确认宿主回调报错时转换明确失败，
// 而不是静默丢掉图片。
func TestMediaResolverErrorIsReported(t *testing.T) {
	relay.SetMediaResolver(relay.MediaResolver{
		GetBase64Data: func(context.Context, relay.MediaSource) (string, string, error) {
			return "", "", errors.New("取图失败")
		},
	})

	engine := relay.NewEngine(relay.Options{})
	_, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.OpenAIChat,
		To:            dialect.ClaudeMessages,
		Body:          []byte(imageRequest),
		OriginModel:   "client-model",
		UpstreamModel: "upstream-model",
	})
	if err == nil {
		t.Fatal("宿主回调报错时转换应当失败")
	}
}

const imageRequest = `{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":[` +
	`{"type":"text","text":"这是什么"},` +
	`{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`
