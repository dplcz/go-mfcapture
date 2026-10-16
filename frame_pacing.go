package mfcapture

// frameLimiter 按固定节拍限制写入帧率。期望时刻每次前进整数个间隔，而不是以上一帧的实际
// 到达时刻为基准：到达时刻带 1ms 量级的调度抖动，MaxFPS 与显示器刷新率接近时（如 60 对 59Hz），
// 以实际到达时刻为基准会把约四成帧误判为"过密"。
type frameLimiter struct {
	interval int64 // 100ns；<=0 表示不限
	next     int64 // 下一帧的期望时刻
	started  bool
}

func newFrameLimiter(maxFPS uint32) *frameLimiter {
	l := &frameLimiter{}
	if maxFPS > 0 {
		l.interval = int64(10_000_000) / int64(maxFPS)
	}
	return l
}

// allow 判断 hns（100ns 单位，单调递增）时刻到达的帧是否写入。允许比期望时刻早到 10% 个间隔。
func (l *frameLimiter) allow(hns int64) bool {
	if l.interval <= 0 {
		return true
	}
	if !l.started {
		l.started = true
		l.next = hns + l.interval
		return true
	}
	if hns < l.next-l.interval/10 {
		return false
	}
	l.next += l.interval
	if l.next <= hns { // 落后超过一个间隔（画面静止后恢复）：重新对齐，避免突发放行
		l.next = hns + l.interval
	}
	return true
}
