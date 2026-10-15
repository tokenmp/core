package server

import (
	"net/http"
	"strings"

	"github.com/tokenmp/core/dialect"
)

// countTokensEndpoint 是计数端点在转发流程里的形状：它比 Messages 多一段路径
// 后缀，且只有 claude 方言的候选服务得了。
var countTokensEndpoint = endpoint{
	suffix:      countTokensSuffix,
	dialectOnly: dialect.ClaudeMessages,
}

// countTokensSuffix 是计数端点相对 Messages 路径多出来的那一段。
//
// 由两个路径常量相减得出，不另写一个字面量：计数路径在 dialect 里只有一份，
// 抄第二份迟早会与它漂移，而漂移的表现是计数请求打到消息端点上，不报错。
var countTokensSuffix = strings.TrimPrefix(dialect.ClaudeCountTokensPath, dialect.ClaudeMessages.LocalPath())

// handleCountTokens 处理 POST /v1/messages/count_tokens。
//
// 请求体与 Messages 同形（同样的 messages、system、tools，只是没有 max_tokens），
// 因此整条管线复用 Messages 方言，只多两件事：候选只认 claude 方言，发往上游的
// 路径补上计数那一段。
//
// 这个处理器不改写 request.URL.Path。访问记录里的 path 取自它，而下游的记录层
// 正是靠这个字面量区分入口的：达到「是 messages 入口」的那份记录会带着一份没有
// 用量字段的响应体进去，于是留下一行 total_tokens 为空的脏记录，全程不报错。
// 两者之间是一处跨仓库的隐式契约，任何一侧单方面改都会让对方静默变行为。
func (s *Server) handleCountTokens(writer http.ResponseWriter, request *http.Request) {
	if !s.requirePost(writer, request) {
		return
	}
	s.forward(writer, request, dialect.ClaudeMessages, "", false, countTokensEndpoint)
}
