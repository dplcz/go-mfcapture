package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// Format 描述 Capturer 产出的交错 PCM 格式。
//
// Float32 决定每个采样的编码：true 为 4 字节 IEEE-754 小端（WASAPI 的
// IEEE_FLOAT），false 为 2 字节有符号小端。音频链路内部只管搬运字节，
// 真正需要 float32 的地方（编码器）用 ToFloat32 一次性转换。
type Format struct {
	SampleRate int  // 采样率；Opus 链路要求 48000
	Channels   int  // 1 或 2
	Float32    bool // true: F32LE；false: S16LE
}

// Validate 检查 Format 是否可被本包的组件消费。
func (f Format) Validate() error {
	if f.SampleRate <= 0 {
		return fmt.Errorf("audio: sample rate %d, want a positive value", f.SampleRate)
	}
	if f.Channels != 1 && f.Channels != 2 {
		return fmt.Errorf("audio: channels %d, want 1 or 2", f.Channels)
	}
	return nil
}

// BytesPerSample 返回单个声道的采样字节数（4 或 2）。
func (f Format) BytesPerSample() int {
	if f.Float32 {
		return 4
	}
	return 2
}

// BlockAlign 返回一个采样帧（所有声道）的字节数。WASAPI 的
// WAVEFORMATEX.nBlockAlign 是同一个概念。
func (f Format) BlockAlign() int { return f.BytesPerSample() * f.Channels }

// Samples 返回 data 里包含的采样帧数（每声道采样数）。
func (f Format) Samples(data []byte) int {
	ba := f.BlockAlign()
	if ba <= 0 {
		return 0
	}
	return len(data) / ba
}

// FrameBytes 返回 sampleFrames 个采样帧的字节数。
func (f Format) FrameBytes(sampleFrames int) int {
	return sampleFrames * f.BlockAlign()
}

// ToFloat32 把交错 PCM 字节转换为 float32，输出范围约 [-1, 1]。
//
// dst 至少要能放下 f.Samples(data)*f.Channels 个 float32；返回写入的
// float32 个数。data 长度必须是 BlockAlign 的整数倍，否则返回错误——
// 半截采样通常意味着采集端或缓冲边界出了问题，静默丢掉会让后续帧错位。
func (f Format) ToFloat32(dst []float32, data []byte) (int, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	ba := f.BlockAlign()
	if len(data)%ba != 0 {
		return 0, fmt.Errorf("audio: PCM length %d is not a multiple of the %d-byte block align",
			len(data), ba)
	}
	n := len(data) / ba * f.Channels
	if len(dst) < n {
		return 0, fmt.Errorf("audio: destination has %d floats, need %d", len(dst), n)
	}
	if f.Float32 {
		for i := 0; i < n; i++ {
			dst[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
		}
		return n, nil
	}
	for i := 0; i < n; i++ {
		dst[i] = float32(int16(binary.LittleEndian.Uint16(data[i*2:]))) / 32768.0
	}
	return n, nil
}

// Frame 是 Capturer 产出的一段交错 PCM。
//
// Timestamp 是 Data 中第一个采样相对 Session epoch 的 PTS，由 Capturer 统一
// 换算（WASAPI 实现把 qpcPosition 锚定到 epoch），上层不需要了解 QPC。
//
// Data 的所有权归 Capturer，只在下一次 ReadFrame 之前有效；Framer.Append
// 会复制进 ring，所以交给 Framer 后不需要再保留。
type Frame struct {
	Data      []byte
	Timestamp time.Duration
	Duration  time.Duration
	// Silent 表示这段数据是采集源补的静音（例如 WASAPI 的 SILENT 标志）。
	// 数据本身仍须是正确长度的全零 PCM，以便按采样数推进时间线。
	Silent bool
}
