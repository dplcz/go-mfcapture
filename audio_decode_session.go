package mfcapture

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"mfcapture/audio"
)

// defaultAudioDecodeQueueSize 是 Submit 队列的默认深度。20 ms 一包、8 路复用
// 也不过 400 包/秒，64 包相当于 1.28 秒的缓冲，足够吸收调度抖动。
const defaultAudioDecodeQueueSize = 64

// AudioDecodeSink 是解码会话向上层交付 PCM 与状态事件的通道。由上层实现。
//
// 与视频侧 decode.Sink 一样，会话不做队列、不丢帧、不限流——解出一帧就同步
// 调一次回调；是否阻塞、如何缓冲是上层自己的策略。
type AudioDecodeSink interface {
	// OnPCMFrame 在 Run 所在 goroutine 内同步调用。
	//
	// f.PCM 指向解码器的内部缓冲，**只在本次回调内有效**：下一次解码会覆盖它。
	// 需要跨回调持有（例如交给另一个线程播放）的调用方必须自己复制。这样约定
	// 是为了让解码器保持稳态零分配——20 ms 一帧、每秒复制 50 次的收益抵不过
	// 多一次内存分配。
	OnPCMFrame(f *audio.DecodedFrame)
	// OnAudioEvent 上报状态变化：AudioDescReady 保证早于第一个 OnPCMFrame；
	// AudioDecodeError 表示单个包解码失败，会话继续运行。
	OnAudioEvent(e AudioEvent)
}

// nopAudioDecodeSink 供基准测试/调试使用；上层不传 Sink 时用它兜底。
type nopAudioDecodeSink struct{}

func (nopAudioDecodeSink) OnPCMFrame(*audio.DecodedFrame) {}
func (nopAudioDecodeSink) OnAudioEvent(AudioEvent)        {}

// AudioDecodeSessionConfig 配置一条 Opus 解码会话。
type AudioDecodeSessionConfig struct {
	// Channels 是对端 Opus 流的声道数（1 或 2），0 = 2。裸包传输没有容器头，
	// 解码器只能由上层告知。
	Channels int
	// PreSkip 是要丢弃的前导采样数，应来自发送端 AudioDesc.PreSkip。
	// 0 = go-opus 默认的 DefaultPreSkip（312）；负数表示发送端已经把 PTS
	// 整体前移、接收端不要再裁——pre-skip 只能扣一次。
	PreSkip int
	// BufferSize 是 Submit 队列深度，0 = 64。队列满时 Submit 返回 false。
	BufferSize int
}

// AudioDecodeSession 是一条"Opus 包 → 交错 PCM"的解码会话。
//
// 线程模型：
//   - NewAudioDecodeSession 只做参数校验，可在任意 goroutine 调用。
//   - Run 在调用方 goroutine 上阻塞运行，解码与 Sink 回调都发生在该 goroutine；
//     Opus 解码是纯 Go、不碰 COM，因此不需要 LockOSThread。
//   - Submit / SubmitPLC 可在任意 goroutine 调用，包通过 channel 投递到 Run
//     所在 goroutine 解码，绝不并发调用编解码器——Opus 解码器跨包携带状态，
//     同一个流必须按序串行喂入。
//   - 停止会话请 cancel 传给 Run 的 ctx；Close 只需在 Run 返回后调用一次。
type AudioDecodeSession struct {
	cfg  AudioDecodeSessionConfig
	sink AudioDecodeSink

	pktCh  chan audioPacket
	closed atomic.Bool

	desc AudioDesc
}

// audioPacket 是队列里的一个待解码项。data 为 nil 表示 PLC 请求。
type audioPacket struct {
	data []byte
	ts   time.Duration
}

// NewAudioDecodeSession 构造一个解码会话，不做任何解码器分配。
// sink 为 nil 时所有回调变成 no-op（仅建议基准测试使用）。
func NewAudioDecodeSession(cfg AudioDecodeSessionConfig, sink AudioDecodeSink) (*AudioDecodeSession, error) {
	if cfg.Channels == 0 {
		cfg.Channels = 2
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		return nil, fmt.Errorf("audio decode session: channels %d, want 1 or 2", cfg.Channels)
	}
	if cfg.BufferSize == 0 {
		cfg.BufferSize = defaultAudioDecodeQueueSize
	}
	if cfg.BufferSize < 0 {
		return nil, fmt.Errorf("audio decode session: buffer size %d, want a non-negative value", cfg.BufferSize)
	}
	if sink == nil {
		sink = nopAudioDecodeSink{}
	}

	preSkip := cfg.PreSkip
	switch {
	case preSkip == 0:
		preSkip = audio.DefaultPreSkip
	case preSkip < 0:
		// 发送端已经扣过 pre-skip：接收端实际要处理的量是 0。
		preSkip = 0
	}

	return &AudioDecodeSession{
		cfg:   cfg,
		sink:  sink,
		pktCh: make(chan audioPacket, cfg.BufferSize),
		desc: AudioDesc{
			Codec:      "opus",
			SampleRate: audio.OpusSampleRate,
			Channels:   cfg.Channels,
			FrameMs:    audio.OpusFrameMs,
			PreSkip:    preSkip,
		},
	}, nil
}

// Run 在调用方 goroutine 上运行解码循环，直到 ctx 被取消。它是阻塞的。
//
// 返回 ctx.Err()（正常结束）或解码器创建失败。单包解码失败不会让 Run 返回，
// 而是通过 AudioDecodeError 上报后继续。
func (s *AudioDecodeSession) Run(ctx context.Context) error {
	dec, err := audio.NewOpusDecoder(audio.OpusDecoderConfig{
		SampleRate: audio.OpusSampleRate,
		Channels:   s.cfg.Channels,
		PreSkip:    s.cfg.PreSkip,
	})
	if err != nil {
		return fmt.Errorf("NewOpusDecoder: %w", err)
	}
	defer dec.Release()

	s.sink.OnAudioEvent(AudioEvent{Kind: AudioDescReady, Desc: s.desc})

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case pkt := <-s.pktCh:
			s.decode(dec, pkt)
		}
	}
}

// Submit 投递一个 Opus 包。可在任意 goroutine 调用；解码在 Run 所在 goroutine
// 异步进行，所以本方法不阻塞在解码上。
//
// 返回 false 表示会话已关闭、data 为空或队列已满——该包没有被接收。丢包后上层
// 应调用 SubmitPLC 补齐时间线，而不是留空洞；队列满时同理。
//
// data 会被复制一份，调用方可以在返回后自由复用/归还自己的缓冲区。
func (s *AudioDecodeSession) Submit(data []byte, ts time.Duration) bool {
	if s.closed.Load() || len(data) == 0 {
		return false
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	return s.enqueue(audioPacket{data: buf, ts: ts})
}

// SubmitPLC 显式请求一帧丢包隐藏。传输层按 RTP 序号检测到丢包、或本地 Submit
// 返回 false 时调用；会话内部用 Decoder.Decode(nil, ts) 生成 20 ms 补偿音频，
// 产出的帧 PLC 为 true。可重复调用，每次对应一个丢失的帧位。
func (s *AudioDecodeSession) SubmitPLC(ts time.Duration) bool {
	if s.closed.Load() {
		return false
	}
	return s.enqueue(audioPacket{ts: ts})
}

func (s *AudioDecodeSession) enqueue(pkt audioPacket) bool {
	select {
	case s.pktCh <- pkt:
		return true
	default:
		return false
	}
}

// Close 标记会话已关闭，之后的 Submit / SubmitPLC 变成 no-op。应在 Run 返回
// 之后调用；解码器由 Run 自己释放。通道不会 close，避免"先关闭后投递"导致
// panic。
func (s *AudioDecodeSession) Close() error {
	s.closed.Store(true)
	return nil
}

// decode 解码一包并交付。只能在 Run 所在 goroutine 调用。
func (s *AudioDecodeSession) decode(dec *audio.OpusDecoder, pkt audioPacket) {
	f, err := dec.Decode(pkt.data, pkt.ts)
	if err != nil {
		s.sink.OnAudioEvent(AudioEvent{Kind: AudioDecodeError, Err: err})
		return
	}
	if f == nil {
		// 整帧都落在 pre-skip 区间内（只有短于 pre-skip 的包会这样），跳过。
		return
	}
	s.sink.OnPCMFrame(f)
}
