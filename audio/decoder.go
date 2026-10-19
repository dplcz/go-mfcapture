package audio

import "time"

// DecodedFrame 是一帧解码后的交错 PCM。
type DecodedFrame struct {
	// PCM 是交错 float32，范围为 [-1, 1]。首帧长度可能小于一帧的采样数
	// （pre-skip 裁剪掉了开头几个毫秒），之后每帧都是满的。
	PCM       []float32
	Timestamp time.Duration
	Duration  time.Duration
	// PLC 为 true 时本帧是丢包隐藏生成的补偿音频，不是真实编码内容。
	PLC bool
}

// Decoder 抽象一个音频解码器，与视频侧的 decode.Decoder 对称。
type Decoder interface {
	// Decode 解码一个 Opus 包。data 为 nil 或空时触发 PLC（丢包隐藏）。
	Decode(data []byte, ts time.Duration) (*DecodedFrame, error)
	// DecodeFEC 用"下一个包"里的带内 FEC（LBRR）数据重建上一个丢失的帧。
	DecodeFEC(nextPacket []byte, ts time.Duration) (*DecodedFrame, error)
	Reset()
	Release()
}
