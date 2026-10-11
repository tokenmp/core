package server_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tokenmp/core/channel"
	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
)

// retryYAML 是一条渠道加一条规则的最小配置，max_retries 可调。
func retryYAML(upstreamURL string, maxRetries int) string {
	return fmt.Sprintf(`
channels:
  - {name: upstream, dialect: openai, base_url: %q, max_retries: %d}
models:
  - {match: "client-model", channel: upstream}
`, upstreamURL, maxRetries)
}

// TestChannelRetriesAreRecorded 覆盖 issue #25 的核心：上游连续 429 时，
// 渠道内的每次重试都记一行 DEBUG，次数用尽再记一行，Attempt.Retries 与上游
// 实际收到的请求数对得上，而路由级的 attempts 仍是候选条数。
func TestChannelRetriesAreRecorded(t *testing.T) {
	upstreamRecord, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	})
	front, logs, outcomes := newSeamServer(t, retryYAML(upstreamServer.URL, 3))

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("上游连续 429 时应当原样返回，实际 %d %s", status, body)
	}
	if upstreamRecord.count() != 4 {
		t.Fatalf("max_retries: 3 时上游应收到 4 次请求，实际 %d", upstreamRecord.count())
	}

	lines := waitForLog(t, logs, "请求完成", 1)
	retries := 0
	for _, line := range lines {
		if line["msg"] != "渠道内重试" {
			continue
		}
		retries++
		assertField(t, line, "level", "DEBUG")
		if got := numberField(t, line, "retry"); got != float64(retries) {
			t.Fatalf("第 %d 行的 retry 不符: %v（%v）", retries, got, line)
		}
		if got := numberField(t, line, "status"); got != http.StatusTooManyRequests {
			t.Fatalf("重试行的 status 不符: %v（%v）", got, line)
		}
		if _, ok := line["wait"]; !ok {
			t.Fatalf("重试行缺少 wait 字段: %v", line)
		}
	}
	if retries != 3 {
		t.Fatalf("应当有 3 行渠道内重试，实际 %d", retries)
	}

	exhausted := lineWith(t, lines, "msg", "渠道内重试次数已用尽")
	assertField(t, exhausted, "level", "DEBUG")
	if got := numberField(t, exhausted, "status"); got != http.StatusTooManyRequests {
		t.Fatalf("用尽行的 status 不符: %v", got)
	}
	if got := numberField(t, exhausted, "max_retries"); got != 3 {
		t.Fatalf("用尽行的 max_retries 不符: %v", got)
	}

	// attempts 是路由级计数，一条候选内部重试 3 次仍然是 1。
	done := onlyLine(t, lines, "请求完成")
	if got := numberField(t, done, "attempts"); got != 1 {
		t.Fatalf("attempts 应当是候选条数 1: %v", got)
	}

	outcome := outcomes.wait(t, 1)[0]
	if len(outcome.Attempts) != 1 {
		t.Fatalf("应当只有一条候选尝试: %+v", outcome.Attempts)
	}
	if got := outcome.Attempts[0].Retries; got != 3 {
		t.Fatalf("Attempt.Retries 不符: %d，期望 3（%+v）", got, outcome.Attempts[0])
	}
}

// TestRetryAfterOverLimitLogsWarning 覆盖 issue #27 的上限口径接线：上游 429 带的
// Retry-After 超过渠道 backoff.max_retry_after 时只发一次请求，记一行 WARN，
// 不缩短成上限再重试，随后交给候选切换。
func TestRetryAfterOverLimitLogsWarning(t *testing.T) {
	upstreamRecord, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Retry-After", "600")
		writeJSON(writer, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	})
	front, logs, _ := newSeamServer(t, fmt.Sprintf(`
channels:
  - {name: upstream, dialect: openai, base_url: %q, max_retries: 3, backoff: {max_retry_after: 5s}}
models:
  - {match: "client-model", channel: upstream}
`, upstreamServer.URL))

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("超限时应当原样返回 429，实际 %d %s", status, body)
	}
	if upstreamRecord.count() != 1 {
		t.Fatalf("超限时上游应当只收到 1 次请求，实际 %d", upstreamRecord.count())
	}

	lines := waitForLog(t, logs, "请求完成", 1)
	warn := lineWith(t, lines, "msg", "Retry-After 超过上限，不再重试")
	assertField(t, warn, "level", "WARN")
	if got := numberField(t, warn, "status"); got != http.StatusTooManyRequests {
		t.Fatalf("WARN 行的 status 不符: %v", got)
	}
	if got := stringField(t, warn, "retry_after"); got != "600" {
		t.Fatalf("WARN 行应当记下上游给的值: %v", got)
	}
	for _, line := range lines {
		if line["msg"] == "渠道内重试" {
			t.Fatalf("超限时不该有渠道内重试行: %v", line)
		}
	}
}

// TestChannelRetriesSilentWhenDisabled 覆盖 max_retries: 0：不重试，Retries 为 0，
// 一行新增日志都不写。
func TestChannelRetriesSilentWhenDisabled(t *testing.T) {
	upstreamRecord, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
	})
	front, logs, outcomes := newSeamServer(t, retryYAML(upstreamServer.URL, 0))

	status, _, body := post(t, front.URL+"/v1/chat/completions",
		`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
	if status != http.StatusTooManyRequests {
		t.Fatalf("状态码不符: %d %s", status, body)
	}
	if upstreamRecord.count() != 1 {
		t.Fatalf("max_retries: 0 时上游只应收到 1 次请求，实际 %d", upstreamRecord.count())
	}

	lines := waitForLog(t, logs, "请求完成", 1)
	for _, line := range lines {
		if line["msg"] == "渠道内重试" || line["msg"] == "渠道内重试次数已用尽" {
			t.Fatalf("max_retries: 0 时不该有重试日志: %v", line)
		}
	}

	outcome := outcomes.wait(t, 1)[0]
	if got := outcome.Attempts[0].Retries; got != 0 {
		t.Fatalf("max_retries: 0 时 Attempt.Retries 应当是 0: %d", got)
	}
}

// TestChannelRetryLogsRespectLogLevel 覆盖级别口径：调试行只在 debug 下出现。
func TestChannelRetryLogsRespectLogLevel(t *testing.T) {
	cases := []struct {
		name  string
		level slog.Level
		want  bool
	}{
		{"info 级看不见", slog.LevelInfo, false},
		{"debug 级看得见", slog.LevelDebug, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, upstreamServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(writer, http.StatusTooManyRequests, `{"error":{"message":"rate limited"}}`)
			})
			front, logs := newLoggingServerAt(t, retryYAML(upstreamServer.URL, 3), testCase.level)

			status, _, body := post(t, front.URL+"/v1/chat/completions",
				`{"model":"client-model","messages":[{"role":"user","content":"你好"}]}`)
			if status != http.StatusTooManyRequests {
				t.Fatalf("状态码不符: %d %s", status, body)
			}
			// 429 的完成行是 WARN，两种级别下都会写出，等它就能确定重试日志已经落盘。
			waitForLog(t, logs, "请求完成", 1)

			text := logs.String()
			if got := strings.Contains(text, "渠道内重试"); got != testCase.want {
				t.Fatalf("渠道内重试行出现与否不符: 期望 %t，实际 %t\n%s", testCase.want, got, text)
			}
			if got := strings.Contains(text, "渠道内重试次数已用尽"); got != testCase.want {
				t.Fatalf("重试用尽行出现与否不符: 期望 %t，实际 %t\n%s", testCase.want, got, text)
			}
		})
	}
}

// twoChannelYAML 造两条候选。firstExtra 是给第一条渠道追加的渠道级设置，
// 以逗号开头，例如 `, failover_statuses: "400,500-599"`。
func twoChannelYAML(firstURL, secondURL, firstExtra string) string {
	return fmt.Sprintf(`
channels:
  - {name: first, dialect: openai, base_url: %q%s}
  - {name: second, dialect: openai, base_url: %q}
models:
  - {match: "client-model", channel: first}
  - {match: "client-model", channel: second}
`, firstURL, firstExtra, secondURL)
}

// TestDefaultFailoverStatuses 覆盖 issue #28 的默认口径：不配两组集合时，
// 408、401、403、429 与 5xx 换候选，400、404、422 不换，与上一版逐条相同。
func TestDefaultFailoverStatuses(t *testing.T) {
	cases := []struct {
		status int
		next   bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadRequest, false},
		{http.StatusNotFound, false},
		{http.StatusUnprocessableEntity, false},
	}
	for _, item := range cases {
		t.Run(http.StatusText(item.status), func(t *testing.T) {
			_, firstServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(writer, item.status, `{"error":{"message":"上游拒绝"}}`)
			})
			secondRecord, secondServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
				writeJSON(writer, http.StatusOK, affinityResponse("second"))
			})
			front := newServer(t, twoChannelYAML(firstServer.URL, secondServer.URL, ""), "")
			status, _, body := post(t, front.URL+"/v1/chat/completions", affinityBody)
			if item.next {
				if status != http.StatusOK {
					t.Fatalf("应当换到第二条，实际 %d %s", status, body)
				}
				if secondRecord.count() != 1 {
					t.Fatalf("第二条候选应当收到一次请求，实际 %d", secondRecord.count())
				}
				return
			}
			if status != item.status {
				t.Fatalf("不应当换候选，实际 %d %s", status, body)
			}
			if secondRecord.count() != 0 {
				t.Fatalf("第二条候选不应被调用，实际 %d 次", secondRecord.count())
			}
		})
	}
}

// TestChannelFailoverStatusesOverride 覆盖渠道级覆盖：把 400 加进第一条候选的
// failover_statuses 之后，第一候选回 400 时第二候选被调用一次。
func TestChannelFailoverStatusesOverride(t *testing.T) {
	_, firstServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusBadRequest, `{"error":{"message":"不认这个模型"}}`)
	})
	secondRecord, secondServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, affinityResponse("second"))
	})
	front := newServer(t, twoChannelYAML(firstServer.URL, secondServer.URL,
		`, failover_statuses: "400,404,408,429,500-599"`), "")
	status, _, body := post(t, front.URL+"/v1/chat/completions", affinityBody)
	if status != http.StatusOK {
		t.Fatalf("400 纳入覆盖集合后应当换到第二条，实际 %d %s", status, body)
	}
	if secondRecord.count() != 1 {
		t.Fatalf("第二条候选应当收到一次请求，实际 %d", secondRecord.count())
	}
}

// TestGlobalFailoverStatusesOverride 覆盖全局覆盖：retry.failover 把 400 纳入之后，
// 没写 failover_statuses 的渠道也按新集合判断。
func TestGlobalFailoverStatusesOverride(t *testing.T) {
	_, firstServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusBadRequest, `{"error":{"message":"不认这个模型"}}`)
	})
	secondRecord, secondServer := newUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(writer, http.StatusOK, affinityResponse("second"))
	})
	front := newServer(t, "retry:\n  failover: \"400,408,429,500-599\"\n"+
		twoChannelYAML(firstServer.URL, secondServer.URL, ""), "")
	status, _, body := post(t, front.URL+"/v1/chat/completions", affinityBody)
	if status != http.StatusOK {
		t.Fatalf("全局集合纳入 400 后应当换到第二条，实际 %d %s", status, body)
	}
	if secondRecord.count() != 1 {
		t.Fatalf("第二条候选应当收到一次请求，实际 %d", secondRecord.count())
	}
}

// TestBuiltInRetryStatusesMatchConfigDefaults 把两处默认集合钉在一起。
//
// 分层红线不允许 config 导入 channel，两组默认值只能各写一份：config 侧是
// DefaultInChannelStatuses 与 DefaultFailoverStatuses 两个字符串，走 YAML 的部署
// 由装配层从这里取值；channel 侧是包级集合，给没写这两个字段就构造 Spec 的宿主兜底。
// 两者说法不一致时不会有任何报错，只会让同一个状态码在两处得到不同判定，
// 因此在这里逐个数对一遍。
func TestBuiltInRetryStatusesMatchConfigDefaults(t *testing.T) {
	cfg := &config.Config{}
	retryStatuses, err := cfg.RetryStatuses()
	if err != nil {
		t.Fatalf("解析默认的渠道内重试集合失败: %v", err)
	}
	failoverStatuses, err := cfg.FailoverStatusesFor(config.Channel{})
	if err != nil {
		t.Fatalf("解析默认的换候选集合失败: %v", err)
	}
	client, err := channel.NewClient(channel.Spec{
		Name:    "a",
		Dialect: dialect.OpenAIChat,
		BaseURL: "https://example.com",
	})
	if err != nil {
		t.Fatalf("创建客户端失败: %v", err)
	}
	for status := 100; status <= 599; status++ {
		if got, want := client.ShouldRetry(status), slices.Contains(retryStatuses, status); got != want {
			t.Fatalf("状态码 %d 的渠道内重试判定不一致: channel 内置 %v，config 默认 %v", status, got, want)
		}
		if got, want := client.ShouldFailover(status), slices.Contains(failoverStatuses, status); got != want {
			t.Fatalf("状态码 %d 的换候选判定不一致: channel 内置 %v，config 默认 %v", status, got, want)
		}
	}
}
