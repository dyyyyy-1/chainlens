// Package indexer 实现索引器主循环：确定可扫区间、拉取日志、落库、推进游标。
package indexer

// Range 是一次扫描覆盖的闭区间区块范围。
type Range struct {
	From uint64
	To   uint64
}

// nextRange 计算本轮可扫描的区间。
//
// 上界是链头本身而不是 head-confirmations：未确认块同样要入库，否则 is_finalized
// 恒为 true、「追平链头的延迟」也无从测量（HANDOFF.md 8.1）。
// 是否已确认由 safeBlock 单独判定，与扫描范围解耦。
func nextRange(nextBlock, head, batch uint64) (Range, bool) {
	if batch == 0 {
		batch = 1
	}
	if nextBlock > head {
		return Range{}, false
	}

	// 先比较跨度再相加，避免 nextBlock+batch-1 溢出。
	to := head
	if head-nextBlock >= batch {
		to = nextBlock + batch - 1
	}

	return Range{From: nextBlock, To: to}, true
}

// safeBlock 返回已确认的最高块号。
//
// 链头尚未越过确认数门槛时不存在安全块，第二个返回值为 false。
func safeBlock(head, confirmations uint64) (uint64, bool) {
	if confirmations >= head {
		return 0, false
	}
	return head - confirmations, true
}
