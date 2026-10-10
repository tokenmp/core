package affinity

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// entries 造一段条目序列，第 n 个条目的内容由参数决定。
func entries(items ...string) [][]byte {
	out := make([][]byte, 0, len(items))
	for _, item := range items {
		out = append(out, []byte(item))
	}
	return out
}

// TestChainIsDeterministic 确认同样的条目算出同样的链。
func TestChainIsDeterministic(t *testing.T) {
	first := Chain(entries("head", "一", "二"))
	second := Chain(entries("head", "一", "二"))
	if len(first) != len(second) {
		t.Fatalf("长度不符: %d 与 %d", len(first), len(second))
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("第 %d 项不符: %d 与 %d", index, first[index], second[index])
		}
	}
}

// TestChainExtendsByPrefix 确认多一条就是在前一条后面接一项，前面的项不变。
//
// 这是前缀链的全部依据。不成立的话从末尾往回查就永远查不到上一轮写下的指纹。
func TestChainExtendsByPrefix(t *testing.T) {
	short := Chain(entries("head", "一"))
	long := Chain(entries("head", "一", "二"))
	if len(long) != 3 || len(short) != 2 {
		t.Fatalf("长度不符: %d 与 %d", len(short), len(long))
	}
	for index := range short {
		if short[index] != long[index] {
			t.Fatalf("第 %d 项变了: %d 与 %d", index, short[index], long[index])
		}
	}
}

// TestChainSeparatesContent 确认内容不同就分叉。
func TestChainSeparatesContent(t *testing.T) {
	first := Chain(entries("head", "一"))
	second := Chain(entries("head", "二"))
	if first[0] != second[0] {
		t.Fatal("头条目相同时第一项应当相同")
	}
	if first[1] == second[1] {
		t.Fatal("第二条不同时第二项应当不同")
	}
}

// TestChainDoesNotConfuseBoundaries 确认条目的切分方式会被区分出来。
//
// 「甲」「乙」与「甲乙」是不同的条目序列，不能被算成同一条链。
func TestChainDoesNotConfuseBoundaries(t *testing.T) {
	split := Chain(entries("head", "甲", "乙"))
	joined := Chain(entries("head", "甲乙"))
	if split[1] == joined[1] {
		t.Fatal("条目边界不同时指纹应当不同")
	}
}

// TestChainEmpty 确认空序列返回空链。
func TestChainEmpty(t *testing.T) {
	if chain := Chain(nil); len(chain) != 0 {
		t.Fatalf("空序列应当返回空链，实际 %v", chain)
	}
}

// TestPrefixBytes 确认字节数是各条目之和。
func TestPrefixBytes(t *testing.T) {
	if got := PrefixBytes(entries("abc", "de")); got != 5 {
		t.Fatalf("字节数不符: %d", got)
	}
	if got := PrefixBytes(nil); got != 0 {
		t.Fatalf("空序列应当是 0，实际 %d", got)
	}
}

// clock 是可控时间，用来测过期而不必真的等。
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// TestTableRemembersChannel 确认记住之后能查回来。
func TestTableRemembersChannel(t *testing.T) {
	table := NewTable(16, time.Minute, newClock().Now)
	chain := Chain(entries("head", "一"))
	table.Remember(chain, "account-a")

	channel, ok := table.Recall(chain)
	if !ok || channel != "account-a" {
		t.Fatalf("应当查回 account-a，实际 %q / %v", channel, ok)
	}
}

// TestTableMatchesLongestPrefix 确认从末尾往回找，命中最近的那一段。
//
// 上一轮写了三项，这一轮有四项，查表必须命中上一轮那三项，而不是更早的。
func TestTableMatchesLongestPrefix(t *testing.T) {
	table := NewTable(16, time.Minute, newClock().Now)
	table.Remember(Chain(entries("head")), "旧渠道")
	table.Remember(Chain(entries("head", "一", "二")), "新渠道")

	channel, ok := table.Recall(Chain(entries("head", "一", "二", "三")))
	if !ok || channel != "新渠道" {
		t.Fatalf("应当命中最近一段的新渠道，实际 %q / %v", channel, ok)
	}
}

// TestTableMissesUnrelatedChain 确认没见过的前缀不命中。
func TestTableMissesUnrelatedChain(t *testing.T) {
	table := NewTable(16, time.Minute, newClock().Now)
	table.Remember(Chain(entries("head", "一")), "account-a")

	if channel, ok := table.Recall(Chain(entries("head", "别的"))); ok {
		t.Fatalf("不应当命中，实际 %q", channel)
	}
}

// TestTableExpires 确认超过寿命的记忆不再命中。
//
// 上游的缓存有寿命，会话冷掉之后继续把它钉在同一个渠道只会白搭上负载集中。
func TestTableExpires(t *testing.T) {
	now := newClock()
	table := NewTable(16, 5*time.Minute, now.Now)
	chain := Chain(entries("head", "一"))
	table.Remember(chain, "account-a")

	if _, ok := table.Recall(chain); !ok {
		t.Fatal("寿命之内应当命中")
	}
	now.advance(6 * time.Minute)
	if channel, ok := table.Recall(chain); ok {
		t.Fatalf("超过寿命后不应当命中，实际 %q", channel)
	}
}

// TestTableRecallRefreshesLifetime 确认读一次就续期。
//
// 上游缓存被读一次寿命就延长一次，这里的记忆跟着走。
func TestTableRecallRefreshesLifetime(t *testing.T) {
	now := newClock()
	table := NewTable(16, 5*time.Minute, now.Now)
	chain := Chain(entries("head", "一"))
	table.Remember(chain, "account-a")

	for range 3 {
		now.advance(4 * time.Minute)
		if _, ok := table.Recall(chain); !ok {
			t.Fatal("每隔四分钟读一次，应当一直活着")
		}
	}
}

// TestTableEvictsOverLimit 确认条数压得回上限以内。
func TestTableEvictsOverLimit(t *testing.T) {
	now := newClock()
	table := NewTable(50, time.Hour, now.Now)
	for index := range 400 {
		table.Remember(Chain(entries("head", fmt.Sprintf("第%d条", index))), "account-a")
		now.advance(time.Second)
	}
	if got := table.Len(); got > 50 {
		t.Fatalf("条数超过上限: %d", got)
	}
}

// TestTableEvictsOldestFirst 确认淘汰时先删最久未用的。
func TestTableEvictsOldestFirst(t *testing.T) {
	now := newClock()
	table := NewTable(20, time.Hour, now.Now)
	oldest := Chain(entries("head", "最早"))
	table.Remember(oldest, "account-old")
	now.advance(time.Second)

	for index := range 100 {
		table.Remember(Chain(entries("head", fmt.Sprintf("第%d条", index))), "account-new")
		now.advance(time.Second)
	}
	if channel, ok := table.Recall(oldest); ok {
		t.Fatalf("最久未用的那条应当已被淘汰，实际还查得到 %q", channel)
	}
}

// TestTableDisabledWhenLimitIsZero 确认上限不大于零时整个记忆关闭。
func TestTableDisabledWhenLimitIsZero(t *testing.T) {
	table := NewTable(0, time.Minute, newClock().Now)
	chain := Chain(entries("head", "一"))
	table.Remember(chain, "account-a")

	if _, ok := table.Recall(chain); ok {
		t.Fatal("关闭时不应命中")
	}
	if table.Len() != 0 {
		t.Fatalf("关闭时不应存东西，实际 %d 条", table.Len())
	}
}

// TestTableIgnoresEmptyChain 确认空链不写也不查。
func TestTableIgnoresEmptyChain(t *testing.T) {
	table := NewTable(16, time.Minute, newClock().Now)
	table.Remember(nil, "account-a")
	if table.Len() != 0 {
		t.Fatalf("空链不应当写入，实际 %d 条", table.Len())
	}
	if _, ok := table.Recall(nil); ok {
		t.Fatal("空链不应当命中")
	}
	// 没有渠道名的记忆没有意义。
	table.Remember(Chain(entries("head")), "")
	if table.Len() != 0 {
		t.Fatalf("空渠道名不应当写入，实际 %d 条", table.Len())
	}
}

// TestTableConcurrent 确认并发读写没有数据竞争。配合 -race 运行。
func TestTableConcurrent(t *testing.T) {
	now := newClock()
	table := NewTable(64, time.Hour, now.Now)
	var group sync.WaitGroup
	for worker := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range 200 {
				chain := Chain(entries("head", fmt.Sprintf("第%d条", index%32)))
				table.Remember(chain, fmt.Sprintf("account-%d", worker))
				if channel, ok := table.Recall(chain); ok && channel == "" {
					t.Errorf("命中的渠道名为空")
					return
				}
			}
		}()
	}
	group.Wait()
	if table.Len() > 64 {
		t.Fatalf("条数超过上限: %d", table.Len())
	}
}

// TestChainAndTableTogether 走一遍真实用法：第二轮靠上一轮的指纹找到渠道。
func TestChainAndTableTogether(t *testing.T) {
	table := NewTable(16, time.Minute, newClock().Now)

	// 第一轮：客户端只发了头与一条消息，冷启动，按顺序选了 account-a。
	first := Chain(entries("head", "一"))
	table.Remember(first, "account-a")

	// 第二轮：历史长了一段，没有任何显式标识。
	second := Chain(entries("head", "一", "二", "三"))
	channel, ok := table.Recall(second)
	if !ok {
		t.Fatal("第二轮应当认出上一轮的前缀")
	}
	if channel != "account-a" {
		t.Fatalf("应当沿用 account-a，实际 %q", channel)
	}

	// 第二轮实际用了 account-b（比如 account-a 限流），记下真实的那条。
	table.Remember(second, "account-b")

	// 第三轮：应当沿用第二轮真实用过的 account-b，而不是第一轮的 account-a。
	third := Chain(entries("head", "一", "二", "三", "四"))
	channel, ok = table.Recall(third)
	if !ok || channel != "account-b" {
		t.Fatalf("应当沿用第二轮真实使用的 account-b，实际 %q / %v", channel, ok)
	}
}

// TestTableEvictionIsConstantTime 确认淘汰不会随着记忆条数增长而变慢。
//
// 按扫描找最久未用的那一版在八千条时单次淘汰要扫数百万次，实测耗时 177 毫秒，
// 而且全程占着锁。这里给一个宽到只可能拦住数量级退化的上限，不测真实的微秒数。
func TestTableEvictionIsConstantTime(t *testing.T) {
	now := newClock()
	table := NewTable(8192, time.Hour, now.Now)
	for index := range 8192 {
		table.Remember([]uint64{uint64(index)}, "account")
		now.advance(time.Millisecond)
	}
	start := time.Now()
	for index := range 200 {
		table.Remember([]uint64{uint64(1_000_000 + index)}, "account")
		now.advance(time.Millisecond)
	}
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("表满后 200 次写入耗时 %v，淘汰不是常数时间", elapsed)
	}
	if table.Len() > 8192 {
		t.Fatalf("条数超过上限: %d", table.Len())
	}
}

// TestTableSmallLimitKeepsEntries 确认很小的上限也能存下东西。
//
// 上一版按「删到上限的九成」淘汰，上限为 1 时目标就是 0，插入即被清空。
func TestTableSmallLimitKeepsEntries(t *testing.T) {
	table := NewTable(1, time.Hour, newClock().Now)
	table.Remember([]uint64{1}, "account")
	if table.Len() != 1 {
		t.Fatalf("上限为 1 时应当存下一条，实际 %d 条", table.Len())
	}
	if channel, ok := table.Recall([]uint64{1}); !ok || channel != "account" {
		t.Fatalf("应当查得回来，实际 %q / %v", channel, ok)
	}
	// 再来一条，会把上一条挤掉。
	table.Remember([]uint64{2}, "account")
	if table.Len() != 1 {
		t.Fatalf("上限为 1 时挤掉旧的之后仍应只有一条，实际 %d 条", table.Len())
	}
	if _, ok := table.Recall([]uint64{1}); ok {
		t.Fatal("被挤掉的那条不应当还在")
	}
}
