package audio

import "time"

// Renderer 抽象一个音频播放设备，与采集侧的 Capturer 对称。
//
// 与 Capturer 一样，Renderer 不是并发安全的：构造、Write、Drain 和 Release
// 必须在同一个 goroutine 上调用，调用方还要保证该 goroutine 已经初始化 COM
// （Windows 实现要求 MTA）。解码会话的 OnPCMFrame 在 Run 所在 goroutine 上
// 回调，直接在回调里 Write 即可。
//
// 实现可以是文件、网络或空设备；Write 返回 nil 只表示数据已被接受，不表示
// 已经播放。需要等待尾部播完时调用 Drain。
type Renderer interface {
	// Format 返回设备实际接受的 PCM 格式。Write 的输入固定是 48 kHz 交错
	// float32，实现自行完成到设备格式的转换（含单/双声道之间的搬运）。
	Format() Format
	// Write 写入交错 float32 PCM（48 kHz，1 或 2 声道，范围 [-1, 1]）。
	//
	// 实现应把 Write 当作背压点：设备缓冲满时阻塞，直到数据被接受。这样上游
	// 解码不必自己实现节拍，队列也不会无限增长。
	Write(pcm []float32) error
	// Drain 等待已经 Write 的数据真正播放完，最多等待 timeout。timeout <= 0
	// 表示只检查一次。返回 ErrTimeout 表示还有数据没播完。
	Drain(timeout time.Duration) error
	// Release 停止播放并释放设备资源。可重复调用。
	Release()
}
