package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokenmp/core/server"
)

// affinityBody 是客户端发来的请求体。
const affinityBody = `{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`

// newAffinityServer 起一个双渠道服务。
//
// 第一个渠道按 firstStatus 响应，第二个渠道一律成功。两个渠道的规则都命中同一个
// 模型名，因此它们互为候选。两个渠道的响应用 id 区分，测试据此判断谁最终接手。
func newAffinityServer(t *testing.T, firstStatus int, affinity bool) (*httptest.Server, *upstream, *upstream) {
	t.Helper()
	first, firstServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		if firstStatus >= http.StatusBadRequest {
			writeJSON(writer, firstStatus, `{"error":{"message":"上游拒绝"}}`)
			return
		}
		writeJSON(writer, http.StatusOK, affinityResponse("first"))
	})
	second, secondServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, affinityResponse("second"))
	})
	affinityLine := ""
	if affinity {
		affinityLine = ", affinity: true"
	}
	front := newServer(t, `
channels:
  - {name: first, dialect: openai, base_url: "`+firstServer.URL+`"}
  - {name: second, dialect: openai, base_url: "`+secondServer.URL+`"}
models:
  - {match: "client-model", channel: first`+affinityLine+`}
  - {match: "client-model", channel: second}
`, "")
	return front, first, second
}

// affinityResponse 造一份带渠道名的上游响应。
func affinityResponse(channel string) string {
	return `{"id":"chatcmpl-` + channel + `","object":"chat.completion","created":1,` +
		`"model":"upstream-model","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
}

// postWith 发一次请求，会话标识为空时不带请求头。返回状态码与响应体。
func postWith(t *testing.T, url, session, body string) (int, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if session != "" {
		request.Header.Set(server.SessionHeaderName, session)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return response.StatusCode, payload
}

// served 发一次请求，返回最终接手的渠道名。
//
// 读响应里的 id，不看渠道各自的计数：兜底时两个渠道都会收到请求，计数分不出谁
// 最终把结果交给了客户端。
func served(t *testing.T, front *httptest.Server, session, body string) string {
	t.Helper()
	status, payload := postWith(t, front.URL+"/v1/chat/completions", session, body)
	if status != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", status, payload)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, payload)
	}
	channel := strings.TrimPrefix(parsed.ID, "chatcmpl-")
	if channel == parsed.ID {
		t.Fatalf("响应里没有渠道标记: %s", payload)
	}
	return channel
}

// sessionFor 找一个会被分配到目标渠道的会话标识。
//
// 亲和用什么键分配是实现细节，测试不复制那套算法，直接试出来。
func sessionFor(t *testing.T, front *httptest.Server, want string) string {
	t.Helper()
	for index := range 200 {
		session := fmt.Sprintf("sess-%d", index)
		if served(t, front, session, affinityBody) == want {
			return session
		}
	}
	t.Fatalf("试了 200 个会话标识，没有一个落到 %s", want)
	return ""
}

// TestAffinityIsSticky 确认同一段会话的连续请求落到同一个渠道。
func TestAffinityIsSticky(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, true)
	sticky := sessionFor(t, front, "second")

	for range 5 {
		if got := served(t, front, sticky, affinityBody); got != "second" {
			t.Fatalf("会话 %s 应当一直粘在 second，实际到了 %s", sticky, got)
		}
	}
}

// TestAffinitySpreadsSessions 确认不同会话会被分到不同渠道。
//
// 这是亲和有意义的另一半：光粘住不分摊，等于把负载集中到一个渠道上。
func TestAffinitySpreadsSessions(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, true)
	const total = 60
	counts := map[string]int{}
	for index := range total {
		counts[served(t, front, fmt.Sprintf("sess-%d", index), affinityBody)]++
	}
	if counts["first"] == 0 || counts["second"] == 0 {
		t.Fatalf("%d 段会话没有分摊到两个渠道: %v", total, counts)
	}
	if counts["first"] < total/10 || counts["second"] < total/10 {
		t.Fatalf("分摊明显失衡: %v", counts)
	}
}

// TestAffinityWithoutSessionKeepsConfigOrder 确认客户端不给会话标识时行为不变。
func TestAffinityWithoutSessionKeepsConfigOrder(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, true)
	for range 5 {
		if got := served(t, front, "", affinityBody); got != "first" {
			t.Fatalf("没有会话标识时应当走配置里的第一条候选，实际到了 %s", got)
		}
	}
}

// TestAffinityUsesPromptCacheKeyAsFallback 确认客户端没给请求头时，用体里的
// prompt_cache_key 兜底。
func TestAffinityUsesPromptCacheKeyAsFallback(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, true)

	// 把会话标识塞进请求体，不带请求头。
	body := func(session string) string {
		return `{"model":"client-model","prompt_cache_key":"` + session + `",` +
			`"messages":[{"role":"user","content":"你好"}]}`
	}
	// 找一个靠请求头会落到 second 的会话标识，再把它放进请求体。
	sticky := sessionFor(t, front, "second")
	got := served(t, front, "", body(sticky))
	if got != "second" {
		t.Fatalf("请求体里的 prompt_cache_key 没有被当作会话标识，落到了 %s", got)
	}
	if again := served(t, front, "", body(sticky)); again != "second" {
		t.Fatalf("靠 prompt_cache_key 分配的结果不稳定: %s", again)
	}
}

// TestAffinityDisabledKeepsConfigOrder 确认没打开亲和时配置顺序说了算。
func TestAffinityDisabledKeepsConfigOrder(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, false)
	for index := range 20 {
		session := fmt.Sprintf("sess-%d", index)
		if got := served(t, front, session, affinityBody); got != "first" {
			t.Fatalf("亲和未打开时应当走第一条候选，实际到了 %s", got)
		}
	}
}

// TestAffinityStillFailsOver 确认粘住的渠道失败时后面的候选照旧兜底。
//
// 亲和是偏好而不是钉子。第一条渠道返回 429 时，请求必须换到第二条。
func TestAffinityStillFailsOver(t *testing.T) {
	// 先在第一条渠道正常工作的服务上找出会被粘到 first 的会话标识。
	healthy, _, _ := newAffinityServer(t, http.StatusOK, true)
	stickyToFirst := sessionFor(t, healthy, "first")

	front, first, second := newAffinityServer(t, http.StatusTooManyRequests, true)
	if got := served(t, front, stickyToFirst, affinityBody); got != "second" {
		t.Fatalf("first 返回 429 时应当换到 second，实际到了 %s", got)
	}
	if first.count() != 1 {
		t.Fatalf("first 应当被尝试一次，实际 %d 次", first.count())
	}
	if second.count() != 1 {
		t.Fatalf("second 应当收到一次兜底请求，实际 %d 次", second.count())
	}
}
