package server_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/server"
)

// countTokensBody 是一份计数端点的请求体：与 Messages 同形，只是没有 max_tokens。
const countTokensBody = `{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`

// countTokensResponse 是上游计数端点的响应形状。
//
//nolint:gosec // 上游响应里那个字段名是 token 计数，不是凭据。
const countTokensResponse = `{"input_tokens":42}`

// claudeYAML 是一条 claude 渠道加一条规则的最小配置。
func claudeYAML(upstreamURL string) string {
	return `
channels:
  - {name: claude-up, dialect: claude, base_url: "` + upstreamURL + `"}
models:
  - {match: "client-model", channel: claude-up}
`
}

// TestCountTokensForwardsToItsOwnPath 是这条端点的正路：请求打到上游的计数路径，
// 报文逐字节不变，上游的响应原样回来。
//
// 断言上游路径而不只是状态码：复用 Messages 那条管线时状态码一样是 200，
// 区别只在打到了哪个上游端点上。
func TestCountTokensForwardsToItsOwnPath(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, countTokensResponse)
	})
	front, _, _ := newSeamServer(t, claudeYAML(upstreamServer.URL))

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if string(body) != countTokensResponse {
		t.Fatalf("上游响应应当原样返回: %s", body)
	}
	if got := record.last(t).Path; got != dialect.ClaudeCountTokensPath {
		t.Fatalf("上游路径应当是计数端点自己的路径，实际 %s", got)
	}
	if got := string(record.last(t).Body); got != countTokensBody {
		t.Fatalf("同方言请求体应当逐字节不变:\n发出 %s\n收到 %s", countTokensBody, got)
	}
	if count := record.count(); count != 1 {
		t.Fatalf("只该打一次上游，实际 %d 次", count)
	}
}

// TestCountTokensOutcomeKeepsItsOwnPath 钉住交给宿主的 Path 是计数端点自己的路径。
//
// 这一条是跨仓库的隐式契约，不是内部细节：下游的记录层按入口路径的字面量分流，
// 达到 Messages 入口的那份记录会带着一份没有用量字段的响应体进去，于是留下一行
// total_tokens 为空的脏记录，全程不报错。Usage 为 nil 这个断言抓不住它 —— 那种
// 情况下 Usage 一样是 nil，用例照样绿，因此必须单独断言 Path。
func TestCountTokensOutcomeKeepsItsOwnPath(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, countTokensResponse)
	})
	front, outcomes := newOutcomeServer(t, claudeYAML(upstreamServer.URL))

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Path != dialect.ClaudeCountTokensPath {
		t.Fatalf("Outcome.Path 应当是计数端点的完整路径，落回 %q 会让记录层把它当成一次消息调用",
			outcome.Path)
	}
	if outcome.Method != http.MethodPost {
		t.Fatalf("Outcome.Method 应当是 POST，实际 %q", outcome.Method)
	}
	if outcome.Model != "client-model" {
		t.Fatalf("Outcome.Model 应当是请求体里的模型名，实际 %q", outcome.Model)
	}
	if outcome.Streaming {
		t.Fatal("计数端点不是流式的")
	}
	if outcome.Usage != nil {
		t.Fatalf("计数响应没有用量字段，Usage 应当为 nil: %+v", outcome.Usage)
	}
	if outcome.Status != http.StatusOK {
		t.Fatalf("Outcome.Status 不符: %d", outcome.Status)
	}
	if len(outcome.Attempts) != 1 || outcome.Attempts[0].Dialect != dialect.ClaudeMessages {
		t.Fatalf("应当只有一条 claude 候选的尝试: %+v", outcome.Attempts)
	}
}

// TestCountTokensWithoutClaudeCandidateIs501 覆盖候选里没有 claude 渠道的情形。
//
// 回 501 而不是 404：模型是配了的，缺的是这个入口要的那种渠道。也不换成别的方言
// 去凑一个数出来 —— 别的方言的分词器算的不是这个模型。客户端对计数端点的 501
// 有自己的降级路径，因此这里不会让它报错退出。
func TestCountTokensWithoutClaudeCandidateIs501(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, seamYAML(upstreamServer.URL))

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("没有 claude 候选时应当 501，实际 %d %s", status, body)
	}
	if !strings.Contains(string(body), "not_supported") {
		t.Fatalf("错误形状应当让客户端认得出这是不支持，而不是请求写错了: %s", body)
	}
	if count := record.count(); count != 0 {
		t.Fatalf("不该打上游，实际打了 %d 次", count)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Path != dialect.ClaudeCountTokensPath || outcome.Status != http.StatusNotImplemented {
		t.Fatalf("失败的计数请求也要按自己的路径记账: %+v", outcome)
	}
}

// TestCountTokensSkipsOtherDialectCandidates 覆盖同一模型下混着别的方言候选：
// 只有 claude 那条会收到请求，排在它前面的 openai 候选一次都不试。
func TestCountTokensSkipsOtherDialectCandidates(t *testing.T) {
	openAIRecord, openAIServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	claudeRecord, claudeServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, countTokensResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: openai-up, dialect: openai, base_url: "`+openAIServer.URL+`"}
  - {name: claude-up, dialect: claude, base_url: "`+claudeServer.URL+`"}
models:
  - {match: "client-model", channel: openai-up}
  - {match: "client-model", channel: claude-up}
`)

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if count := openAIRecord.count(); count != 0 {
		t.Fatalf("别的方言的候选不该被尝试，实际打了 %d 次", count)
	}
	if got := claudeRecord.last(t).Path; got != dialect.ClaudeCountTokensPath {
		t.Fatalf("上游路径不符: %s", got)
	}
	outcome := outcomes.wait(t, 1)[0]
	if outcome.Channel != "claude-up" || len(outcome.Attempts) != 1 {
		t.Fatalf("应当只试 claude 候选一条: %+v", outcome)
	}
}

// TestCountTokensErrorIsPassedThrough 覆盖上游拒绝：状态码与错误体原样透传，
// 不在网关这一层翻译。
func TestCountTokensErrorIsPassedThrough(t *testing.T) {
	const upstreamError = `{"type":"error","error":{"type":"invalid_request_error","message":"messages: 至少一条"}}`
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusBadRequest, upstreamError)
	})
	front, _, _ := newSeamServer(t, claudeYAML(upstreamServer.URL))

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("上游状态码应当透传，实际 %d %s", status, body)
	}
	if string(body) != upstreamError {
		t.Fatalf("上游错误体应当原样返回: %s", body)
	}
}

// TestCountTokensFollowsChannelPathOverride 定下路径后缀与渠道 path 的关系：
// 后缀追加在渠道给出的路径之后，而不是回到方言默认的那一条。
//
// 渠道把消息路径改写过了（例如基址自带了 /api/v1），它的计数端点就在改写后的
// 路径之后，拼回方言默认值会打到一个根本不存在的地址上。
func TestCountTokensFollowsChannelPathOverride(t *testing.T) {
	record, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, countTokensResponse)
	})
	front, _, _ := newSeamServer(t, `
channels:
  - name: claude-up
    dialect: claude
    base_url: "`+upstreamServer.URL+`/api/v1"
    path: /messages
models:
  - {match: "client-model", channel: claude-up}
`)

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody, nil)
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if got := record.last(t).Path; got != "/api/v1/messages/count_tokens" {
		t.Fatalf("后缀应当追加在渠道 path 之后，实际 %s", got)
	}
}

// TestCountTokensRejectsGet 确认这条端点与其余端点一样只接受 POST。
func TestCountTokensRejectsGet(t *testing.T) {
	front, _, _ := newSeamServer(t, `
channels:
  - {name: claude-up, dialect: claude, base_url: "https://example.com"}
models:
  - {match: "client-model", channel: claude-up}
`)

	response, err := get(t, front.URL+dialect.ClaudeCountTokensPath)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("应当 405，实际 %d", response.StatusCode)
	}
}

// TestCountTokensIgnoresAffinity 确认计数请求不按会话重排候选。
//
// 两条 claude 候选写了同一个亲和组名，先试出一个会被亲和分到 second 的会话标识，
// 再用它发计数请求：候选顺序应当仍是配置里的第一条。会话标识必须先试出来，
// 随便挑一个的话，它恰好被分到 first 时这条用例对「有没有走亲和」什么也没说。
func TestCountTokensIgnoresAffinity(t *testing.T) {
	firstRecord, firstServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, countTokensResponse)
	})
	secondRecord, secondServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, countTokensResponse)
	})
	front, _, _ := newSeamServer(t, `
channels:
  - {name: first, dialect: claude, base_url: "`+firstServer.URL+`"}
  - {name: second, dialect: claude, base_url: "`+secondServer.URL+`"}
models:
  - {match: "client-model", channel: first, affinity: pool}
  - {match: "client-model", channel: second, affinity: pool}
`)
	session := sessionGoingTo(t, front, firstRecord, secondRecord, "second")
	firstBefore, secondBefore := firstRecord.count(), secondRecord.count()

	status, body := postWithHeaders(t, front.URL+dialect.ClaudeCountTokensPath, countTokensBody,
		map[string]string{server.SessionHeaderName: session})
	if status != http.StatusOK {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	// 只看这次请求带来的增量：试会话标识时两个渠道都已经收过请求。
	if got := firstRecord.count() - firstBefore; got != 1 {
		t.Fatalf("计数请求应当走配置里的第一条候选，first 这次收到 %d 次", got)
	}
	if got := secondRecord.count() - secondBefore; got != 0 {
		t.Fatalf("计数请求不该被亲和改道，second 这次收到 %d 次", got)
	}
}

// sessionGoingTo 试出一个会被亲和分到 want 渠道的会话标识。
//
// 亲和的分配算法是实现细节，测试不复制那套算法，直接在消息端点上试出来：
// 与计数端点共用一套亲和安排，因此试出来的标识对两者都成立。
func sessionGoingTo(t *testing.T, front *httptest.Server, first, second *upstream, want string) string {
	t.Helper()
	target := second
	if want == "first" {
		target = first
	}
	for index := range 200 {
		session := fmt.Sprintf("sess-%d", index)
		before := target.count()
		status, body := postWithHeaders(t, front.URL+dialect.ClaudeMessages.LocalPath(), countTokensBody,
			map[string]string{server.SessionHeaderName: session})
		if status != http.StatusOK {
			t.Fatalf("试探会话标识时状态码不符: %d %s", status, body)
		}
		if target.count() > before {
			return session
		}
	}
	t.Fatalf("试了 200 个会话标识，没有一个落到 %s", want)
	return ""
}
