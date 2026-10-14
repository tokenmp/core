package server

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"time"
)

// 请求 id 的字节布局：前缀、计数器、随机段三段拼成 20 字节，
// 再用无填充的标准 base32 编成 32 个字符。
//
// 三段的职责不同，不要合并：
//   - 前缀是时间刻度，只为人看：同一个时间格内产生的 id 前几位相同，
//     翻日志时一眼能认出「这几条是同一批」。它不承担唯一性。
//   - 计数器是进程内自增，只为可读与调试：这条进程收到第几条一眼可见。
//     它同样不承担唯一性，进程一重启就从头发一遍。
//   - 随机段是唯一性的全部来源，12 字节即 96 位。按生日界，记录数到 10^9 时
//     碰撞概率约 10^-11；再加长的收益要等记录数涨几个数量级才看得见，
//     而每条请求都要为此多读几次熵源。
//
// 20 字节经 base32 恰好编成 32 个字符（160 位是 5 的整数倍，没有余数），
// 对下游的 VARCHAR(64) 留足余量，不需要截断逻辑。字符集是 base32 的 A-Z 与
// 2-7：不含引号、反斜杠与控制字符，写进 JSON 日志行或 SQL 字面量都不必转义。
const (
	// idPrefixBytes 是前缀的字节数，也是计数器的字节数：两段都是 32 位整数。
	idPrefixBytes  = 4
	idCounterBytes = 4
	// idRandomBytes 是随机段的字节数，见上面关于生日界的说明。
	idRandomBytes = 12
	// idTotalBytes 是三段拼起来的字节数。
	idTotalBytes = idPrefixBytes + idCounterBytes + idRandomBytes
	// idPrefixQuantum 是前缀的时间刻度，单位秒。四秒一格：够同一批请求落进同一格，
	// 又让 32 位的格数要等上百年才绕回一圈。
	idPrefixQuantum = 4
	// idTruncate 是取低 32 位用的掩码。两段都要的正是截断——时间前缀按格数绕回、
	// 计数器到 2^32 回零——所以掩码写出来比按「不可能溢出」处理更诚实，也让
	// 静态检查看得见这个取舍，不必用 nolint 把它藏起来。
	idTruncate = 0xFFFFFFFF
)

// idEncoding 是 id 的字符集。用标准 base32 而不是 URL 变体：
// id 只出现在日志与下游的数据库里，不需要容忍 URL 与文件名。
var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newRequestID 生成一个请求 id。
//
// 下游把它当全局幂等键用（同一批重复提交靠它去重），因此唯一性不能建立在
// 「服务进程不重启」这个前提上：进程一重启，任何进程内的计数器都会从头发一遍，
// 新产生的 id 与库里既有行撞车，重复的那些会被下游整批当成重发丢掉——调用照常
// 返回成功，记录、账单与扣费却都不发生，而且不报错。
//
// 所以唯一性全部落在随机段上，前缀与计数器只为人读。不采用「时间戳 + 进程标识
// + 计数器」那类写法：它的正确性依赖「进程标识在部署范围内唯一」，而容器里每个
// PID 命名空间都从 1 开始，横向扩容时这个前提正好不成立——那正是幂等键最要紧的
// 场景。随机段不依赖任何这类前提。
//
// 随机段的代价是每条请求都要读熵源，这可以接受：一次 crypto/rand.Read 的开销
// 远低于一次上游请求，而它是这条链路上唯一能保证跨进程、跨重启、跨实例都不重复
// 的机制。
//
// 同一进程内并发的多次调用互不阻塞：前缀与计数器取自无锁的时间源与原子计数器，
// 熵源本身也是并发安全的。id 只在日志与回调里出现，不参与任何路由或鉴权判断。
func (s *Server) newRequestID() string {
	var raw [idTotalBytes]byte
	binary.BigEndian.PutUint32(raw[:idPrefixBytes], uint32((time.Now().Unix()/idPrefixQuantum)&idTruncate))
	binary.BigEndian.PutUint32(raw[idPrefixBytes:idPrefixBytes+idCounterBytes],
		uint32(s.requestIDCounter.Add(1)&idTruncate))
	// crypto/rand.Read 自 Go 1.24 起保证填满且不返回错误：读不到熵源时进程会
	// 不可恢复地崩溃，而不是返回一个错误（文档写明了这两点）。仍按签名处理返回值，
	// 是为了让 errcheck 看得见这个调用，而不是靠 nolint 把它藏起来。
	if _, err := rand.Read(raw[idPrefixBytes+idCounterBytes:]); err != nil {
		panic("server: 读取随机字节失败: " + err.Error())
	}
	return idEncoding.EncodeToString(raw[:])
}
