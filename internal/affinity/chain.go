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

// Table 记住「请求前缀的指纹 → 渠道」的对应关系，供没有会话标识的客户端兜底。
//
// 键是内容指纹而不是会话名，因此两段内容完全相同的前缀会共用一条记忆。这不是
// 缺陷：内容相同就意味着上游那份缓存真的可以共享。
//
// 淘汰按插入顺序，不按最近使用。这里需要的是「上限之内保留各段会话最近若干轮的
// 指纹」，而活跃会话每一轮都写一条新记忆，上一轮那条只有一轮的岁数，挤不掉。
// 做成最近最少使用要额外维护一个堆，收益不足以抵消复杂度。
//
// Table 可以并发使用。limit 不大于零时整个记忆关闭，Recall 一律不命中。
type Table struct {
	mu      sync.Mutex
	records map[uint64]record
	// ring 按插入顺序记下还在表里的键，写满之后覆盖最旧的一格。
	//
	// 有了它淘汰是常数时间。按扫描找最久未用的那一版在上限八千时单次要扫数百万
	// 次，而且全程占着锁，实测一次淘汰耗时 177 毫秒，等于让全部并发请求停住。
	ring  []uint64
	next  int
	limit int
	ttl   time.Duration
	now   func() time.Time
}

// record 是一次记忆。
type record struct {
	channel string
	usedAt  time.Time
}

// NewTable 创建记忆表。ttl 为上游缓存的寿命，命中与写入都会续期。
func NewTable(limit int, ttl time.Duration, now func() time.Time) *Table {
	if now == nil {
		now = time.Now
	}
	table := &Table{
		records: make(map[uint64]record, max(limit, 0)),
		limit:   limit,
		ttl:     ttl,
		now:     now,
	}
	if limit > 0 {
		table.ring = make([]uint64, 0, limit)
	}
	return table
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
			// 过期的那一条直接删掉，不必等淘汰时才清。它在环里占的那一格等被
			// 覆盖时再删一次，删不存在的键是无操作。
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
	key := chain[len(chain)-1]
	if _, exists := t.records[key]; !exists {
		if len(t.ring) < t.limit {
			t.ring = append(t.ring, key)
		} else {
			delete(t.records, t.ring[t.next])
			t.ring[t.next] = key
			t.next = (t.next + 1) % t.limit
		}
	}
	t.records[key] = record{channel: channel, usedAt: t.now()}
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
