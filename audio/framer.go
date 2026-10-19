package audio

import (
	"fmt"
	"time"
)

// FramerConfig 配置 Framer。
type FramerConfig struct {
	Format Format
	// MaxLatency 是积压上限，零值取 200 ms。超过后丢弃最旧的整帧。
	MaxLatency time.Duration
}

// Framer 把 Capturer 产出的任意长度 PCM 切成固定 20 ms 帧，并维护 PTS 时间线。
//
// 三项职责对应 P2 的验收标准：
//   - 分帧：Take 只在攒满 960 个采样时产出一帧，长期平均 50 pps；
//   - 静音补齐：Silence 在采集源没有数据时按 20 ms 推进时间线；
//   - 积压丢弃：底层 Ring 超过 MaxLatency 时丢最旧的整帧，DroppedFrames 统计。
//
// Framer 不做阻塞，也不做限速：调用方（AudioPipeline）决定何时读采集源、
// 何时补静音。输入节奏正常时输出节奏自然是 50 pps；WASAPI 一次给 30 ms
// 时可能连续产出两帧，长期速率仍然正确。
//
// 时间线规则：第一条非空数据锚定 PTS，之后按采样数推进。ring 排空后再次
// 来数据时允许向前重新对齐到采集端时钟（设备/流恢复），但不会倒退——补过
// 静音后源时间戳略早于下一帧时，保持单调性是抖动缓冲更需要的性质。
//
// 不是并发安全的。Take 返回的 Frame.Data 复用内部缓冲，只在下一次 Take
// 或 Silence 之前有效。
type Framer struct {
	format     Format
	ring       *Ring
	frameBytes int
	frameDur   time.Duration
	buf        []byte // Take 的复制目标，避免把 ring 内部布局暴露出去
	silence    []byte // 全零帧，Silence 反复复用

	nextPTS time.Duration
	started bool
	dropped int // 累计丢弃的采样帧数（每声道）
}

// NewFramer 创建一个 Framer，内部按 MaxLatency 分配 ring。
func NewFramer(cfg FramerConfig) (*Framer, error) {
	if err := cfg.Format.Validate(); err != nil {
		return nil, err
	}
	if cfg.Format.SampleRate != OpusSampleRate {
		return nil, fmt.Errorf("audio: sample rate %d, want %d (resampling is not implemented)",
			cfg.Format.SampleRate, OpusSampleRate)
	}
	ring, err := NewRing(cfg.Format, cfg.MaxLatency)
	if err != nil {
		return nil, err
	}
	frameBytes := cfg.Format.FrameBytes(frameSamples)
	return &Framer{
		format:     cfg.Format,
		ring:       ring,
		frameBytes: frameBytes,
		frameDur:   samplesToDuration(frameSamples, cfg.Format.SampleRate),
		buf:        make([]byte, frameBytes),
		silence:    make([]byte, frameBytes),
	}, nil
}

// Append 把一帧采集数据复制进 ring。nil 或空数据是合法的空操作（WASAPI
// 的 GetNextPacketSize 为 0 时会出现），不会锚定时间线。
func (f *Framer) Append(fr *Frame) error {
	if fr == nil || len(fr.Data) == 0 {
		return nil
	}
	if len(fr.Data)%f.format.BlockAlign() != 0 {
		return fmt.Errorf("audio: PCM length %d is not a multiple of the %d-byte block align",
			len(fr.Data), f.format.BlockAlign())
	}

	if !f.started {
		f.nextPTS = fr.Timestamp
		f.started = true
	} else if f.ring.Available() == 0 && fr.Timestamp > f.nextPTS {
		// 缓冲已经排空，源时钟走到了下一帧之后：这是真实的间隔（例如
		// 设备短暂停顿），以采集端时间为准向前对齐。
		f.nextPTS = fr.Timestamp
	}

	dropped, err := f.ring.Append(fr.Data)
	if err != nil {
		return err
	}
	if dropped > 0 {
		f.dropped += dropped
		// 丢掉的是时间线最前面的采样，PTS 跟着前推，否则保留下来的音频
		// 会被标晚 dropped 这么久。
		f.nextPTS += samplesToDuration(dropped, f.format.SampleRate)
	}
	return nil
}

// Take 在攒满一个 20 ms 帧时取出它，否则返回 (nil, false)。
func (f *Framer) Take() (*Frame, bool) {
	if f.ring.Available() < frameSamples {
		return nil, false
	}
	src, err := f.ring.Take(frameSamples)
	if err != nil {
		// Available 已经检查过，这里只可能是取帧参数错误。
		return nil, false
	}
	copy(f.buf, src)

	fr := &Frame{
		Data:      f.buf,
		Timestamp: f.nextPTS,
		Duration:  f.frameDur,
	}
	f.nextPTS += f.frameDur
	return fr, true
}

// Silence 产出一帧 20 ms 静音并推进时间线。
//
// 调用方在采集源超时且 Available()==0 时调用它，保持 50 pps 包率和 A/V
// 同步。返回的 Data 指向 Framer 内部的只读零缓冲，调用方不得修改；
// 它会持续有效，但每次 Silence 返回的 Timestamp 不同。
func (f *Framer) Silence() *Frame {
	f.started = true
	fr := &Frame{
		Data:      f.silence,
		Timestamp: f.nextPTS,
		Duration:  f.frameDur,
		Silent:    true,
	}
	f.nextPTS += f.frameDur
	return fr
}

// Available 返回已缓冲、尚未分帧的采样帧数（每声道）。
func (f *Framer) Available() int { return f.ring.Available() }

// NextPTS 返回下一帧将要使用的 PTS。
func (f *Framer) NextPTS() time.Duration { return f.nextPTS }

// DroppedSamples 返回因积压累计丢弃的采样帧数（每声道）。
func (f *Framer) DroppedSamples() int { return f.dropped }

// DroppedFrames 返回按 20 ms 帧折算的丢弃量。不足一帧的部分向下取整，
// 精确值用 DroppedSamples。
func (f *Framer) DroppedFrames() int { return f.dropped / frameSamples }

// Format 返回构造时使用的 PCM 格式。
func (f *Framer) Format() Format { return f.format }

// Reset 清空缓冲、时间线和丢弃统计。用于设备切换或开始一条新流；
// 之后第一次 Append/Silence 会重新锚定 PTS。
func (f *Framer) Reset() {
	f.ring.Reset()
	f.nextPTS = 0
	f.started = false
	f.dropped = 0
}
