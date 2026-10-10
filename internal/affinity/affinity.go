// Package affinity 按会话标识在候选渠道之间做确定性分配，让同一段会话的连续
// 请求落到同一个上游账号上，从而使上游的提示缓存有机会命中。
//
// 这里只做排序，不做选择：粘住的那条排在最前，其余候选按固定顺序跟在后面，
// 调用方的失败降级照旧生效。
package affinity

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// Order 返回 identities 的下标顺序，粘性最强的一条排在最前。
//
// 用 rendezvous 哈希（最高随机权重），不用取模。区别在候选集合变化时：取模会让
// 几乎所有键重新分配，rendezvous 只移动原本落在增删项上的那一部分。渠道上下线
// 时按取模会让全部会话的缓存集体失效，按 rendezvous 只影响其中一个渠道的份额。
func Order(key string, identities []string) []int {
	order := make([]int, len(identities))
	if len(identities) == 0 {
		return order
	}
	scored := make([]scoredIdentity, len(identities))
	for index, identity := range identities {
		scored[index] = scoredIdentity{
			index:     index,
			identity:  identity,
			signature: signature(key, identity),
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].signature != scored[j].signature {
			return scored[i].signature > scored[j].signature
		}
		// 分数相同时按标识本身定序，保证跨进程结果一致。
		return scored[i].identity < scored[j].identity
	})
	for position, item := range scored {
		order[position] = item.index
	}
	return order
}

// Pick 返回 key 会粘住的标识，identities 为空时返回空串。
func Pick(key string, identities []string) string {
	if len(identities) == 0 {
		return ""
	}
	return identities[Order(key, identities)[0]]
}

// scoredIdentity 是一条候选的排序依据。
type scoredIdentity struct {
	index     int
	identity  string
	signature uint64
}

// signature 把会话标识与候选标识映到一个 64 位分数。
//
// 用 SHA-256 而不是 FNV 这类快哈希，是因为 rendezvous 的均衡性依赖雪崩性质：
// 分数分布有偏时，同一个渠道会吃掉过多的键。
func signature(key, identity string) uint64 {
	buffer := make([]byte, 0, len(key)+len(identity)+1)
	buffer = append(buffer, key...)
	// 加一个分隔字节，避免（"ab", "c"）与（"a", "bc"）算出同一个分数。
	buffer = append(buffer, 0)
	buffer = append(buffer, identity...)
	sum := sha256.Sum256(buffer)
	return binary.BigEndian.Uint64(sum[:8])
}
