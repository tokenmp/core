package server_test

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
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
