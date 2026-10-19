package audio

import (
	"fmt"
	"math"
	"time"

	"github.com/tphakala/go-opus/opus"
)

// OpusEncoderConfig 配置一个 Opus 编码器。
type OpusEncoderConfig struct {
	// SampleRate 必须是 48000（零值取 48000）。go-opus 还支持 8/12/16/24 kHz，
	// 但采集端固定 48 kHz，且 Opus 的容器时间戳统一按 48 kHz 计，引入其他
	// 采样率只会带来重采样负担。
	SampleRate int
	// Channels 是 1 或 2（零值取 2）。
	Channels int
	// BitrateBps 是目标码率（零值取 96000）。
	BitrateBps int
	// Complexity 是 0~10（零值取 5，见 defaultComplexity）。
	Complexity int
	// CBR 为 true 时恒定码率；为 false 时 Constrained VBR（默认推荐）。
	CBR bool
	// DTX 开启后，连续数字静音（PCM 恰好全零）超过约 200 ms 会被编成 1 字节的
	// TOC-only 包。它只省码率，不影响音质；解码端会用舒适噪声/PLC 填补。
	DTX bool
}

// OpusEncoder 把交错 float32 PCM 编码成 Opus 包，包装 go-opus。
//
// 不是并发安全的：Opus 编码器跨帧携带状态（延迟补偿环、DC 阻断滤波、CELT
// 重叠相加、能量预测、VBR 池），一帧一帧按序喂给同一个实例。
type OpusEncoder struct {
	cfg OpusEncoderConfig
	enc *opus.Encoder

	// s16 是 float32→int16 的转换暂存，out 是编码输出缓冲。两者都在构造时
	// 一次性分配，稳态编码不再分配。
	s16 []int16
	out []byte

	pcmLen int // 每帧的采样总数 = frameSamples * Channels
}

var _ Encoder = (*OpusEncoder)(nil)

// NewOpusEncoder 构造一个编码器。不做任何平台调用，可在任意 goroutine 调用。
func NewOpusEncoder(cfg OpusEncoderConfig) (*OpusEncoder, error) {
	if cfg.SampleRate == 0 {
		cfg.SampleRate = OpusSampleRate
	}
	if cfg.SampleRate != OpusSampleRate {
		return nil, fmt.Errorf("opus: sample rate %d, want %d", cfg.SampleRate, OpusSampleRate)
	}
	if cfg.Channels == 0 {
		cfg.Channels = 2
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		return nil, fmt.Errorf("opus: channels %d, want 1 or 2", cfg.Channels)
	}
	if cfg.BitrateBps == 0 {
		cfg.BitrateBps = 96_000
	}
	if cfg.Complexity == 0 {
		cfg.Complexity = defaultComplexity
	}

	enc, err := newGoOpusEncoder(cfg)
	if err != nil {
		return nil, fmt.Errorf("opus.NewEncoder: %w", err)
	}
	return &OpusEncoder{
		cfg:    cfg,
		enc:    enc,
		s16:    make([]int16, frameSamples*cfg.Channels),
		out:    make([]byte, maxPacketSize),
		pcmLen: frameSamples * cfg.Channels,
	}, nil
}

func newGoOpusEncoder(cfg OpusEncoderConfig) (*opus.Encoder, error) {
	return opus.NewEncoder(opus.EncoderConfig{
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		Bitrate:    cfg.BitrateBps,
		Complexity: cfg.Complexity,
		CBR:        cfg.CBR,
		// CBR 与 ConstrainedVBR 互斥：CBR 下 CELT 的码率控制器根本不读
		// constraint，所以只在 VBR 模式下打开。
		ConstrainedVBR: !cfg.CBR,
		DTX:            cfg.DTX,
	})
}

// Encode 编码恰好一帧 20 ms 的交错 PCM。
//
// pcm 长度必须等于 FrameSamples()*Channels()，否则返回错误——Opus 不接受任意
// 长度，分帧是采集层的职责。
//
// 返回的 EncodedFrame.Data 指向编码器内部缓冲，**只在下一次 Encode 之前有效**。
// 需要长期持有（例如交给 Sink 后异步发送）的调用方必须自己复制。20 ms ×
// 128 kbps 约 320 字节，复制成本可以忽略。
func (e *OpusEncoder) Encode(pcm []float32, ts time.Duration) (*EncodedFrame, error) {
	if e.enc == nil {
		return nil, fmt.Errorf("opus: encoder already released")
	}
	if len(pcm) != e.pcmLen {
		return nil, fmt.Errorf("opus: len(pcm)=%d, want %d (%d ms x %d ch @ %d Hz)",
			len(pcm), e.pcmLen, OpusFrameMs, e.cfg.Channels, e.cfg.SampleRate)
	}

	f32ToS16(e.s16, pcm)
	n, err := e.enc.Encode(e.s16, e.out)
	if err != nil {
		return nil, fmt.Errorf("opus: encode: %w", err)
	}
	return &EncodedFrame{
		Data:      e.out[:n],
		Timestamp: ts,
		Duration:  OpusFrameMs * time.Millisecond,
		// go-opus 文档：DTX 包长 1 字节；未开 DTX 时最短也是 2 字节。
		Discontinuous: n <= 1,
	}, nil
}

// SetBitrate 运行时改码率。
//
// go-opus 没有暴露 OPUS_SET_BITRATE，只能重建编码器。重建约 4 µs，代价是丢掉
// 跨帧状态（延迟补偿环、CELT 重叠相加等），听感上是一次 20 ms 的状态重置。
// 不要在每帧都调；用于秒级的自适应码率完全够用。
func (e *OpusEncoder) SetBitrate(bps int) error {
	if e.enc == nil {
		return fmt.Errorf("opus: encoder already released")
	}
	if bps <= 0 {
		return fmt.Errorf("opus: bitrate %d, want a positive bits-per-second", bps)
	}
	cfg := e.cfg
	cfg.BitrateBps = bps
	enc, err := newGoOpusEncoder(cfg)
	if err != nil {
		return fmt.Errorf("opus.NewEncoder: %w", err)
	}
	e.enc = enc
	e.cfg.BitrateBps = bps
	return nil
}

// FrameSamples 返回每声道每帧的采样数（960）。采集层据此分帧。
func (e *OpusEncoder) FrameSamples() int { return frameSamples }

// Channels 返回声道数。
func (e *OpusEncoder) Channels() int { return e.cfg.Channels }

// SampleRate 返回编码采样率，恒为 48000。
func (e *OpusEncoder) SampleRate() int { return e.cfg.SampleRate }

// PreSkip 返回编码器在 48 kHz 下的 pre-skip 采样数（go-opus 为 312，即 6.5 ms）。
// 写容器头和填 AudioDesc 都用这个值——不要硬编码，换编码器实现时会变。
func (e *OpusEncoder) PreSkip() int {
	if e.enc == nil {
		return 0
	}
	return e.enc.PreSkip()
}

// Flush 恒返回 nil：Opus 一次 Encode 就产出一个完整包，编码器内部不留待发包。
// 保留此方法只为与视频侧 Encoder 接口对称。
func (e *OpusEncoder) Flush() ([]*EncodedFrame, error) { return nil, nil }

// Release 释放编码器持有的缓冲。go-opus 的 Encoder 没有显式销毁入口，置 nil
// 交给 GC 即可；之后 Encode/SetBitrate 返回错误而不是 panic。
func (e *OpusEncoder) Release() {
	e.enc = nil
}

// f32ToS16 把交错 float32 PCM 转成 int16，约定输入范围为 [-1, 1]。
//
// 超出范围的部分截断到 int16 的表示边界而不是回绕——回绕会把轻微过载变成
// 爆音。NaN 按静音处理：WASAPI 在设备切换/缓冲异常时可能给出 NaN，让它流进
// 编码器会污染跨帧状态。
func f32ToS16(dst []int16, src []float32) {
	for i, v := range src {
		if math.IsNaN(float64(v)) {
			dst[i] = 0
			continue
		}
		s := v * 32767.0
		switch {
		case s >= 32767.0: // 同时覆盖 +Inf
			dst[i] = 32767
		case s <= -32768.0: // 同时覆盖 -Inf
			dst[i] = -32768
		default:
			dst[i] = int16(s)
		}
	}
}
