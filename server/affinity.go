package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
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

// minPoolSize 是亲和池起作用所需的最少成员数。只有一条候选时没有可分配的对象。
const minPoolSize = 2

// affinityPlan 是一次请求的亲和安排。
type affinityPlan struct {
	// routes 是按亲和重排后的候选。池不成立时就是传进来的那些。
	routes []config.Route
	// chain 非空表示这次靠前缀链分配，选到渠道之后要把结果记下来。
	chain []uint64
}

// planAffinity 算出候选路由的尝试顺序。
//
// 池内按会话标识定序，粘住的那条排到最前；池外的候选保持配置顺序，排在池后面。
// 只重排，不筛选：粘住的渠道失败时，后面的候选照旧按这个顺序兜底。因此亲和是偏好
// 而不是钉子，渠道不可用时不会把请求卡在那里。
func (s *Server) planAffinity(
	request *http.Request,
	client dialect.Name,
	body []byte,
	routes []config.Route,
) affinityPlan {
	pool := affinityPool(routes)
	if len(pool) < minPoolSize {
		// 池里只有一个成员时亲和没有可分配的对象，保持配置顺序。这也让「只给一条
		// 规则写了组名」回到什么都不做的保守行为。
		return affinityPlan{routes: routes}
	}

	// 显式标识优先。它与方言无关，Claude 与 Gemini 的客户端只能靠它。
	if key := explicitSessionKey(request, client, body); key != "" {
		return affinityPlan{routes: s.orderByHash(key, pool, routes)}
	}

	// 没有标识时靠请求内容认出上一轮发过的前缀。
	entries, err := dialect.EntriesOf(client, body)
	if err != nil {
		// 切分失败不影响转发，只是这次做不了亲和。
		return affinityPlan{routes: routes}
	}
	if minimum := s.minPrefixBytes; minimum > 0 && affinity.PrefixBytes(entries) < minimum {
		// 前缀短到进不了上游缓存，钉住只剩负载集中的坏处。
		return affinityPlan{routes: routes}
	}
	chain := affinity.Chain(entries)
	if len(chain) == 0 {
		return affinityPlan{routes: routes}
	}
	if channel, ok := s.affinityTable.Recall(chain); ok {
		return affinityPlan{routes: orderByChannel(channel, pool, routes), chain: chain}
	}
	// 冷启动：按这一轮的内容先定一条，下一轮靠这条链查回来。
	return affinityPlan{
		routes: s.orderByHash(strconv.FormatUint(chain[len(chain)-1], 16), pool, routes),
		chain:  chain,
	}
}

// explicitSessionKey 返回客户端显式给出的会话标识，没有时返回空串。
//
// 请求头优先：它与方言无关，Claude 与 Gemini 的客户端只能靠它。
// prompt_cache_key 是 OpenAI 为缓存路由专门提供的字段，作为兜底。
func explicitSessionKey(request *http.Request, client dialect.Name, body []byte) string {
	if value := strings.TrimSpace(request.Header.Get(SessionHeaderName)); value != "" {
		return value
	}
	return strings.TrimSpace(dialect.SessionKeyOf(client, body))
}

// orderByHash 按 rendezvous 哈希给池内候选定序，池外的候选跟在后面。
func (s *Server) orderByHash(key string, pool []int, routes []config.Route) []config.Route {
	identities := make([]string, len(pool))
	for position, index := range pool {
		identities[position] = routeIdentity(routes[index])
	}
	order := affinity.Order(key, identities)
	reordered := orderPool(order, pool, routes)
	s.logAffinityChange(key, reordered, routes)
	return reordered
}

// orderByChannel 把某个渠道的候选排到最前，池内其余候选保持配置顺序。
//
// 不走哈希，因为要指定的就是那一条。后面的顺序跟配置一致，读起来最直观。
func orderByChannel(channel string, pool []int, routes []config.Route) []config.Route {
	order := make([]int, 0, len(pool))
	for position, index := range pool {
		if routes[index].Channel == channel {
			order = append(order, position)
		}
	}
	if len(order) == 0 {
		// 记住的渠道已经不在候选里了，回到配置顺序。
		return routes
	}
	for position, index := range pool {
		if routes[index].Channel != channel {
			order = append(order, position)
		}
	}
	return orderPool(order, pool, routes)
}

// orderPool 按给定的池内顺序组装候选列表。
func orderPool(order []int, pool []int, routes []config.Route) []config.Route {
	reordered := make([]config.Route, 0, len(routes))
	for _, position := range order {
		reordered = append(reordered, routes[pool[position]])
	}
	inPool := make(map[int]bool, len(pool))
	for _, index := range pool {
		inPool[index] = true
	}
	for index, route := range routes {
		if !inPool[index] {
			reordered = append(reordered, route)
		}
	}
	return reordered
}

// logAffinityChange 在亲和改变了首选渠道时记一条调试日志。
func (s *Server) logAffinityChange(key string, reordered, original []config.Route) {
	if len(reordered) == 0 || len(original) == 0 {
		return
	}
	if reordered[0].Channel == original[0].Channel {
		return
	}
	s.logger.Debug("会话亲和改选了渠道",
		"session", maskSession(key),
		"sticky", routeOf(reordered[0]),
		"configured", routeOf(original[0]))
}

// affinityPool 返回参与亲和的候选下标。
//
// 组名取第一条带组名的候选。同组候选组成一个池，组名不同的候选不混在一起。
// 多条规则写不同组名时只认排在最前的那一组，其余按池外候选处理。
func affinityPool(routes []config.Route) []int {
	group := ""
	for _, route := range routes {
		if route.Affinity != "" {
			group = route.Affinity
			break
		}
	}
	if group == "" {
		return nil
	}
	var pool []int
	for index, route := range routes {
		if route.Affinity == group {
			pool = append(pool, index)
		}
	}
	return pool
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
