package channel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	// maxBackoffShift 限制退避的指数，避免移位溢出成零等待的忙重试。
	maxBackoffShift = 6
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

// RetryableStatus 报告状态码是否属于渠道内会重试的那一类。
//
// 调用方也用得到这个判断：Send 返回一个可重试的状态码，说明渠道内的重试次数已经
// 用尽，据此可以解释「为什么没再试」。
func RetryableStatus(status int) bool { return retryableStatus[status] }

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
	// Dialect 覆盖渠道的默认方言，用于同一条渠道上按路由切换协议。
	// 为空时用渠道自己的方言。覆盖生效时渠道的 Path 不参与拼接，
	// 因为那个值是按渠道方言写的。
	Dialect dialect.Name
	// Method 是 HTTP 方法，为空时用 POST。
	Method string
	// Model 是需要写进 URL 路径的模型名，只有 Gemini 用得到。
	Model string
	// Stream 决定取流式端点还是非流式端点，只有 Gemini 用得到。
	Stream bool
	// Body 是请求体，会被完整缓存在内存里，因此可以重试。
	Body []byte
	// Header 是请求级请求头，同名时覆盖 Spec.Headers 里的值。
	//
	// 覆盖是请求级比渠道级更具体的结果：同一个头只发一条，不会两条并存。
	// 渠道的鉴权头不在此列，它在最后按 Auth 写入。
	Header http.Header
	// OnRetry 在每次渠道内重试之前调用，为 nil 时什么也不做。它在调用方的
	// goroutine 上同步执行，channel 不加锁也不缓存：重试是这次调用的事实，
	// 交给调用方决定怎么记账。
	OnRetry func(RetryInfo)
}

// RetryInfo 是一次渠道内重试的事实。
type RetryInfo struct {
	// Attempt 是第几次重试，从 1 开始。第一次请求不算重试。
	Attempt int
	// Status 是触发这次重试的上游状态码。请求没建立时是 0。
	Status int
	// Wait 是这次重试之前等待的时长。
	Wait time.Duration
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
	// retryStatus 是上一次尝试触发重试的状态码，连接失败时为 0。回调在退避
	// 等完之后、真正发出重试之前调用，因此它描述的是这次重试已经等过的时长。
	retryStatus := 0
	for attempt := 0; attempt <= c.spec.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := backoffFor(attempt)
			if err := sleepBackoff(ctx, wait); err != nil {
				return nil, err
			}
			if req.OnRetry != nil {
				req.OnRetry(RetryInfo{Attempt: attempt, Status: retryStatus, Wait: wait})
			}
		}
		response, err := c.attempt(ctx, method, endpoint, req)
		if err != nil {
			lastErr = err
			retryStatus = 0
			continue
		}
		if retryableStatus[response.StatusCode] && attempt < c.spec.MaxRetries {
			drainAndClose(response.Body)
			lastErr = fmt.Errorf("channel: 上游返回 %s", response.Status)
			retryStatus = response.StatusCode
			continue
		}
		return response, nil
	}
	return nil, fmt.Errorf("channel: %s 请求失败，已重试 %d 次: %w", c.spec.Name, c.spec.MaxRetries, lastErr)
}

// attempt 发一次请求。
//
// 非流式请求的整体超时挂到响应体上，而不是在本次调用返回时取消：调用方要等
// Send 返回之后才读响应体，在这里取消会把连接关掉，读体随即失败。
func (c *Client) attempt(ctx context.Context, method, endpoint string, req Request) (*http.Response, error) {
	callCtx := ctx
	var cancel context.CancelFunc
	if !req.Stream {
		timeout := c.spec.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		callCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	request, err := http.NewRequestWithContext(callCtx, method, endpoint, bytes.NewReader(req.Body))
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("channel: 构造上游请求失败: %w", err)
	}
	for key, value := range c.spec.Headers {
		request.Header.Set(key, value)
	}
	for key, values := range req.Header {
		// 先清掉渠道配置的同名值再写：只用 Add 会让同名头在线上出现两条，
		// 上游取第一条就等于渠道配置压过请求级，那不是调用方传 Header 的意图。
		request.Header.Del(key)
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
		if cancel != nil {
			cancel()
		}
		return nil, fmt.Errorf("channel: 请求上游失败: %w", redactError(err))
	}
	if cancel != nil {
		response.Body = &cancelOnClose{body: response.Body, cancel: cancel}
	}
	return response, nil
}

// cancelOnClose 让非流式请求的超时覆盖到响应体读完为止。
type cancelOnClose struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

// Read 透传给底层响应体。
func (b *cancelOnClose) Read(buffer []byte) (int, error) { return b.body.Read(buffer) }

// Close 关闭响应体并释放该次请求的超时计时。
func (b *cancelOnClose) Close() error {
	err := b.body.Close()
	b.once.Do(b.cancel)
	return err
}

// redactError 去掉错误信息里的查询串与用户信息。
//
// 查询式鉴权把凭据放在 URL 上，而 net/url 的错误会原样带上整个 URL，
// 直接记日志或回给客户端就等于把凭据送出去。
func redactError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	cleaned := *urlErr
	cleaned.URL = redactURL(urlErr.URL)
	return &cleaned
}

// redactURL 返回去掉查询串与用户信息的地址。
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<上游地址>"
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.User = nil
	return parsed.String()
}

// endpoint 拼出上游 URL，并把鉴权查询参数带上。
func (c *Client) endpoint(req Request) (string, error) {
	name := req.Dialect
	if name == "" {
		name = c.spec.Dialect
	}
	// 渠道的 path 覆盖只在方言一致时生效：那个值是按渠道方言写的。
	path := ""
	if name == c.spec.Dialect {
		path = c.spec.Path
	}
	if path == "" {
		resolved, err := name.UpstreamPath(req.Model, req.Stream)
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

// backoffFor 返回第 attempt 次重试前的等待时长，第 n 次为 base × 2^(n-1)。
func backoffFor(attempt int) time.Duration {
	if attempt > maxBackoffShift {
		attempt = maxBackoffShift
	}
	backoff := retryBaseBackoff << (attempt - 1)
	if backoff > retryMaxBackoff {
		backoff = retryMaxBackoff
	}
	return backoff
}

// sleepBackoff 等待一段退避时间，context 取消时提前返回。
func sleepBackoff(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
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
