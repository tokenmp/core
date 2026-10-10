package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/tokenmp/core/config"
	"github.com/tokenmp/core/dialect"
	"github.com/tokenmp/core/internal/affinity"
)

// SessionHeaderName 是客户端声明会话标识的请求头。
//
// 同一段会话的连续请求带上同一个值，core 会把它们粘到同一个上游渠道上，让上游
// 的提示缓存有机会命中。这个头只在本进程内使用，不会转发给上游。
const SessionHeaderName = "X-TokenMP-Session"

// affinityRoutes 按会话标识重排候选路由，粘住的那条排到最前。
//
// 只重排，不筛选：粘住的渠道失败时，后面的候选照旧按原顺序兜底。因此亲和是偏好
// 而不是钉子，渠道不可用时不会把请求卡在那里。
func (s *Server) affinityRoutes(
	request *http.Request,
	client dialect.Name,
	body []byte,
	routes []config.Route,
) []config.Route {
	if len(routes) < 2 || !anyAffinity(routes) {
		return routes
	}
	key := sessionKey(request, client, body)
	if key == "" {
		// 客户端没有给出会话标识。此时任何分配都只能靠请求内容猜，而这个模块
		// 不做猜测，保持配置顺序。
		return routes
	}
	identities := make([]string, len(routes))
	for index, route := range routes {
		identities[index] = routeIdentity(route)
	}
	order := affinity.Order(key, identities)
	reordered := make([]config.Route, 0, len(routes))
	for _, index := range order {
		reordered = append(reordered, routes[index])
	}
	if reordered[0].Channel != routes[0].Channel {
		s.logger.Debug("会话亲和改选了渠道",
			"session", maskSession(key),
			"sticky", routeOf(reordered[0]),
			"configured", routeOf(routes[0]))
	}
	return reordered
}

// sessionKey 返回本次请求的会话标识，没有时返回空串。
//
// 请求头优先：它与方言无关，Claude 与 Gemini 的客户端只能靠它。
// prompt_cache_key 是 OpenAI 为缓存路由专门提供的字段，作为兜底。
func sessionKey(request *http.Request, client dialect.Name, body []byte) string {
	if value := strings.TrimSpace(request.Header.Get(SessionHeaderName)); value != "" {
		return value
	}
	return strings.TrimSpace(dialect.SessionKeyOf(client, body))
}

// anyAffinity 报告候选路由里是否有规则要求做亲和。
func anyAffinity(routes []config.Route) bool {
	for _, route := range routes {
		if route.Affinity {
			return true
		}
	}
	return false
}

// routeIdentity 是一条候选在亲和分配里的身份。
//
// 渠道在其中，因为上游的缓存按账号隔离，同一个渠道的不同上游模型名落在同一份
// 缓存里。上游模型名也在其中，是为了让两条指向不同模型的规则成为不同的身份，
// 否则它们在排序里无法区分。
func routeIdentity(route config.Route) string {
	return route.Channel + "|" + string(route.Dialect) + "|" + route.Upstream
}

// maskSession 把会话标识收敛成可写进日志的短哈希。
//
// 标识由客户端提供，可能带着用户名一类的信息，因此不记原文。
func maskSession(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}
