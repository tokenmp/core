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
// 这个入口的用途。覆盖是显式的，渠道配置自己不受影响，宿主没设的同名头照旧上去，
// 被覆盖的那个只发一条。渠道的鉴权头是例外，它在最后按 Auth 写入，宿主设的同名头
// 不生效，凭据始终由渠道决定。
//
// 一个名字只带一个值：ctx 里存的是名字到取值的映射，重复字段的头（比如两条同名
// Cookie）表达不了。
//
// 名字不是合法的 HTTP 头名（RFC 7230 的 token），或取值含控制字符时，这条头在转发
// 时被丢弃并记一行 WARN：让一个写错的头名把整条请求带成 502 代价太大。水平制表符
// 在取值里是合法的，其他控制字符不是。
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
		if !validHeaderName(name) || !validHeaderValue(value) {
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

// validHeaderName 判断名字是不是合法的 HTTP 头名，即 RFC 7230 的 token。
//
// 只挡空名与换行不够：名字里带空格这类字符时 Go 的传输层会在写请求时报错，那条请求
// 就变成 502 了，与「写错一个头名不该带走整条请求」相反。标准库没有导出这个判断，
// 为它拉一个依赖不划算，所以按 token 字符集自己判。
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for index := 0; index < len(name); index++ {
		switch character := name[index]; {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", character) >= 0:
		default:
			return false
		}
	}
	return true
}

// validHeaderValue 判断取值里有没有会被传输层拒掉的控制字符（水平制表符除外），
// 理由与头名相同。
func validHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
