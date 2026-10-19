package audio

import "time"

// EncodedFrame 是一个完整的 Opus 包及其时间线信息。
type EncodedFrame struct {
	Data      []byte
	Timestamp time.Duration // 包内第一个采样的 PTS，相对 epoch
	Duration  time.Duration // 固定 20 ms
	// Discontinuous 为 true 时本包是 DTX 产生的 1 字节空包，解码端应做丢包
	// 隐藏而不是当作正常音频。
	Discontinuous bool
}

// Encoder 抽象一个音频编码器，与视频侧的 encode.Encoder 对称。
type Encoder interface {
	// Encode 编码恰好一帧 20 ms 的交错 PCM，长度必须等于 FrameSamples()*声道数。
	Encode(pcm []float32, ts time.Duration) (*EncodedFrame, error)
	SetBitrate(bps int) error
	Flush() ([]*EncodedFrame, error)
	Release()
}
