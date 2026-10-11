package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/relay"
)

// behaviorChannel 是一份最小的可用配置，用来单独观察 defaults 段的效果。
const behaviorChannel = `
channels:
  - {name: upstream, dialect: openai, base_url: "http://127.0.0.1:8000"}
models:
  - {match: "client-model", channel: upstream}
`

// newBehaviorServer 按 YAML 装配一个服务。
func newBehaviorServer(t *testing.T, yaml string) *Server {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	srv, err := FromConfig(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("装配服务失败: %v", err)
	}
	return srv
}

// engineMaxTokens 让内核转一次不带 max_tokens 的 OpenAI 请求到 Claude，读出补上的值。
func engineMaxTokens(t *testing.T, engine *relay.Engine) uint {
	t.Helper()
	prepared, err := engine.PrepareRequest(context.Background(), relay.Request{
		From:          dialect.OpenAIChat,
		To:            dialect.ClaudeMessages,
		Body:          []byte(`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`),
		OriginModel:   "client-model",
		UpstreamModel: "client-model",
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
	return *converted.MaxTokens
}

// TestUnsetBehaviorDefaultsKeepPreviousBehavior 是对照：不写 defaults 段时，装配出
// 的请求体上限与内核补的 max_tokens 都等于内置默认，与没有这两个配置键时逐条相同。
func TestUnsetBehaviorDefaultsKeepPreviousBehavior(t *testing.T) {
	srv := newBehaviorServer(t, behaviorChannel)
	if srv.maxBodyBytes != config.DefaultMaxBodyBytes {
		t.Fatalf("未写 defaults 时上限应为 %d，实际 %d", config.DefaultMaxBodyBytes, srv.maxBodyBytes)
	}
	if got := engineMaxTokens(t, srv.engine); got != relay.DefaultClaudeMaxTokens {
		t.Fatalf("未写 defaults 时应补 %d，实际 %d", relay.DefaultClaudeMaxTokens, got)
	}
}

// TestBehaviorDefaultsReachRuntime 覆盖两个键真的从配置走到运行期。
func TestBehaviorDefaultsReachRuntime(t *testing.T) {
	srv := newBehaviorServer(t, `
defaults:
  max_body_bytes: 1048576
  default_max_tokens: 7777
`+behaviorChannel)
	if srv.maxBodyBytes != 1048576 {
		t.Fatalf("上限应来自 defaults.max_body_bytes，实际 %d", srv.maxBodyBytes)
	}
	if got := engineMaxTokens(t, srv.engine); got != 7777 {
		t.Fatalf("补的 max_tokens 应来自 defaults.default_max_tokens，实际 %d", got)
	}
}
