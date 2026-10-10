package channel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tokenmp/core/dialect"
)

// 连接池与重试的固定取值。
const (
	// retryBaseBackoff 是重试前的基础等待时长，第 n 次重试等待 base × 2^n。
	retryBaseBackoff = 250 * time.Millisecond
	retryMaxBackoff  = 2 * time.Second
	keepAlivePeriod  = 30 * time.Second
	maxIdleConns     = 100
	idleConnTimeout  = 90 * time.Second
	// drainLimitBytes 是丢弃响应体时最多读走的字节数，只为了让连接可复用。
	drainLimitBytes = 1 << 16
)

// retryableStatus 是值得重试的上游状态码。
var retryableStatus = map[int]bool{
	http.StatusRequestTimeout:      true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

// Client 是一个渠道的 HTTP 客户端。可以并发使用。
type Client struct {
	spec       Spec
	http       *http.Client
	credential string
}

// NewClient 创建渠道客户端。凭据在此时从进程环境读一次，读不到直接报错。
func NewClient(spec Spec) (*Client, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	credential, err := spec.Auth.credential()
	if err != nil {
		return nil, err
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	transport := &http.Transport{
		// 超时只压到「收到响应头」为止。整体超时不能加在 http.Client 上：
		// 那个字段覆盖到响应体读完之后，会把长时间的流式响应整段掐断。
		ResponseHeaderTimeout: timeout,
		Proxy:                 http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: keepAlivePeriod,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdleConns,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   timeout,
		ExpectContinueTimeout: time.Second,
	}
	return &Client{
		spec:       spec,
		http:       &http.Client{Transport: transport},
		credential: credential,
	}, nil
}

// Spec 返回渠道描述。
func (c *Client) Spec() Spec { return c.spec }

// Name 返回渠道名。
func (c *Client) Name() string { return c.spec.Name }

// Dialect 返回上游方言。
func (c *Client) Dialect() dialect.Name { return c.spec.Dialect }

// Request 是一次上游调用。
type Request struct {
	// Method 是 HTTP 方法，为空时用 POST。
	Method string
	// Model 是需要写进 URL 路径的模型名，只有 Gemini 用得到。
	Model string
	// Stream 决定取流式端点还是非流式端点，只有 Gemini 用得到。
	Stream bool
	// Body 是请求体，会被完整缓存在内存里，因此可以重试。
	Body []byte
	// Header 是附加请求头。
	Header http.Header
}

// Send 发送请求并返回响应。响应体由调用方关闭。
//
// 重试只覆盖「请求没成功建立」与「上游返回可重试状态」两类情况：连接错误、
// 408、429、5xx。非流式请求会带上整体超时，流式请求不加，靠
// ResponseHeaderTimeout 与调用方的 context 控制。
func (c *Client) Send(ctx context.Context, req Request) (*http.Response, error) {
	endpoint, err := c.endpoint(req)
	if err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodPost
	}
	var lastErr error
	for attempt := 0; attempt <= c.spec.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return nil, err
			}
		}
		response, err := c.attempt(ctx, method, endpoint, req)
		if err != nil {
			lastErr = err
			continue
		}
		if retryableStatus[response.StatusCode] && attempt < c.spec.MaxRetries {
			drainAndClose(response.Body)
			lastErr = fmt.Errorf("channel: 上游返回 %s", response.Status)
			continue
		}
		return response, nil
	}
	return nil, fmt.Errorf("channel: %s 请求失败，已重试 %d 次: %w", c.spec.Name, c.spec.MaxRetries, lastErr)
}

// attempt 发一次请求。
func (c *Client) attempt(ctx context.Context, method, endpoint string, req Request) (*http.Response, error) {
	callCtx := ctx
	if !req.Stream {
		timeout := c.spec.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(callCtx, method, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("channel: 构造上游请求失败: %w", err)
	}
	for key, value := range c.spec.Headers {
		request.Header.Set(key, value)
	}
	for key, values := range req.Header {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if request.Header.Get("Content-Type") == "" {
		request.Header.Set("Content-Type", "application/json")
	}
	c.applyAuth(request)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("channel: 请求上游失败: %w", err)
	}
	return response, nil
}

// endpoint 拼出上游 URL，并把鉴权查询参数带上。
func (c *Client) endpoint(req Request) (string, error) {
	path := c.spec.Path
	if path == "" {
		resolved, err := c.spec.Dialect.UpstreamPath(req.Model, req.Stream)
		if err != nil {
			return "", err
		}
		path = resolved
	}
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	parsed, err := url.Parse(strings.TrimSuffix(c.spec.BaseURL, "/") + path)
	if err != nil {
		return "", fmt.Errorf("channel: 拼接上游 URL 失败: %w", err)
	}
	if c.spec.Auth.effectiveStyle() == StyleQuery {
		query := parsed.Query()
		query.Set(c.spec.Auth.Query, c.credential)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

// applyAuth 按样式写入鉴权信息。
func (c *Client) applyAuth(request *http.Request) {
	switch c.spec.Auth.effectiveStyle() {
	case StyleBearer:
		request.Header.Set("Authorization", "Bearer "+c.credential)
	case StyleAPIKey:
		request.Header.Set("x-api-key", c.credential)
	case StyleCustom:
		request.Header.Set(c.spec.Auth.Header, c.credential)
	case StyleQuery, StyleNone:
		// query 已在 URL 上，none 什么都不做。
	}
}

// sleepBackoff 按指数退避等待，context 取消时提前返回。
func sleepBackoff(ctx context.Context, attempt int) error {
	backoff := retryBaseBackoff << (attempt - 1)
	if backoff > retryMaxBackoff {
		backoff = retryMaxBackoff
	}
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// drainAndClose 丢弃并关闭响应体，让连接可以复用。丢弃的字节数有限，不影响语义。
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, drainLimitBytes))
	_ = body.Close()
}

// Set 是按名字索引的渠道客户端集合。
type Set struct {
	order  []string
	byName map[string]*Client
}

// NewSet 按顺序创建渠道客户端集合。任一渠道不合法或凭据缺失都会失败。
func NewSet(specs []Spec) (*Set, error) {
	set := &Set{byName: make(map[string]*Client, len(specs))}
	for _, spec := range specs {
		if _, exists := set.byName[spec.Name]; exists {
			return nil, fmt.Errorf("channel: 渠道名 %s 重复", spec.Name)
		}
		client, err := NewClient(spec)
		if err != nil {
			return nil, err
		}
		set.order = append(set.order, spec.Name)
		set.byName[spec.Name] = client
	}
	return set, nil
}

// Get 按名字取客户端。
func (s *Set) Get(name string) (*Client, bool) {
	client, ok := s.byName[name]
	return client, ok
}

// Names 按创建顺序返回渠道名。
func (s *Set) Names() []string { return append([]string(nil), s.order...) }

// Len 返回渠道数量。
func (s *Set) Len() int { return len(s.order) }
