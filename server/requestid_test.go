package server_test

import (
	"encoding/base32"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
)

// decodeRequestID 把 request_id 解回三段字节。
//
// 形状是 [时间前缀 4 字节][进程内计数 4 字节][随机段 12 字节]，共 20 字节，
// 见 server/requestid.go。这里自己解一遍而不是从 server 包里取常量：
// 用例要断言的是「落在线上和日志里的那个串」的形状，跟着实现取常量会让
// 常量改了用例也跟着改，等于没断言。
func decodeRequestID(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(id)
	if err != nil {
		t.Fatalf("request_id 不是合法 base32: %q: %v", id, err)
	}
	if len(raw) != 20 {
		t.Fatalf("request_id 解码出 %d 字节，想要 20: %q", len(raw), id)
	}
	return raw
}

// TestRequestIDDiffersAcrossServers 覆盖 #49 报的那条路径：request_id 曾经是纯进程内
// 自增计数器，重启之后从头计数，新产生的 id 与库里既有行撞车。下游把它当全局幂等键
// （同一批重复提交靠它去重），撞车之后那几条会被整批当成重发丢掉——调用照常返回成功，
// 记录、账单与扣费却都不发生，而且不报错。
//
// 这条用例考的是最坏情况：两个实例落在**同一个时间格**里。时间前缀与进程内计数都相同
// 时，两个 id 还不同就只能靠随机段——那正是唯一性该落的地方。要是用例不小心构造出
// 跨格的一对，「前缀不同的两个 id 本来就注定不同」，这条用例当场失去它要证的东西，
// 而且没有任何信号。所以这里不是「几乎必然同格」，而是循环重试到同格为止；
// 次数用尽仍构造不出来才是真的失败，并在信息里说明是没构造出最坏情况。
func TestRequestIDDiffersAcrossServers(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	routes := `
channels:
  - {name: upstream, dialect: openai, base_url: "` + upstreamServer.URL + `"}
models:
  - {match: "client-model", channel: upstream}
`

	// 每次尝试都在新的两个 Server 上各发一次请求。两个 Server 构造相隔微秒级，
	// 因此几乎总在同一个 4 秒格内；跨格时（约每 7 万次一次）重来一遍。
	const attempts = 64
	for attempt := 1; attempt <= attempts; attempt++ {
		first, firstOutcomes := newOutcomeServer(t, routes)
		second, secondOutcomes := newOutcomeServer(t, routes)

		post(t, first.URL+"/v1/chat/completions",
			`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
		post(t, second.URL+"/v1/chat/completions",
			`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)

		firstID := firstOutcomes.wait(t, 1)[0].RequestID
		secondID := secondOutcomes.wait(t, 1)[0].RequestID
		if firstID == "" || secondID == "" {
			t.Fatalf("request_id 不能为空: %q / %q", firstID, secondID)
		}
		firstRaw := decodeRequestID(t, firstID)
		secondRaw := decodeRequestID(t, secondID)

		firstPrefix := binary.BigEndian.Uint32(firstRaw[:4])
		secondPrefix := binary.BigEndian.Uint32(secondRaw[:4])
		if firstPrefix != secondPrefix {
			// 跨了时间格，这一对证明不了什么，重来。
			continue
		}

		if firstID == secondID {
			t.Fatalf("两个实例在同一时间格、同为第 1 次请求时发出了同一个 request_id: %s", firstID)
		}
		// 计数段：两个实例各自的第一条请求，都应当是 1。这条是形状约定，
		// 之前没有断言，顺手补上——计数段要是全局共享或从 0 起，这里会红。
		for _, item := range []struct {
			who string
			raw []byte
		}{{"第一个实例", firstRaw}, {"第二个实例", secondRaw}} {
			if got := binary.BigEndian.Uint32(item.raw[4:8]); got != 1 {
				t.Fatalf("%s 的第一条请求，计数段应当是 1，实得 %d（id %s）",
					item.who, got, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(item.raw))
			}
		}
		return
	}
	t.Fatalf("试了 %d 次都没能让两个实例落在同一个时间格内，因此没能构造出最坏情况", attempts)
}

// TestRequestIDShapeIsSafeToStore 覆盖 id 的形状：长度上界与字符集。
//
// 长度上界这条是下游的列宽约束（`request_id` 是 VARCHAR(64) 一类的列），
// 字符集这条是为了不必转义：id 要写进 JSON 日志行，也可能被下游拼进 SQL 字面量，
// 出现引号、反斜杠或控制字符就会逼出一个转义口径。
// 只允许 base32 的 `A-Z` 与 `2-7`，这个判断同时顺手挡掉了十进制计数器那类退回。
func TestRequestIDShapeIsSafeToStore(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	const count = 50
	for index := 0; index < count; index++ {
		post(t, front.URL+"/v1/chat/completions",
			`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	}
	for _, outcome := range outcomes.wait(t, count) {
		if len(outcome.RequestID) > 64 {
			t.Fatalf("request_id 超出列宽 64: %q（%d 字节）",
				outcome.RequestID, len(outcome.RequestID))
		}
		for _, char := range outcome.RequestID {
			isUpper := char >= 'A' && char <= 'Z'
			isDigit := char >= '2' && char <= '7'
			if !isUpper && !isDigit {
				t.Fatalf("request_id 含 base32 字符集之外的字符 %q: %s", char, outcome.RequestID)
			}
		}
	}
}

// TestRequestIDLengthIsFixed 把长度固定成一个定值，而不是只压一个上界。
//
// 定长是刻意的：id 会落进下游按位比较、按前缀归档的地方，长度随时间或计数增长
// 会让那些地方跟着变（比如按字节切片取前几位）。
//
// 这条用例管的是「上界」那条断言管不到的一类退回：把随机段缩到 4 字节，
// id 仍然全是合法 base32 字符、也仍然短于 64，上面那条完全绿，只有这条会红
// （实测：`应当恒为 32 个字符，实得 20`）。两条都留着，各自挡一面。
func TestRequestIDLengthIsFixed(t *testing.T) {
	_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, openAIChatResponse)
	})
	front, outcomes := newOutcomeServer(t, `
channels:
  - {name: upstream, dialect: openai, base_url: "`+upstreamServer.URL+`"}
models:
  - {match: "client-model", channel: upstream}
`)

	const count = 8
	for index := 0; index < count; index++ {
		post(t, front.URL+"/v1/chat/completions",
			`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	}
	got := outcomes.wait(t, count)
	// 20 字节按 base32 编码恰好 32 个字符：160 位是 5 的整数倍，没有余数补位。
	const want = 32
	for _, outcome := range got {
		if len(outcome.RequestID) != want {
			t.Fatalf("request_id 应当恒为 %d 个字符，实得 %d: %q",
				want, len(outcome.RequestID), outcome.RequestID)
		}
	}
	if strings.Contains(got[0].RequestID, "-") {
		t.Fatalf("request_id 不应当带填充或分隔符: %s", got[0].RequestID)
	}
}
