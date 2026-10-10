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

// promptCacheKeyBody 造一份把会话标识放进请求体的报文。
func promptCacheKeyBody(session string) string {
	return `{"model":"client-model","prompt_cache_key":"` + session + `",` +
		`"messages":[{"role":"user","content":"你好"}]}`
}

// channelSpec 描述一个待启动的渠道。
type channelSpec struct {
	// name 是渠道名，也是响应里用来区分来源的标记。
	name string
	// status 非零且属于错误码时，该渠道按这个状态拒绝请求。
	status int
}

// newChannelServer 起一个服务，渠道由 specs 给出，模型规则由调用方给出。
//
// 每个渠道的响应里写着自己的名字，测试据此判断谁最终接手。看响应而不看渠道各自
// 的计数，是因为兜底时多个渠道都会收到请求，计数分不出谁把结果交给了客户端。
func newChannelServer(t *testing.T, specs []channelSpec, models string) (*httptest.Server, map[string]*upstream) {
	t.Helper()
	var channelYAML strings.Builder
	records := make(map[string]*upstream, len(specs))
	for _, spec := range specs {
		record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
			if spec.status >= http.StatusBadRequest {
				writeJSON(writer, spec.status, `{"error":{"message":"上游拒绝"}}`)
				return
			}
			writeJSON(writer, http.StatusOK, affinityResponse(spec.name))
		})
		records[spec.name] = record
		fmt.Fprintf(&channelYAML, "  - {name: %s, dialect: openai, base_url: \"%s\"}\n",
			spec.name, upstreamServer.URL)
	}
	front := newServer(t, "channels:\n"+channelYAML.String()+"models:\n"+models, "")
	return front, records
}

// newAffinityServer 起一个双渠道服务，两个渠道写同一个亲和组名。
func newAffinityServer(t *testing.T, firstStatus int, affinity bool) (*httptest.Server, *upstream, *upstream) {
	t.Helper()
	group := ""
	if affinity {
		group = ", affinity: pool"
	}
	front, records := newChannelServer(t,
		[]channelSpec{{name: "first", status: firstStatus}, {name: "second"}},
		"  - {match: \"client-model\", channel: first"+group+"}\n"+
			"  - {match: \"client-model\", channel: second"+group+"}\n")
	return front, records["first"], records["second"]
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

// served 发一次请求，返回最终接手的渠道名。请求必须成功。
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

// sessionFor 在一个正常服务的实例上，找一个会被分配到目标渠道的会话标识。
//
// 亲和用什么键分配是实现细节，测试不复制那套算法，直接试出来。只能用在所有渠道
// 都成功的服务上：渠道失败时这里会把降级误判成亲和命中。
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

	// 找一个靠请求头会落到 second 的会话标识，再把它放进请求体且不带请求头。
	sticky := sessionFor(t, front, "second")
	if got := served(t, front, "", promptCacheKeyBody(sticky)); got != "second" {
		t.Fatalf("请求体里的 prompt_cache_key 没有被当作会话标识，落到了 %s", got)
	}
	if again := served(t, front, "", promptCacheKeyBody(sticky)); again != "second" {
		t.Fatalf("靠 prompt_cache_key 分配的结果不稳定: %s", again)
	}
}

// TestAffinityHeaderBeatsBody 确认请求头与请求体都有标识时按请求头走。
//
// 两个来源的优先级是文档写明的行为，只有两个都带上才测得出。
func TestAffinityHeaderBeatsBody(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, true)
	toFirst := sessionFor(t, front, "first")
	toSecond := sessionFor(t, front, "second")

	if got := served(t, front, toFirst, promptCacheKeyBody(toSecond)); got != "first" {
		t.Fatalf("请求头应当优先于请求体，实际到了 %s", got)
	}
	if got := served(t, front, toSecond, promptCacheKeyBody(toFirst)); got != "second" {
		t.Fatalf("请求头应当优先于请求体，实际到了 %s", got)
	}
}

// TestAffinityDisabledKeepsConfigOrder 确认没写组名时配置顺序说了算。
func TestAffinityDisabledKeepsConfigOrder(t *testing.T) {
	front, _, _ := newAffinityServer(t, http.StatusOK, false)
	for index := range 20 {
		session := fmt.Sprintf("sess-%d", index)
		if got := served(t, front, session, affinityBody); got != "first" {
			t.Fatalf("没写组名时应当走第一条候选，实际到了 %s", got)
		}
	}
}

// TestAffinitySingleMemberPoolKeepsConfigOrder 确认池里只有一个成员时什么都不做。
//
// 只给一条规则写了组名时，保守行为是回到配置顺序，而不是把这一条永远排到最前。
func TestAffinitySingleMemberPoolKeepsConfigOrder(t *testing.T) {
	front, _ := newChannelServer(t,
		[]channelSpec{{name: "first"}, {name: "second"}},
		"  - {match: \"client-model\", channel: second, affinity: pool}\n"+
			"  - {match: \"client-model\", channel: first}\n")
	for index := range 20 {
		session := fmt.Sprintf("sess-%d", index)
		if got := served(t, front, session, affinityBody); got != "second" {
			t.Fatalf("池只有一个成员时应当保持配置顺序，实际到了 %s", got)
		}
	}
}

// TestAffinityPoolExcludesCatchAll 确认兜底规则不会被拉进池里当首选。
//
// Resolve 返回所有命中该模型名的规则，因此一条 match: "*" 的兜底规则也是候选。
// 它没有写组名，就必须留在池外，只作为降级路径。不修的话约三分之一的请求会主动
// 打到兜底渠道上。
func TestAffinityPoolExcludesCatchAll(t *testing.T) {
	front, _ := newChannelServer(t,
		[]channelSpec{{name: "first"}, {name: "second"}, {name: "catchall"}},
		"  - {match: \"client-model\", channel: first, affinity: pool}\n"+
			"  - {match: \"client-model\", channel: second, affinity: pool}\n"+
			"  - {match: \"*\", channel: catchall}\n")
	const total = 40
	primary := map[string]int{}
	for index := range total {
		primary[served(t, front, fmt.Sprintf("sess-%d", index), affinityBody)]++
	}
	if primary["catchall"] != 0 {
		t.Fatalf("兜底渠道被当作首选服务了 %d 次: %v", primary["catchall"], primary)
	}
	// 池里两条都要被用到，否则这个测试可能因为「总是走第一条」而假过。
	if primary["first"] == 0 || primary["second"] == 0 {
		t.Fatalf("池内没有分摊: %v", primary)
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
