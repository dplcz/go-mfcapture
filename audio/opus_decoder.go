package audio

import (
	"fmt"
	"time"

	"github.com/tphakala/go-opus/opus"
)

// OpusDecoderConfig 配置一个 Opus 解码器。
type OpusDecoderConfig struct {
	// SampleRate 必须是 48000（零值取 48000）。
	SampleRate int
	// Channels 是 1 或 2（零值取 2）。
	Channels int
	// PreSkip 是要丢弃的前导采样数（零值取 defaultPreSkip）。
	// 它来自发送端的 AudioDesc.PreSkip，不是解码器自己能算出来的：编码器
	// 有算法延迟，流开头的 PreSkip 个采样是无效填充。
	//
	// 置为负数可关闭丢弃，用于"发送端已经把 PTS 整体减掉了 pre-skip"的链路——
	// 裁剪只能做一次，两端都做会多丢 6.5 ms。
	PreSkip int
}

// OpusDecoder 把 Opus 包解码成交错 float32 PCM，包装 go-opus。
//
// 不是并发安全的：解码器跨包携带状态（SILK/CELT 重叠相加、能量预测、后滤波、
// LTP、重采样和模式切换记忆），同一个流的包必须按序喂给同一个实例。
type OpusDecoder struct {
	cfg      OpusDecoderConfig
	dec      *opus.Decoder
	frameLen int // 标准帧的采样总数 = frameSamples * Channels

	// s16 是解码输出暂存，f32 是 int16→float32 的转换目标。两者按需增长，
	// 稳态不再分配。
	s16 []int16
	f32 []float32

	// skipped 是已经丢弃的 pre-skip 采样数（每声道计）。
	skipped int
}

var _ Decoder = (*OpusDecoder)(nil)

// NewOpusDecoder 构造一个解码器。不做任何平台调用，可在任意 goroutine 调用。
func NewOpusDecoder(cfg OpusDecoderConfig) (*OpusDecoder, error) {
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
	if cfg.PreSkip == 0 {
		cfg.PreSkip = defaultPreSkip
	}

	dec, err := opus.NewDecoder(cfg.SampleRate, cfg.Channels)
	if err != nil {
		return nil, fmt.Errorf("opus.NewDecoder: %w", err)
	}
	return &OpusDecoder{
		cfg:      cfg,
		dec:      dec,
		frameLen: frameSamples * cfg.Channels,
		s16:      make([]int16, frameSamples*cfg.Channels),
		f32:      make([]float32, frameSamples*cfg.Channels),
	}, nil
}

// Decode 解码一个 Opus 包。
//
// data 为 nil 或空时触发 PLC（丢包隐藏），按标准 20 ms 帧生成补偿音频，
// 返回帧的 PLC 字段为 true。传输层检测到丢包时就该这么调，而不是跳过不补——
// 跳过会让音频时间线出现空洞，抖动缓冲会跟着错位。
//
// 返回的 DecodedFrame.PCM 指向解码器内部缓冲，**只在下一次 Decode / DecodeFEC
// 之前有效**，需要持有的调用方必须自己复制。
//
// 若本帧完全落在 pre-skip 区间内（只有帧长小于 6.5 ms 时可能发生），返回
// (nil, nil)，调用方直接跳过即可。
func (d *OpusDecoder) Decode(data []byte, ts time.Duration) (*DecodedFrame, error) {
	if d.dec == nil {
		return nil, fmt.Errorf("opus: decoder already released")
	}

	// 解码缓冲按包内实际长度给：Opus 允许 2.5~120 ms 的包，虽然本项目只产
	// 20 ms，但传输层可能收到对端打包的更长帧。
	samples := frameSamples
	if len(data) > 0 {
		if n, err := opus.PacketDuration(data); err == nil && n > samples {
			samples = n
		}
	}

	n, err := d.dec.Decode(data, d.ensureScratch(samples))
	if err != nil {
		return nil, fmt.Errorf("opus: decode: %w", err)
	}
	return d.finish(d.s16[:n*d.cfg.Channels], ts, len(data) == 0)
}

// DecodeFEC 用"下一个包"里的带内 FEC（LBRR）数据重建上一个丢失的帧。
//
// nextPacket 是丢包之后收到的那个包；若它没携带覆盖该时长的 FEC，go-opus
// 会自动退回 PLC。恢复的是标准 20 ms 帧，不裁剪 pre-skip（FEC 只可能发生在
// 流已经走过了开头之后）。
func (d *OpusDecoder) DecodeFEC(nextPacket []byte, ts time.Duration) (*DecodedFrame, error) {
	if d.dec == nil {
		return nil, fmt.Errorf("opus: decoder already released")
	}
	if len(nextPacket) == 0 {
		return nil, fmt.Errorf("opus: DecodeFEC needs the next packet, got an empty one")
	}

	n, err := d.dec.DecodeFEC(nextPacket, d.ensureScratch(frameSamples))
	if err != nil {
		return nil, fmt.Errorf("opus: decode FEC: %w", err)
	}
	total := n * d.cfg.Channels
	s16ToF32(d.f32[:total], d.s16[:total])
	return &DecodedFrame{
		PCM:       d.f32[:total],
		Timestamp: ts,
		Duration:  samplesToDuration(n, d.cfg.SampleRate),
	}, nil
}

// finish 做 int16→float32 转换、pre-skip 裁剪和帧组装。
func (d *OpusDecoder) finish(pcm []int16, ts time.Duration, plc bool) (*DecodedFrame, error) {
	n := len(pcm) / d.cfg.Channels
	s16ToF32(d.f32[:len(pcm)], pcm)

	drop := 0
	if d.cfg.PreSkip > 0 && d.skipped < d.cfg.PreSkip {
		drop = d.cfg.PreSkip - d.skipped
		if drop > n {
			drop = n
		}
		d.skipped += drop
	}
	kept := n - drop
	if kept <= 0 {
		return nil, nil
	}

	off := drop * d.cfg.Channels
	return &DecodedFrame{
		PCM: d.f32[off : off+kept*d.cfg.Channels],
		// 裁掉的采样要把 PTS 一起前推，否则解码后的流相对发送端的 PTS 会
		// 滞后 pre-skip 的时长。
		Timestamp: ts + samplesToDuration(drop, d.cfg.SampleRate),
		Duration:  samplesToDuration(kept, d.cfg.SampleRate),
		PLC:       plc,
	}, nil
}

// ensureScratch 保证暂存缓冲至少能放下 samples 个采样/声道，返回对应的 int16 视图。
func (d *OpusDecoder) ensureScratch(samples int) []int16 {
	need := samples * d.cfg.Channels
	if cap(d.s16) < need {
		d.s16 = make([]int16, need)
	}
	if cap(d.f32) < need {
		d.f32 = make([]float32, need)
	}
	return d.s16[:need]
}

// Reset 清空跨包解码器状态，并重新启用 pre-skip 裁剪。语义是"开始一条新流"：
// 切流或 seek 到流开头时用。
func (d *OpusDecoder) Reset() {
	if d.dec == nil {
		return
	}
	d.dec.Reset()
	d.skipped = 0
}

// FrameSamples 返回每声道每帧的标准采样数（960）。
func (d *OpusDecoder) FrameSamples() int { return frameSamples }

// Channels 返回声道数。
func (d *OpusDecoder) Channels() int { return d.cfg.Channels }

// SampleRate 返回解码采样率，恒为 48000。
func (d *OpusDecoder) SampleRate() int { return d.cfg.SampleRate }

// Release 释放解码器持有的缓冲。go-opus 的 Decoder 没有显式销毁入口，置 nil
// 交给 GC；之后 Decode/DecodeFEC 返回错误而不是 panic。
func (d *OpusDecoder) Release() {
	d.dec = nil
}

// s16ToF32 把交错 int16 PCM 转成 float32，输出范围约 [-1, 1]。
//
// 除 32768 而不是 32767：int16 的负半轴能表示到 -32768，用 32768 才能让
// ±满幅对称。（编码侧反向用 32767，是为了让 +1.0 不被截断到 32767 之外。）
func s16ToF32(dst []float32, src []int16) {
	for i, v := range src {
		dst[i] = float32(v) / 32768.0
	}
}

func samplesToDuration(n, sampleRate int) time.Duration {
	return time.Duration(n) * time.Second / time.Duration(sampleRate)
}
