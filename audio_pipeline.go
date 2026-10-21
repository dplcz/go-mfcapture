package mfcapture

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	d3d "github.com/kirides/go-d3d"

	"mfcapture/audio"
	"mfcapture/mf"
)

// AudioDesc 描述一条 Opus 音频流的解码参数，供对端初始化解码器。
type AudioDesc struct {
	Codec      string // 恒为 "opus"
	SampleRate int    // 恒为 48000
	Channels   int    // 实际协商到的声道数（1 或 2）
	BitrateBps int    // 目标码率
	FrameMs    int    // 恒为 20
	// PreSkip 是编码器的 pre-skip 采样数（go-opus 为 312，即 6.5 ms）。
	// 写容器头时用这个值；传输层可以整体前移 PTS 或由接收端裁剪，二选一。
	PreSkip int
}

// AudioEventKind 标识音频管线主动上报的状态变化。
type AudioEventKind uint8

const (
	// AudioDescReady 在任何 OnAudioFrame 之前触发一次，此时 Desc() 已可用。
	AudioDescReady AudioEventKind = iota
	// AudioDeviceChanged 在采集设备失效后成功重建采集源时触发，Desc 为新的
	// 流参数（声道数可能与之前不同）。事件里的 Err 非 nil 表示这次重建失败，
	// 管线会继续按退避重试。
	AudioDeviceChanged
	// AudioCaptureError 采集或编码发生非致命错误时触发。Err 为具体错误。
	AudioCaptureError
	// AudioSilenceDegraded 连续补静音超过 audioSilenceDegradedAfter 时触发一次。
	AudioSilenceDegraded
	// AudioDecodeError 是 AudioDecodeSession 解码单个 Opus 包失败时上报的事件。
	// 该帧会被跳过，会话继续运行。Err 为具体错误。
	AudioDecodeError
)

// AudioEvent 是音频管线向上层主动上报的状态变化。
type AudioEvent struct {
	Kind AudioEventKind
	Desc AudioDesc // DescReady / DeviceChanged 时有效
	Err  error     // DeviceChanged 失败 / CaptureError 时有效
}

// AudioSink 是音频管线向上层交付数据的唯一通道。由上层实现。
//
// 契约与视频侧 Sink 一致：管线不做队列、不丢帧、不限流，编码出一帧就同步
// 调一次 OnAudioFrame。上层若在回调里阻塞，会让采集 ring 积压并触发丢帧，
// 这是上层自己的选择。帧数据已经由管线复制，Sink 可以长期持有。
type AudioSink interface {
	OnAudioFrame(f *audio.EncodedFrame)
	OnAudioEvent(e AudioEvent)
}

// nopAudioSink 供基准测试/调试使用；上层不传 Sink 时用它兜底。
type nopAudioSink struct{}

func (nopAudioSink) OnAudioFrame(*audio.EncodedFrame) {}
func (nopAudioSink) OnAudioEvent(AudioEvent)          {}

// AudioStats 是音频管线的累计统计快照。
type AudioStats struct {
	Frames       uint64 // 已交付的编码帧数（含补静音）
	Bytes        uint64 // 已交付的编码字节数
	EncodeUs     uint64 // 最近一帧的编码耗时（微秒）
	Dropped      uint64 // 因积压累计丢弃的采样数（每声道）
	SilentFrames uint64 // 其中由管线补出来的静音帧数
	ActualBps    uint64 // 最近一秒的实际码率
}

// AudioPipelineConfig 配置一条采集→分帧→编码的音频管线。
type AudioPipelineConfig struct {
	// Epoch 是音频 PTS 的零点，零值取 Run 开始的时刻。视频管线必须用同一个
	// 值，A/V 同步才有意义。
	Epoch time.Time
	// BitrateBps 是目标码率，0 = 96000。
	BitrateBps int
	// Channels 是向采集端点请求的声道数（1 或 2），0 = 2。设备回退到混音格式
	// 时以实际声道数为准，AudioDesc.Channels 会如实上报。
	Channels int
	// Complexity 是 Opus 复杂度 0~10，0 = 5。
	Complexity int
	// CBR 为 true 时恒定码率；false（默认）= Constrained VBR。
	CBR bool
	// DTX 开启后长段数字静音会编码成 1 字节空包，省码率。
	DTX bool
	// NoSilence 关闭"无数据时补静音"。零值 = 补静音，保持 50 pps 包率与
	// A/V 时间线；关闭后时间线会停在最后一份真实数据处。
	NoSilence bool
	// MaxLatencyMs 是分帧缓冲的积压上限，0 = 200 ms。
	MaxLatencyMs int
}

// AudioPipeline 是一条"WASAPI loopback → Opus 包"的管线。
//
// 线程模型与视频 Pipeline 相同：
//   - NewAudioPipeline 只做参数校验，不碰 COM，可在任意 goroutine 调用。
//   - Run 绑定 OS 线程，并在该线程上完成 CoInitializeEx、打开采集端点、
//     建编码器、跑循环、清理，全程不换线程。
//   - SetBitrate / Stats / Desc 可在任意 goroutine 调用；SetBitrate 通过命令
//     队列投递到 Run 线程执行，绝不跨线程直接调 COM。
//   - 停止管线请 cancel 传给 Run 的 ctx；Close 只需在 Run 返回后调用一次。
//
// 设备失效（ErrDeviceLost）由 Run 自己处理：销毁并在退避后重建采集源，
// 中断期间继续按 20 ms 补静音；只有 ctx 取消才会让 Run 返回。
type AudioPipeline struct {
	cfg  AudioPipelineConfig
	sink AudioSink

	cmdCh  chan audioCmd
	closed atomic.Bool

	frames       atomic.Uint64
	bytes        atomic.Uint64
	encodeUs     atomic.Uint64
	dropped      atomic.Uint64
	silentFrames atomic.Uint64
	actualBps    atomic.Uint64

	descMu sync.RWMutex
	desc   AudioDesc

	// newCapturer 只在 Run 开始前赋值（测试用它替换 WASAPI）。生产路径用
	// audio.NewWASAPICapturer。
	newCapturer func(audio.WASAPICapturerConfig) (audio.Capturer, error)

	// 以下字段只在 Run() 所在 goroutine 内访问。
	capturer audio.Capturer
	framer   *audio.Framer
	encoder  *audio.OpusEncoder
	format   audio.Format
	pcm      []float32
	epoch    time.Time

	windowStart time.Time
	windowBytes uint64
	silenceRun  time.Duration // 连续补静音的累计时长
	degraded    bool          // 本轮静音已经上报过 AudioSilenceDegraded
}

type audioCmdKind uint8

const (
	audioCmdBitrate audioCmdKind = iota
)

type audioCmd struct {
	kind audioCmdKind
	bps  int
}

// 音频循环的时间参数。读采集源最多等一个 20 ms 帧位，这样 WASAPI 的事件
// 唤醒不会被切成碎片，同时静音补齐的抖动不超过一帧。
const (
	audioReadTimeout          = audio.OpusFrameMs * time.Millisecond
	audioMinReadTimeout       = time.Millisecond
	audioReconnectMin         = 500 * time.Millisecond
	audioReconnectMax         = 5 * time.Second
	audioSilenceDegradedAfter = 5 * time.Second
	audioBitrateWindow        = time.Second
)

// NewAudioPipeline 构造一条音频管线。不做任何 COM 调用。
// sink 为 nil 时所有回调变成 no-op（仅建议基准测试使用）。
func NewAudioPipeline(cfg AudioPipelineConfig, sink AudioSink) (*AudioPipeline, error) {
	if cfg.BitrateBps == 0 {
		cfg.BitrateBps = 96_000
	}
	if cfg.BitrateBps < 0 {
		return nil, fmt.Errorf("audio pipeline: bitrate %d, want a positive bits-per-second", cfg.BitrateBps)
	}
	if cfg.Channels == 0 {
		cfg.Channels = 2
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		return nil, fmt.Errorf("audio pipeline: channels %d, want 1 or 2", cfg.Channels)
	}
	if cfg.Complexity < 0 || cfg.Complexity > 10 {
		return nil, fmt.Errorf("audio pipeline: complexity %d, want 0..10", cfg.Complexity)
	}
	if cfg.MaxLatencyMs < 0 {
		return nil, fmt.Errorf("audio pipeline: max latency %d ms, want a non-negative value", cfg.MaxLatencyMs)
	}
	if sink == nil {
		sink = nopAudioSink{}
	}
	p := &AudioPipeline{
		cfg:         cfg,
		sink:        sink,
		cmdCh:       make(chan audioCmd, commandQueueSize),
		newCapturer: newWASAPICapturer,
	}
	if !cfg.Epoch.IsZero() {
		p.epoch = cfg.Epoch
	}
	return p, nil
}

func newWASAPICapturer(cfg audio.WASAPICapturerConfig) (audio.Capturer, error) {
	c, err := audio.NewWASAPICapturer(cfg)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Desc 返回当前音频流描述。在收到 AudioDescReady 之前返回零值。
func (p *AudioPipeline) Desc() AudioDesc {
	p.descMu.RLock()
	defer p.descMu.RUnlock()
	return p.desc
}

// Stats 返回累计统计快照，可在任意 goroutine 调用。
func (p *AudioPipeline) Stats() AudioStats {
	return AudioStats{
		Frames:       p.frames.Load(),
		Bytes:        p.bytes.Load(),
		EncodeUs:     p.encodeUs.Load(),
		Dropped:      p.dropped.Load(),
		SilentFrames: p.silentFrames.Load(),
		ActualBps:    p.actualBps.Load(),
	}
}

// SetBitrate 运行时调整目标码率。可在任意 goroutine 调用，通过命令队列
// 在 Run 线程异步执行；go-opus 会重建编码器，听感上是一次 20 ms 的状态重置。
func (p *AudioPipeline) SetBitrate(bps int) {
	if p.closed.Load() {
		return
	}
	select {
	case p.cmdCh <- audioCmd{kind: audioCmdBitrate, bps: bps}:
	default: // 队列满说明 Run 已停止，丢弃即可
	}
}

// Close 标记管线已关闭，之后的 SetBitrate 变成 no-op。应在 Run 返回之后调用；
// Run 内部的资源释放由 Run 自己完成。命令通道不会 close，避免"先关闭后投递"
// 导致 panic。
func (p *AudioPipeline) Close() error {
	p.closed.Store(true)
	return nil
}

// Run 在调用方 goroutine 上运行整条音频管线，直到 ctx 被取消。它是阻塞的。
//
// 返回的 error 为 ctx.Err()（正常结束）或无法恢复的采集/编码错误。设备失效
// 会在内部重连，不会让 Run 返回。
func (p *AudioPipeline) Run(ctx context.Context) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hr := mf.CoInitializeEx(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("CoInitializeEx: %w", d3d.HRESULT(hr))
	}
	defer mf.CoUninitialize()

	if p.epoch.IsZero() {
		p.epoch = time.Now()
	}

	capturer, err := p.openCapturer()
	if err != nil {
		p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
		return fmt.Errorf("NewWASAPICapturer: %w", err)
	}
	p.capturer = capturer
	defer func() {
		if p.capturer != nil {
			p.capturer.Release()
			p.capturer = nil
		}
	}()

	if err := p.setup(capturer.Format()); err != nil {
		p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
		return err
	}
	defer func() {
		if p.encoder != nil {
			p.encoder.Release()
			p.encoder = nil
		}
	}()
	p.sink.OnAudioEvent(AudioEvent{Kind: AudioDescReady, Desc: p.Desc()})

	p.windowStart = time.Now()
	backoff := audioReconnectMin

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.drainCommands()

		// 时间线已锚定、缓冲为空且当前帧位已经到期：按要求补静音。启动时
		// 先不锚定，给采集源一个真正的机会交出第一包，避免无故吞掉 20 ms。
		now := p.now()
		if !p.cfg.NoSilence && p.framer.Started() && p.framer.Available() == 0 &&
			now >= p.framer.NextPTS() {
			if err := p.emitFrame(p.framer.Silence()); err != nil {
				p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
				return err
			}
			p.updateWindow()
			continue
		}

		timeout := audioReadTimeout
		if p.framer.Started() {
			if due := p.framer.NextPTS() - p.now(); due < timeout {
				timeout = due
			}
		}
		if timeout < audioMinReadTimeout {
			timeout = audioMinReadTimeout
		}

		fr, err := p.capturer.ReadFrame(timeout)
		if err != nil {
			switch {
			case errors.Is(err, audio.ErrTimeout):
				// 采集源一直没有数据：从现在开始锚定时间线，下一轮起补静音。
				if !p.cfg.NoSilence && !p.framer.Started() {
					p.framer.Anchor(p.now())
				}
				continue
			case errors.Is(err, audio.ErrDeviceLost):
				p.sink.OnAudioEvent(AudioEvent{Kind: AudioDeviceChanged, Desc: p.Desc(), Err: err})
				if err := p.reconnect(ctx, &backoff); err != nil {
					return err
				}
				continue
			default:
				p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
				return fmt.Errorf("ReadFrame: %w", err)
			}
		}
		backoff = audioReconnectMin

		if err := p.framer.Append(fr); err != nil {
			p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
			return fmt.Errorf("Framer.Append: %w", err)
		}
		for {
			f, ok := p.framer.Take()
			if !ok {
				break
			}
			if err := p.emitFrame(f); err != nil {
				p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
				return err
			}
		}
		p.syncDropped()
		p.updateWindow()
	}
}

// openCapturer 按当前配置打开一个新的采集源。Epoch 必须与管线一致，WASAPI
// 才知道怎么把 qpcPosition 换算成 PTS。
func (p *AudioPipeline) openCapturer() (audio.Capturer, error) {
	return p.newCapturer(audio.WASAPICapturerConfig{
		Epoch:    p.epoch,
		Channels: p.cfg.Channels,
	})
}

// setup 用采集源的实际格式建分帧器与编码器。只能在 Run 线程调用。
func (p *AudioPipeline) setup(format audio.Format) error {
	maxLatency := time.Duration(p.cfg.MaxLatencyMs) * time.Millisecond
	framer, err := audio.NewFramer(audio.FramerConfig{
		Format:     format,
		MaxLatency: maxLatency,
	})
	if err != nil {
		return fmt.Errorf("NewFramer: %w", err)
	}
	encoder, err := audio.NewOpusEncoder(audio.OpusEncoderConfig{
		SampleRate: audio.OpusSampleRate,
		Channels:   format.Channels,
		BitrateBps: p.cfg.BitrateBps,
		Complexity: p.cfg.Complexity,
		CBR:        p.cfg.CBR,
		DTX:        p.cfg.DTX,
	})
	if err != nil {
		return fmt.Errorf("NewOpusEncoder: %w", err)
	}

	p.framer = framer
	p.encoder = encoder
	p.format = format
	p.pcm = make([]float32, audio.OpusFrameSamples*format.Channels)
	p.setDesc(AudioDesc{
		Codec:      "opus",
		SampleRate: audio.OpusSampleRate,
		Channels:   format.Channels,
		BitrateBps: p.cfg.BitrateBps,
		FrameMs:    audio.OpusFrameMs,
		PreSkip:    encoder.PreSkip(),
	})
	return nil
}

// reconnect 销毁失效的采集源，按指数退避重建，直到成功或 ctx 取消。
// 等待期间保持静音节拍，避免 A/V 时间线长时间停顿。只能在 Run 线程调用。
func (p *AudioPipeline) reconnect(ctx context.Context, backoff *time.Duration) error {
	if p.capturer != nil {
		p.capturer.Release()
		p.capturer = nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		capturer, err := p.openCapturer()
		if err == nil {
			p.capturer = capturer
			if err := p.applyFormat(capturer.Format()); err != nil {
				p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
				return err
			}
			p.sink.OnAudioEvent(AudioEvent{Kind: AudioDeviceChanged, Desc: p.Desc()})
			return nil
		}

		p.sink.OnAudioEvent(AudioEvent{Kind: AudioDeviceChanged, Desc: p.Desc(), Err: err})
		if err := p.sleepWithSilence(ctx, *backoff); err != nil {
			return err
		}
		*backoff *= 2
		if *backoff > audioReconnectMax {
			*backoff = audioReconnectMax
		}
	}
}

// applyFormat 在采集源重建后按实际格式重建分帧器与编码器，并把新时间线锚定
// 在旧时间线的下一帧上，保证 PTS 连续。只能在 Run 线程调用。
func (p *AudioPipeline) applyFormat(format audio.Format) error {
	next := p.framer.NextPTS()
	maxLatency := time.Duration(p.cfg.MaxLatencyMs) * time.Millisecond
	framer, err := audio.NewFramer(audio.FramerConfig{Format: format, MaxLatency: maxLatency})
	if err != nil {
		return fmt.Errorf("NewFramer: %w", err)
	}
	framer.Anchor(next)

	encoder, err := audio.NewOpusEncoder(audio.OpusEncoderConfig{
		SampleRate: audio.OpusSampleRate,
		Channels:   format.Channels,
		BitrateBps: p.cfg.BitrateBps,
		Complexity: p.cfg.Complexity,
		CBR:        p.cfg.CBR,
		DTX:        p.cfg.DTX,
	})
	if err != nil {
		return fmt.Errorf("NewOpusEncoder: %w", err)
	}

	p.encoder.Release()
	p.framer = framer
	p.encoder = encoder
	p.format = format
	p.pcm = make([]float32, audio.OpusFrameSamples*format.Channels)
	p.setDesc(AudioDesc{
		Codec:      "opus",
		SampleRate: audio.OpusSampleRate,
		Channels:   format.Channels,
		BitrateBps: p.cfg.BitrateBps,
		FrameMs:    audio.OpusFrameMs,
		PreSkip:    encoder.PreSkip(),
	})
	return nil
}

// sleepWithSilence 等待 d，等待期间仍按 20 ms 节奏补静音。只能在 Run 线程调用。
func (p *AudioPipeline) sleepWithSilence(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !p.cfg.NoSilence {
			now := p.now()
			if !p.framer.Started() {
				p.framer.Anchor(now)
			}
			if p.framer.Available() == 0 && now >= p.framer.NextPTS() {
				if err := p.emitFrame(p.framer.Silence()); err != nil {
					return err
				}
				continue
			}
		}

		wait := time.Until(deadline)
		if wait <= 0 {
			return nil
		}
		if !p.cfg.NoSilence {
			if due := p.framer.NextPTS() - p.now(); due < wait {
				wait = due
			}
		}
		if wait < audioMinReadTimeout {
			wait = audioMinReadTimeout
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// emitFrame 把一帧 20 ms PCM 编码后交给 Sink。编码器输出指向内部缓冲，这里
// 复制一份，让 Sink 可以异步持有。只能在 Run 线程调用。
func (p *AudioPipeline) emitFrame(f *audio.Frame) error {
	n, err := p.format.ToFloat32(p.pcm, f.Data)
	if err != nil {
		return fmt.Errorf("Format.ToFloat32: %w", err)
	}

	t0 := time.Now()
	enc, err := p.encoder.Encode(p.pcm[:n], f.Timestamp)
	p.encodeUs.Store(uint64(time.Since(t0).Microseconds()))
	if err != nil {
		return fmt.Errorf("Encode: %w", err)
	}

	data := make([]byte, len(enc.Data))
	copy(data, enc.Data)
	out := &audio.EncodedFrame{
		Data:          data,
		Timestamp:     enc.Timestamp,
		Duration:      enc.Duration,
		Discontinuous: enc.Discontinuous,
	}

	p.frames.Add(1)
	p.bytes.Add(uint64(len(data)))
	if f.Silent {
		p.silentFrames.Add(1)
		p.silenceRun += f.Duration
		if !p.degraded && p.silenceRun >= audioSilenceDegradedAfter {
			p.degraded = true
			p.sink.OnAudioEvent(AudioEvent{Kind: AudioSilenceDegraded, Desc: p.Desc()})
		}
	} else {
		p.silenceRun = 0
		p.degraded = false
	}
	p.sink.OnAudioFrame(out)
	return nil
}

// drainCommands 处理上层投递过来的控制命令。只能在 Run 线程调用。
func (p *AudioPipeline) drainCommands() {
	for {
		select {
		case cmd := <-p.cmdCh:
			switch cmd.kind {
			case audioCmdBitrate:
				if err := p.encoder.SetBitrate(cmd.bps); err != nil {
					p.sink.OnAudioEvent(AudioEvent{Kind: AudioCaptureError, Err: err})
					continue
				}
				p.cfg.BitrateBps = cmd.bps
				p.descMu.Lock()
				p.desc.BitrateBps = cmd.bps
				p.descMu.Unlock()
			}
		default:
			return
		}
	}
}

func (p *AudioPipeline) setDesc(d AudioDesc) {
	p.descMu.Lock()
	p.desc = d
	p.descMu.Unlock()
}

// syncDropped 把 Framer 的累计丢弃量同步到统计。只能在 Run 线程调用。
func (p *AudioPipeline) syncDropped() {
	p.dropped.Store(uint64(p.framer.DroppedSamples()))
}

// updateWindow 每秒刷新一次实际码率。只能在 Run 线程调用。
func (p *AudioPipeline) updateWindow() {
	now := time.Now()
	elapsed := now.Sub(p.windowStart)
	if elapsed < audioBitrateWindow {
		return
	}
	bytes := p.bytes.Load()
	if delta := bytes - p.windowBytes; delta > 0 {
		p.actualBps.Store(uint64(float64(delta*8) / elapsed.Seconds()))
	} else {
		p.actualBps.Store(0)
	}
	p.windowStart = now
	p.windowBytes = bytes
}

func (p *AudioPipeline) now() time.Duration {
	return time.Since(p.epoch)
}
