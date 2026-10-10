package affinity

import (
	"fmt"
	"testing"
)

// TestOrderIsPermutation 确认返回的是下标的一个排列。
func TestOrderIsPermutation(t *testing.T) {
	identities := []string{"a", "b", "c", "d", "e"}
	order := Order("sess-1", identities)
	if len(order) != len(identities) {
		t.Fatalf("长度不符: %d", len(order))
	}
	seen := make(map[int]bool, len(order))
	for _, index := range order {
		if index < 0 || index >= len(identities) {
			t.Fatalf("下标越界: %d", index)
		}
		if seen[index] {
			t.Fatalf("下标重复: %d", index)
		}
		seen[index] = true
	}
}

// TestOrderIsDeterministic 确认同一个键每次得到同一个顺序。
func TestOrderIsDeterministic(t *testing.T) {
	identities := []string{"a", "b", "c"}
	first := Order("sess-1", identities)
	for range 50 {
		next := Order("sess-1", identities)
		for index := range first {
			if first[index] != next[index] {
				t.Fatalf("同一会话的顺序不稳定:\n%v\n%v", first, next)
			}
		}
	}
}

// TestOrderIsEmpty 确认空候选不报错。
func TestOrderIsEmpty(t *testing.T) {
	if order := Order("sess-1", nil); len(order) != 0 {
		t.Fatalf("空候选应当返回空顺序，实际 %v", order)
	}
	if got := Pick("sess-1", nil); got != "" {
		t.Fatalf("空候选应当返回空串，实际 %q", got)
	}
}

// TestDistributionIsEven 确认分配是均衡的，没有哪个渠道吃掉大部分会话。
//
// 这是 rendezvous 哈希的核心性质：分数分布有偏会让负载失衡，比缓存失效更糟。
func TestDistributionIsEven(t *testing.T) {
	identities := []string{"a", "b", "c", "d"}
	const total = 4000
	counts := make(map[string]int, len(identities))
	for index := range total {
		key := fmt.Sprintf("sess-%d", index)
		counts[Pick(key, identities)]++
	}
	want := total / len(identities)
	// 允许 20% 的偏差，超过就说明哈希的雪崩性质有问题。
	tolerance := want / 5
	for _, identity := range identities {
		if counts[identity] < want-tolerance || counts[identity] > want+tolerance {
			t.Fatalf("渠道 %s 分到 %d 个会话，期望 %d 上下浮动 %d: %v",
				identity, counts[identity], want, tolerance, counts)
		}
	}
}

// TestRemovingIdentityOnlyMovesItsOwnShares 确认渠道下线时只影响它自己的份额。
//
// 这条是选 rendezvous 而不是取模的理由。按取模实现时，去掉一个渠道会让几乎
// 全部会话换渠道，上游缓存集体失效。
func TestRemovingIdentityOnlyMovesItsOwnShares(t *testing.T) {
	before := []string{"a", "b", "c", "d"}
	after := []string{"a", "b", "c"}
	removed := "d"
	const total = 2000

	moved := 0
	wasOnRemoved := 0
	for index := range total {
		key := fmt.Sprintf("sess-%d", index)
		previous := Pick(key, before)
		current := Pick(key, after)
		if previous == removed {
			wasOnRemoved++
		}
		if previous != current {
			moved++
		}
	}
	if wasOnRemoved == 0 {
		t.Fatal("没有会话落在被移除的渠道上，这个测试失去了意义")
	}
	// 只有原本落在被移除渠道上的会话才允许换渠道。
	if moved != wasOnRemoved {
		t.Fatalf("换渠道的会话 %d 个，落在被移除渠道上的只有 %d 个", moved, wasOnRemoved)
	}
}

// TestAddedIdentityOnlyTakesItsOwnShares 确认新增渠道只从已有渠道各取一份。
func TestAddedIdentityOnlyTakesItsOwnShares(t *testing.T) {
	before := []string{"a", "b", "c"}
	after := []string{"a", "b", "c", "d"}
	const total = 2000

	moved := 0
	for index := range total {
		key := fmt.Sprintf("sess-%d", index)
		if Pick(key, before) != Pick(key, after) {
			moved++
		}
	}
	// 新渠道应当吃掉大约四分之一的会话，允许较大偏差，只要求它是这个量级。
	if moved < total/8 || moved > total/2 {
		t.Fatalf("新增渠道后换渠道的会话有 %d 个，不在总量的 1/8 到 1/2 之间", moved)
	}
}
