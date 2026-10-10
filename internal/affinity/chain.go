package affinity

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"
)

// Chain 把条目序列折成一条累积哈希链。
//
// 链的第 i 项是「前 i + 1 个条目」的指纹。因此同一段会话在连续几轮里的链互为
// 前缀：上一轮的整条链就是这一轮链的前一段。靠这个关系，查「上一轮走了哪个渠道」
// 不需要知道上一轮有几个条目，只要从末尾往回查第一个命中的下标。
//
// 不用「整段内容算一个哈希」，是因为那样只能回答「这一轮是不是上一轮」，回答不了
// 「这一轮的开头哪一段是上一轮」。工具调用在一轮里能占六七条，去掉尾部固定条数
// 这种办法算不准。
func Chain(entries [][]byte) []uint64 {
	chain := make([]uint64, 0, len(entries))
	// 用完整的 32 字节摘要串联，只把前 8 字节作为指纹暴露出去。串联时截断会让
	// 不同的前段在后续条目上撞到一起。
	var running [sha256.Size]byte
	for index, entry := range entries {
		digest := sha256.New()
		if index > 0 {
			digest.Write(running[:])
		}
		digest.Write(entry)
		copy(running[:], digest.Sum(nil))
		chain = append(chain, binary.BigEndian.Uint64(running[:8]))
	}
	return chain
}

// PrefixBytes 返回整个条目序列的字节数。
//
// 用来判断前缀够不够长。上游对可缓存的最小长度有门槛，短到进不了缓存时做亲和
// 只剩负载集中的坏处。
func PrefixBytes(entries [][]byte) int {
	total := 0
	for _, entry := range entries {
		total += len(entry)
	}
	return total
}

// 淘汰后回落到的比例。留出余量是为了不必每次插入都做一次全表扫描：回落到九成
// 之后，要再插进一成才会触发下一次淘汰。
const (
	evictNumerator   = 9
	evictDenominator = 10
)

// record 是一次记忆。
type record struct {
	channel string
	usedAt  time.Time
}

// Table 记住「请求前缀的指纹 → 渠道」的对应关系，供没有会话标识的客户端兜底。
//
// 键是内容指纹而不是会话名，因此两段内容完全相同的前缀会共用一条记忆。这不是
// 缺陷：内容相同就意味着上游那份缓存真的可以共享。
//
// Table 可以并发使用。limit 不大于零时整个记忆关闭，Recall 一律不命中。
type Table struct {
	mu      sync.Mutex
	records map[uint64]record
	limit   int
	ttl     time.Duration
	now     func() time.Time
}

// NewTable 创建记忆表。ttl 为上游缓存的寿命，命中与写入都会续期。
func NewTable(limit int, ttl time.Duration, now func() time.Time) *Table {
	if now == nil {
		now = time.Now
	}
	return &Table{
		records: make(map[uint64]record, max(limit, 0)),
		limit:   limit,
		ttl:     ttl,
		now:     now,
	}
}

// Recall 从链的末尾往回找第一条还活着的记忆，返回它记下的渠道。
//
// 从末尾往回是因为链越长说明前缀越贴近当前请求，命中的那一条就是最近一轮发过的
// 前缀。命中即续期：上游那份缓存被读一次就延长一次寿命。
func (t *Table) Recall(chain []uint64) (string, bool) {
	if t.limit <= 0 || t.ttl <= 0 || len(chain) == 0 {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for index := len(chain) - 1; index >= 0; index-- {
		found, ok := t.records[chain[index]]
		if !ok {
			continue
		}
		if now.Sub(found.usedAt) > t.ttl {
			// 过期的那一条直接删掉，不必等淘汰时才清。
			delete(t.records, chain[index])
			continue
		}
		found.usedAt = now
		t.records[chain[index]] = found
		return found.channel, true
	}
	return "", false
}

// Remember 记下这一轮的完整前缀对应哪个渠道。
//
// 只写链的末项。前段由更早的轮次写过，Recall 会从末尾往回扫到它们。
func (t *Table) Remember(chain []uint64, channel string) {
	if t.limit <= 0 || t.ttl <= 0 || len(chain) == 0 || channel == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.records[chain[len(chain)-1]] = record{channel: channel, usedAt: t.now()}
	t.evictLocked()
}

// Len 返回当前的记忆条数。
func (t *Table) Len() int {
	if t.limit <= 0 {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.records)
}

// evictLocked 把条数压回上限以内。调用方须持有锁。
//
// 先清过期的，还不够就按最久未用删到上限的九成。留出那一成是为了不必每次插入都
// 做一次全表扫描。
func (t *Table) evictLocked() {
	if len(t.records) <= t.limit {
		return
	}
	now := t.now()
	for key, item := range t.records {
		if now.Sub(item.usedAt) > t.ttl {
			delete(t.records, key)
		}
	}
	target := t.limit * evictNumerator / evictDenominator
	for len(t.records) > target {
		var oldestKey uint64
		var oldestAt time.Time
		first := true
		for key, item := range t.records {
			if first || item.usedAt.Before(oldestAt) {
				oldestKey, oldestAt, first = key, item.usedAt, false
			}
		}
		if first {
			return
		}
		delete(t.records, oldestKey)
	}
}
