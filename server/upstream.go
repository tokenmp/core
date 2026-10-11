package server

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"

	"github.com/tokenmp/core/config"
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
// 客户端自带的请求头默认不转发，只有名字出现在渠道 pass_headers 名单里时才会带上，
// 因此这里明确设置的头总是会上游。
//
// 与渠道配置里的同名头按请求覆盖：请求级比渠道级具体，按请求覆盖某个渠道头正是
// 这个入口的用途。它也比名字出现在渠道 pass_headers 名单里时从客户端转发的头更
// 具体，同名时宿主赢。覆盖是显式的，渠道配置自己不受影响，宿主没设的同名头照旧
// 上去，被覆盖的那个只发一条。覆盖是按名字整个替换：宿主看不到渠道配置里的原值，
// 两个值都想要得自己拼。渠道的鉴权头是例外，它在最后按 Auth 写入，宿主设的同名头
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

// checkPassHeaders 在装配时审一遍每条渠道的 pass_headers。
//
// config.Validate 只挡空项与含冒号、空白的写法，RFC 7230 token 的完整判断留在这里，
// 两份规则不抄在一起；禁止名单也在这里再挡一次，程序化构造的配置绕过 config.Validate
// 时同样拦得住。不像宿主头那样丢弃了事：名单是静态配置，启动时就报错比运行期静默
// 少转发一个头容易发现。
func checkPassHeaders(channels []config.Channel) error {
	denied := forbiddenHeaderSet()
	for _, item := range channels {
		for _, name := range item.PassHeaders {
			canonical := http.CanonicalHeaderKey(name)
			switch {
			case !validHeaderName(canonical):
				return fmt.Errorf("server: 渠道 %s 的 pass_headers 里 %q 不是合法的 HTTP 头名", item.Name, name)
			case denied[canonical]:
				return fmt.Errorf("server: 渠道 %s 的 pass_headers 里 %q 属于不可放行的头", item.Name, name)
			}
		}
	}
	return nil
}

// buildPassHeaders 把每条渠道的放行名单归一成查找表，运行期按表过滤客户端头。名字
// 用 http.CanonicalHeaderKey 归一：进来到的请求头已经是这个形式，两边才比得上。
//
// 非法与不可放行的名字在这里丢弃并记一行 WARN，与宿主头一样不把整条请求带成 502。
// 走 FromConfigWith 的配置已经经过 checkPassHeaders，走到这里的只剩直接调 New 的宿主。
func buildPassHeaders(channels []config.Channel, logger *slog.Logger) map[string]map[string]bool {
	denied := forbiddenHeaderSet()
	out := make(map[string]map[string]bool, len(channels))
	for _, item := range channels {
		if len(item.PassHeaders) == 0 {
			continue
		}
		set := make(map[string]bool, len(item.PassHeaders))
		for _, name := range item.PassHeaders {
			canonical := http.CanonicalHeaderKey(name)
			if !validHeaderName(canonical) || denied[canonical] {
				logger.Warn("丢弃非法的 pass_headers 项", "channel", item.Name, "header", name)
				continue
			}
			set[canonical] = true
		}
		if len(set) > 0 {
			out[item.Name] = set
		}
	}
	return out
}

// forbiddenHeaderSet 返回归一后的不可放行头名集合。名单住在 config，这里只转成
// 查找表，避免两份规则漂移。
func forbiddenHeaderSet() map[string]bool {
	names := config.ForbiddenPassHeaders()
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}
