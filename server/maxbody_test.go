package server_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestConfiguredMaxBodyBytesRejectsOversizedRequest 覆盖 defaults.max_body_bytes
// 真的拦住超限请求：上限之内的照常转发，超过的以 413 拒绝且不碰上游。
func TestConfiguredMaxBodyBytesRejectsOversizedRequest(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front := newServer(t, `
defaults:
  max_body_bytes: 1024
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`, "")

	small := `{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`
	status, _, body := post(t, front.URL+"/v1/chat/completions", small)
	if status != http.StatusOK {
		t.Fatalf("上限之内应当放行，实际 %d %s", status, body)
	}
	if record.count() != 1 {
		t.Fatalf("上限之内的请求应当到达上游，实际 %d 次", record.count())
	}

	oversized := `{"model":"client-model","messages":[{"role":"user","content":"` +
		strings.Repeat("a", 2048) + `"}]}`
	status, _, body = post(t, front.URL+"/v1/chat/completions", oversized)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求应当 413，实际 %d %s", status, body)
	}
	if record.count() != 1 {
		t.Fatalf("超限请求不应到达上游，实际上游收到 %d 次", record.count())
	}
}
