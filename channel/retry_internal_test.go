package channel

import (
	"net/http"
	"testing"
	"time"
)

// TestRetryAfterWait 覆盖 Retry-After 的两种格式：秒数与 HTTP 日期。只认 429 与
// 503，缺失或不可解析时返回 0，让指数退避照旧兜底。
func TestRetryAfterWait(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name   string
		status int
		value  string
		want   time.Duration
	}{
		{"秒数", http.StatusTooManyRequests, "2", 2 * time.Second},
		{"零秒按缺省", http.StatusTooManyRequests, "0", 0},
		{"负秒按缺省", http.StatusTooManyRequests, "-1", 0},
		{"HTTP 日期", http.StatusTooManyRequests, now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second},
		{"已过期的日期按缺省", http.StatusTooManyRequests, now.Add(-time.Second).Format(http.TimeFormat), 0},
		{"503 也认", http.StatusServiceUnavailable, "4", 4 * time.Second},
		{"500 不认", http.StatusInternalServerError, "4", 0},
		{"不可解析按缺省", http.StatusTooManyRequests, "soon", 0},
		{"缺失按缺省", http.StatusTooManyRequests, "", 0},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			response := &http.Response{StatusCode: item.status, Header: http.Header{}}
			if item.value != "" {
				response.Header.Set("Retry-After", item.value)
			}
			if got := retryAfterWait(response, now); got != item.want {
				t.Fatalf("retryAfterWait = %v，期望 %v", got, item.want)
			}
		})
	}
}
