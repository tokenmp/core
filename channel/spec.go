// Package channel 提供上游渠道的建模与 HTTP 客户端：基址拼接、鉴权样式、超时、
// 重试与 SSE 读写。
//
// 渠道只负责「怎么把一段字节送到上游、怎么把上游的字节读回来」，不解析协议内容：
// 方言之间的转换在 relay 包。
package channel

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/tokenmp/core/dialect"
)

// 鉴权样式的合法取值。
const (
	// StyleBearer 用 Authorization: Bearer <key>。
	StyleBearer = "bearer"
	// StyleAPIKey 用 x-api-key: <key>。
	StyleAPIKey = "x-api-key"
	// StyleQuery 把 key 放在查询参数上。
	StyleQuery = "query"
	// StyleCustom 用指定名字的请求头。
	StyleCustom = "custom"
	// StyleNone 不鉴权，用于指向本地假上游或已在内网放行的上游。
	StyleNone = "none"
)

// Auth 是渠道的鉴权方式。
type Auth struct {
	// Style 取 bearer、x-api-key、query、custom 或 none。
	Style string
	// KeyEnv 是凭据所在的环境变量名。凭据只在创建客户端时从进程环境读一次。
	KeyEnv string
	// Header 是 Style 为 custom 时的请求头名。
	Header string
	// Query 是 Style 为 query 时的查询参数名。
	Query string
}

// Backoff 是渠道内重试的退避参数。三个字段都可选，零值按内置常量处理，
// 因此不写这一节时的行为与逐条写死常量的版本相同。
type Backoff struct {
	// Base 是第一次重试前的等待时长，第 n 次重试等待 base × 2^(n-1)。
	Base time.Duration
	// Max 是指数退避的上限。
	Max time.Duration
	// MaxRetryAfter 是上游 Retry-After 的上限。上游要求的等待超过它时不再重试，
	// 直接交给候选切换；不缩短成上限再重试。
	MaxRetryAfter time.Duration
}

// Spec 是一个渠道的完整描述。
type Spec struct {
	Name string
	// Dialect 是上游说话用的方言。
	Dialect dialect.Name
	// BaseURL 是渠道根地址，例如 https://api.openai.com。
	BaseURL string
	// Path 覆盖方言的默认上游路径。BaseURL 已带路径时（例如
	// https://openrouter.ai/api/v1）必须显式给出，取值从该路径之后算起。
	// Gemini 的路径含模型名，不允许覆盖。
	Path string
	// Auth 是鉴权方式。
	Auth Auth
	// Timeout 是单次请求的整体超时。为 0 时用 DefaultTimeout。
	Timeout time.Duration
	// MaxRetries 是失败后的重试次数，0 表示不重试。
	MaxRetries int
	// Headers 是附加到每个请求上的头。
	Headers map[string]string
	// Backoff 是渠道内重试的退避参数，零值字段按内置常量处理。
	Backoff Backoff
	// RetryStatuses 是渠道内重试的状态码集合，由装配层从全局的重试配置填入。
	// 为空时用包级 RetryableStatus 那个内置集合。
	RetryStatuses []int
	// FailoverStatuses 是换下一条候选的状态码集合，只有 server 读它。为空时用
	// 包级 FailoverStatus 那个内置集合；渠道级配置可以覆盖它。
	FailoverStatuses []int
}

// DefaultTimeout 是未指定超时时的取值。
const DefaultTimeout = 120 * time.Second

// DefaultMaxRetryAfter 是上游 Retry-After 上限的默认值。取值对齐 openai-python
// 的 MAX_RETRY_AFTER_DELAY：超过它就不再等待重试，交给候选切换。
const DefaultMaxRetryAfter = 120 * time.Second

// MaxRetries 是重试次数的上限。转发是同步路径，重试次数乘以超时会直接变成
// 客户端等待时长，因此这里给一个上界而不是放任配置。
const MaxRetries = 10

// ErrMissingCredential 表示凭据环境变量未设置。
var ErrMissingCredential = errors.New("channel: 凭据环境变量未设置")

// Validate 校验渠道描述。
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("channel: name 不能为空")
	}
	if _, ok := dialect.Parse(string(s.Dialect)); !ok {
		return fmt.Errorf("channel: %s 的 dialect %q 不是支持的方言", s.Name, s.Dialect)
	}
	parsed, err := url.Parse(s.BaseURL)
	if err != nil {
		return fmt.Errorf("channel: %s 的 base_url 无法解析: %w", s.Name, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("channel: %s 的 base_url 必须是 http:// 或 https:// 开头", s.Name)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("channel: %s 的 base_url 不能带查询串或片段，鉴权参数由 auth 配置生成", s.Name)
	}
	if parsed.Path != "" && parsed.Path != "/" && s.Path == "" && s.Dialect != dialect.GeminiGenerate {
		return fmt.Errorf(
			"channel: %s 的 base_url 带了路径 %q，此时必须显式给出 path（从该路径之后算起）",
			s.Name, parsed.Path)
	}
	if s.Path != "" && s.Dialect == dialect.GeminiGenerate {
		return fmt.Errorf("channel: %s 的路径含模型名，不支持 path 覆盖", s.Name)
	}
	if s.MaxRetries < 0 || s.MaxRetries > MaxRetries {
		return fmt.Errorf("channel: %s 的 max_retries 须在 0 到 %d 之间", s.Name, MaxRetries)
	}
	if s.Backoff.Base < 0 || s.Backoff.Max < 0 || s.Backoff.MaxRetryAfter < 0 {
		return fmt.Errorf("channel: %s 的退避时长不能为负", s.Name)
	}
	for _, status := range s.RetryStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("channel: %s 的 retry_statuses 里 %d 不在 100 到 599 之间", s.Name, status)
		}
	}
	for _, status := range s.FailoverStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("channel: %s 的 failover_statuses 里 %d 不在 100 到 599 之间", s.Name, status)
		}
	}
	for name, value := range s.Headers {
		if strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("channel: %s 的请求头 %q 含换行符", s.Name, name)
		}
	}
	return s.Auth.Validate()
}

// Validate 校验鉴权描述。
func (a Auth) Validate() error {
	style := a.effectiveStyle()
	switch style {
	case StyleBearer, StyleAPIKey, StyleQuery, StyleCustom:
		if strings.TrimSpace(a.KeyEnv) == "" {
			return fmt.Errorf("channel: 鉴权方式 %s 需要 key_env", style)
		}
	case StyleNone:
		return nil
	default:
		return fmt.Errorf("channel: 未知鉴权方式 %q", a.Style)
	}
	if style == StyleCustom && a.Header == "" {
		return errors.New("channel: 鉴权方式 custom 需要 header")
	}
	if style == StyleQuery && a.Query == "" {
		return errors.New("channel: 鉴权方式 query 需要 query")
	}
	return nil
}

// effectiveStyle 返回实际生效的鉴权方式，未填即不鉴权。
func (a Auth) effectiveStyle() string {
	if strings.TrimSpace(a.Style) == "" {
		return StyleNone
	}
	return a.Style
}

// credential 读取凭据。返回空字符串表示该渠道不需要凭据。
func (a Auth) credential() (string, error) {
	if a.effectiveStyle() == StyleNone {
		return "", nil
	}
	value := os.Getenv(a.KeyEnv)
	if value == "" {
		return "", fmt.Errorf("%w: %s", ErrMissingCredential, a.KeyEnv)
	}
	return value, nil
}
