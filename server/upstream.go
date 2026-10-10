package server

import (
	"context"
	"maps"
	"net/http"
	"strings"
)

// upstreamHeaderKey 是请求级上游头的 context 键。
//
// 用包内不导出的空结构体：宿主自己也往 context 里放键，结构体键不会撞上它们的
// 字符串键或自有类型键。
type upstreamHeaderKey struct{}

// WithUpstreamHeader 把一条上游请求头挂到 ctx 上，转发这个请求时随上游请求带出去。
//
// 宿主在 Options.Middleware 里调用：中间件装在内核外侧，这里的 ctx 会一路传到
// 转发路径与 OnRequestDone。同一个名字重复设置时后一次生效，等价于 http.Header.Set。
//
// 这是按请求给上游加头的唯一入口，客户端自带的请求头不会被转发，因此只有在这里
// 明确设置的头才会上去。
//
// 与渠道配置里的同名头按请求覆盖：请求级比渠道级具体，按请求覆盖某个渠道头正是
// 这个入口的用途。覆盖是显式的，渠道配置自己不受影响，宿主没设的同名头照旧上去。
// 渠道的鉴权头是例外，它在最后按 Auth 写入，宿主设的同名头不生效，凭据始终由渠道决定。
//
// 名字为空、或名字与取值含换行时，这条头在转发时被丢弃并记一行 WARN：让一个写错的
// 头名把整条请求带成 502 代价太大。
func WithUpstreamHeader(ctx context.Context, name, value string) context.Context {
	headers := maps.Clone(upstreamHeadersOf(ctx))
	if headers == nil {
		headers = make(map[string]string, 1)
	}
	headers[http.CanonicalHeaderKey(name)] = value
	return context.WithValue(ctx, upstreamHeaderKey{}, headers)
}

// upstreamHeadersOf 取出宿主按请求设的上游头，没有时返回 nil。
func upstreamHeadersOf(ctx context.Context) map[string]string {
	headers, _ := ctx.Value(upstreamHeaderKey{}).(map[string]string)
	return headers
}

// upstreamHeaders 把宿主按请求设的头整理成 channel.Request.Header。
//
// 一次请求只整理一遍，全部候选路由共用：宿主设的是请求级的头，换渠道重试不该让它变。
func (s *Server) upstreamHeaders(ctx context.Context) http.Header {
	headers := upstreamHeadersOf(ctx)
	if len(headers) == 0 {
		return nil
	}
	out := make(http.Header, len(headers))
	for name, value := range headers {
		if name == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			// 这里只记头名不记取值：取值是宿主可控的字符串，日志里少放一份。
			s.logger.Warn("丢弃非法的上游请求头", "request_id", requestIDOf(ctx), "header", name)
			continue
		}
		out.Set(name, value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
