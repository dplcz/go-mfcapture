package mfcapture

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"

	"mfcapture/capture"
	"mfcapture/encode"
	"mfcapture/mf"
)

// StreamFormat 表示编码输出中 NAL 单元的封装方式。
type StreamFormat uint8

const (
	// StreamFormatAnnexB：NAL 以 00 00 00 01 起始码分隔，SPS/PPS 内联在每个 IDR 之前。
	// 实测 Media Foundation 的 H264 编码器 MFT 输出的正是这种格式。
	StreamFormatAnnexB StreamFormat = iota
	// StreamFormatAVCC：NAL 以 4 字节长度前缀分隔，参数集放在 Extradata 里。
	// 当前实现不会产生这种格式，保留该值仅用于协议上报。
	StreamFormatAVCC
)

// VideoDesc 描述一条视频流的解码参数，供对端初始化解码器。
type VideoDesc struct {
	Codec        string // 恒为 "H264"
	Profile      uint32 // profile_idc，从流内 SPS 解析；首个关键帧到达前为 0
	Level        uint32 // level_idc，同上
	Width        uint32
	Height       uint32
	FPSNum       uint32
	FPSDen       uint32
	StreamFormat StreamFormat
	// Extradata 为 out-of-band 参数集。当前恒为 nil：SPS/PPS 随每个 IDR 内联下发，
	// 对端等第一个关键帧即可起播，不需要额外带外传参。
	Extradata []byte
}

// EventKind 标识下层主动上报的状态变化。
type EventKind uint8

const (
	// EventDescReady 在任何 OnFrame 之前触发一次，此时 Desc() 的宽高/帧率已可用。
	EventDescReady EventKind = iota
	// EventResolutionChanged 采集分辨率变化后触发。下层已自行重建好编码器，
	// 上层只需重新取 Desc() 下发给对端；不需要额外请求关键帧（新编码器首帧即 IDR）。
	EventResolutionChanged
	// EventEncoderDegraded 没有找到硬件编码器 MFT、回退到软件编码时触发。
	EventEncoderDegraded
	// EventCaptureError 采集或编码发生非致命错误时触发。Err 为具体错误。
	EventCaptureError
)

// Event 是下层向上层主动上报的状态变化。
type Event struct {
	Kind EventKind
	Desc VideoDesc // DescReady / ResolutionChanged 时有效
	Err  error     // CaptureError 时有效
}

// Sink 是下层向上层交付数据的唯一通道。由上层实现。
//
// 下层不做队列、不丢帧、不限流——它只保证"编码出一帧就调一次 OnFrame"。
// 是否阻塞、队列多深、满了丢什么，全部是上层自己的策略，改策略不需要动本包。
type Sink interface {
	// OnFrame 在 Run() 所在线程内同步调用。上层实现若在此阻塞，等于自己选择丢弃采集帧。
	OnFrame(f *encode.EncodedFrame)
	// OnCursor 在独立的轮询 goroutine 内调用，与 OnFrame 不同线程，实现需自行保证并发安全。
	OnCursor(c *capture.CursorInfo)
	// OnEvent 上报状态变化。OnEvent(EventDescReady) 保证早于第一个 OnFrame。
	OnEvent(e Event)
}

// nopSink 供基准测试/调试使用；上层不传 Sink 时用它兜底，使回调点无需判空。
type nopSink struct{}

func (nopSink) OnFrame(*encode.EncodedFrame) {}
func (nopSink) OnCursor(*capture.CursorInfo) {}
func (nopSink) OnEvent(Event)                {}

// Stats 是采集/编码的累计统计快照，供上层做自适应码率与运维观测。
type Stats struct {
	Frames     uint64 // 已交付的编码帧数
	KeyFrames  uint64
	Bytes      uint64 // 已交付的编码字节数
	EncodeUs   uint64 // 最近一帧的编码耗时（微秒）
	IsHardware bool   // 是否使用硬件编码器
}

// PipelineConfig 配置一条采集→编码管线。
type PipelineConfig struct {
	Output     uint   // 显示器索引，取值来自 capture.EnumMonitors() 的 Index
	BitrateBps uint32 // 目标码率；0 = 8 Mbps
	// MaxFPS 是写入帧率上限，0 = 不限。按固定节拍限流，允许帧比节拍早到 10% 个间隔。
	//
	// 注意：编码器是按 FPSNum/FPSDen 声明的帧率给每帧分配码率预算的。若 MaxFPS=0 而
	// 屏幕实际变化很快，真实帧率会远高于 FPSNum，实际码率 ≈ BitrateBps × (实际fps/FPSNum)。
	// 实测：声明 30fps、配置 8 Mbps、不限帧率时，屏幕高频变化下实际跑到约 100fps / 20 Mbps。
	// 要码率可控就让 MaxFPS 与 FPSNum 一致（或直接用 Stats.Frames 自己观测）。
	MaxFPS     uint32
	FPSNum     uint32 // 0 = 30
	FPSDen     uint32 // 0 = 1
	GOPSize    uint32 // 关键帧间隔（帧数）；0 = 编码器默认，首帧可能来得较晚
	LowLatency bool   // 低延迟模式；零值为 false
	CursorFPS  uint32 // 光标轮询频率；0 = 30
}

// Pipeline 是一条"屏幕 → H264 字节 + 光标"的管线。
//
// 线程模型（这是本类型最重要的约定）：
//   - NewPipeline 只做纯构造，不碰 COM，可在任意 goroutine 调用。
//   - Run 会调用 runtime.LockOSThread() 把调用方 goroutine 绑定到 OS 线程，并在该线程上
//     完成 COM/MF 初始化、建资源、跑循环、清理，全程不换线程。COM/MF 的初始化与释放都
//     发生在 Run 内部，因此不会出现"初始化在 A 线程、释放在 B 线程"的公寓错配。
//   - RequestKeyFrame / SetBitrate / Stats / Desc 可在任意 goroutine 调用；前两者通过
//     命令队列投递到 Run 所在线程执行，绝不跨线程直接调 COM。
//   - 停止管线请 cancel 传给 Run 的 ctx；Close 只需在 Run 返回后调用一次。
type Pipeline struct {
	cfg  PipelineConfig
	sink Sink

	cmdCh  chan pipelineCmd
	closed atomic.Bool

	frames    atomic.Uint64
	keyFrames atomic.Uint64
	bytes     atomic.Uint64
	encodeUs  atomic.Uint64
	hw        atomic.Bool

	descMu sync.RWMutex
	desc   VideoDesc

	// 以下字段只在 Run() 所在 goroutine 内访问。
	capturer  capture.Capturer
	encoder   *encode.H264MFTEncoder
	cursorMon *capture.CursorMonitor
	device    *d3d11.ID3D11Device
	deviceCtx *d3d11.ID3D11DeviceContext
	devMgr    *mf.IMFDXGIDeviceManager
	resetTok  uint32
}

type pipelineCmdKind uint8

const (
	pipelineCmdKeyFrame pipelineCmdKind = iota
	pipelineCmdBitrate
)

type pipelineCmd struct {
	kind pipelineCmdKind
	bps  uint32
}

const commandQueueSize = 16

// NewPipeline 构造一条管线。不做任何 COM/MF 调用，也不会失败于环境（除参数校验外）。
// sink 为 nil 时所有回调变成 no-op（仅建议基准测试使用）。
func NewPipeline(cfg PipelineConfig, sink Sink) (*Pipeline, error) {
	if cfg.BitrateBps == 0 {
		cfg.BitrateBps = 8_000_000
	}
	if cfg.FPSNum == 0 {
		cfg.FPSNum = 30
	}
	if cfg.FPSDen == 0 {
		cfg.FPSDen = 1
	}
	if sink == nil {
		sink = nopSink{}
	}
	return &Pipeline{
		cfg:   cfg,
		sink:  sink,
		cmdCh: make(chan pipelineCmd, commandQueueSize),
	}, nil
}

// Desc 返回当前视频流描述。在收到 EventDescReady 之前返回零值。
func (p *Pipeline) Desc() VideoDesc {
	p.descMu.RLock()
	defer p.descMu.RUnlock()
	return p.desc
}

// Stats 返回累计统计快照，可在任意 goroutine 调用。
func (p *Pipeline) Stats() Stats {
	return Stats{
		Frames:     p.frames.Load(),
		KeyFrames:  p.keyFrames.Load(),
		Bytes:      p.bytes.Load(),
		EncodeUs:   p.encodeUs.Load(),
		IsHardware: p.hw.Load(),
	}
}

// RequestKeyFrame 请求下一个编码帧为 IDR。可在任意 goroutine 调用；
// 通过命令队列投递到采集线程执行，因此是异步生效的（延迟不超过一次采集超时，默认 500ms）。
func (p *Pipeline) RequestKeyFrame() {
	if p.closed.Load() {
		return
	}
	select {
	case p.cmdCh <- pipelineCmd{kind: pipelineCmdKeyFrame}:
	default: // 队列满说明 Run 已停止，丢弃即可
	}
}

// SetBitrate 运行时调整目标码率。可在任意 goroutine 调用，同样通过命令队列异步生效。
func (p *Pipeline) SetBitrate(bps uint32) {
	if p.closed.Load() {
		return
	}
	select {
	case p.cmdCh <- pipelineCmd{kind: pipelineCmdBitrate, bps: bps}:
	default:
	}
}

// Close 标记管线已关闭，之后的 RequestKeyFrame/SetBitrate 变成 no-op。
// 应在 Run 返回之后调用；Run 内部的资源释放由 Run 自己完成，Close 不释放 COM 资源。
// 注意：命令通道不会 close，避免"先关闭后投递"导致 panic。
func (p *Pipeline) Close() error {
	p.closed.Store(true)
	return nil
}

const (
	acquireTimeoutMs  = 500
	captureRetryDelay = 250 * time.Millisecond
)

// waitCaptureRetry waits before retrying a temporarily unavailable desktop
// duplication session, while still allowing Run to stop promptly.
func waitCaptureRetry(ctx context.Context) error {
	timer := time.NewTimer(captureRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run 在调用方 goroutine 上运行整条管线，直到 ctx 被取消。它是阻塞的。
//
// 返回的 error 为 ctx.Err()（正常结束）或具体的采集/编码错误。
func (p *Pipeline) Run(ctx context.Context) error {
	// COM/MF 的初始化与释放全部收在本函数内，保证同线程进出。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if hr := mf.CoInitializeEx(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("CoInitializeEx: %w", d3d.HRESULT(hr))
	}
	defer mf.CoUninitialize()

	if hr := mf.MFStartup(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("MFStartup: %w", d3d.HRESULT(hr))
	}
	defer mf.MFShutdown()

	device, deviceCtx, err := capture.NewVideoCapableD3D11Device()
	if err != nil {
		return fmt.Errorf("NewVideoCapableD3D11Device: %w", err)
	}
	defer device.Release()
	defer deviceCtx.Release()
	p.device, p.deviceCtx = device, deviceCtx

	if err := capture.EnableMultithreadProtection(device); err != nil {
		return fmt.Errorf("EnableMultithreadProtection: %w", err)
	}

	var resetTok uint32
	var devMgr *mf.IMFDXGIDeviceManager
	if hr := mf.MFCreateDXGIDeviceManager(&resetTok, &devMgr); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("MFCreateDXGIDeviceManager: %w", d3d.HRESULT(hr))
	}
	defer devMgr.Release()
	if hr := devMgr.ResetDevice(device, resetTok); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("IMFDXGIDeviceManager.ResetDevice: %w", d3d.HRESULT(hr))
	}
	p.devMgr, p.resetTok = devMgr, resetTok

	var dup *capture.Duplicator
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dup, err = capture.NewDuplicator(device, deviceCtx, p.cfg.Output)
		if err == nil {
			break
		}
		if !errors.Is(err, capture.ErrCaptureUnavailable) {
			return fmt.Errorf("NewDuplicator: %w", err)
		}
		if err := waitCaptureRetry(ctx); err != nil {
			return err
		}
	}
	defer dup.Release()
	p.capturer = dup

	if err := p.buildEncoder(dup.Width(), dup.Height()); err != nil {
		return err
	}
	defer func() {
		if p.encoder != nil {
			p.encoder.Release()
			p.encoder = nil
		}
	}()

	p.hw.Store(p.encoder.IsHardware)
	p.setDesc(VideoDesc{
		Codec:        "H264",
		Width:        dup.Width(),
		Height:       dup.Height(),
		FPSNum:       p.cfg.FPSNum,
		FPSDen:       p.cfg.FPSDen,
		StreamFormat: StreamFormatAnnexB,
	})
	p.sink.OnEvent(Event{Kind: EventDescReady, Desc: p.Desc()})
	if !p.encoder.IsHardware {
		p.sink.OnEvent(Event{Kind: EventEncoderDegraded})
	}

	p.cursorMon = capture.NewCursorMonitor(p.sink.OnCursor, p.cfg.CursorFPS)
	cursorCtx, stopCursor := context.WithCancel(ctx)
	defer stopCursor()
	go p.cursorMon.Start(cursorCtx)
	defer p.cursorMon.Stop()

	limiter := newFrameLimiter(p.cfg.MaxFPS)

	start := time.Now()
	lastHns := int64(-1)

	for {
		if err := ctx.Err(); err != nil {
			return p.drainEncoder(err)
		}
		p.drainCommands()

		frame, err := p.capturer.AcquireFrame(acquireTimeoutMs)
		p.drainCommands()
		if err != nil {
			if errors.Is(err, capture.ErrTimeout) {
				continue
			}
			if errors.Is(err, capture.ErrCaptureUnavailable) {
				if err := waitCaptureRetry(ctx); err != nil {
					return err
				}
				continue
			}
			if errors.Is(err, capture.ErrAccessLostRecovered) {
				// 采集会话已重建（锁屏/分辨率/模式变化）。分辨率可能变了，
				// 编码器输入类型必须跟着重建，否则后续每帧都会失败。
				if err := p.handleResolutionChange(); err != nil {
					return err
				}
				_ = p.encoder.ForceKeyFrame()
				continue
			}
			p.sink.OnEvent(Event{Kind: EventCaptureError, Err: err})
			return fmt.Errorf("AcquireFrame: %w", err)
		}

		hns := time.Since(start).Nanoseconds() / 100
		if !limiter.allow(hns) {
			continue
		}

		sample, err := wrapTextureAsSample(frame, hns, lastHns)
		if err != nil {
			p.sink.OnEvent(Event{Kind: EventCaptureError, Err: err})
			return fmt.Errorf("wrapTextureAsSample: %w", err)
		}

		t0 := time.Now()
		encoded, encErr := p.encoder.Encode(sample)
		p.encodeUs.Store(uint64(time.Since(t0).Microseconds()))
		sample.Release()
		if encErr != nil {
			p.sink.OnEvent(Event{Kind: EventCaptureError, Err: encErr})
			return fmt.Errorf("Encode: %w", encErr)
		}

		lastHns = hns
		if encoded != nil {
			p.deliver(encoded)
		}
	}
}

// drainEncoder 排空编码器缓冲后返回。ctx 取消时调用。
func (p *Pipeline) drainEncoder(cause error) error {
	remaining, _ := p.encoder.Flush()
	for _, f := range remaining {
		p.deliver(f)
	}
	return cause
}

// buildEncoder 释放旧编码器并按新分辨率重建。只能在 Run 所在 goroutine 调用。
func (p *Pipeline) buildEncoder(width, height uint32) error {
	if p.encoder != nil {
		p.encoder.Release()
		p.encoder = nil
	}
	enc, err := encode.NewH264MFTEncoder(encode.H264EncoderConfig{
		Width:      width,
		Height:     height,
		FPSNum:     p.cfg.FPSNum,
		FPSDen:     p.cfg.FPSDen,
		BitrateBps: p.cfg.BitrateBps,
		LowLatency: p.cfg.LowLatency,
		GOPSize:    p.cfg.GOPSize,
	}, p.device, p.devMgr)
	if err != nil {
		return fmt.Errorf("NewH264MFTEncoder: %w", err)
	}
	p.encoder = enc
	return nil
}

// handleResolutionChange 在 ACCESS_LOST 恢复后按需重建编码器并上报新分辨率。
func (p *Pipeline) handleResolutionChange() error {
	w, h := p.capturer.Width(), p.capturer.Height()
	old := p.Desc()
	if w == old.Width && h == old.Height {
		return nil
	}
	if err := p.buildEncoder(w, h); err != nil {
		p.sink.OnEvent(Event{Kind: EventCaptureError, Err: err})
		return err
	}
	p.hw.Store(p.encoder.IsHardware)
	p.setDesc(VideoDesc{
		Codec:        "H264",
		Width:        w,
		Height:       h,
		FPSNum:       p.cfg.FPSNum,
		FPSDen:       p.cfg.FPSDen,
		StreamFormat: StreamFormatAnnexB,
	})
	p.sink.OnEvent(Event{Kind: EventResolutionChanged, Desc: p.Desc()})
	return nil
}

// drainCommands 处理上层投递过来的控制命令。只能在 Run 所在 goroutine 调用。
func (p *Pipeline) drainCommands() {
	for {
		select {
		case cmd := <-p.cmdCh:
			switch cmd.kind {
			case pipelineCmdKeyFrame:
				_ = p.encoder.ForceKeyFrame()
			case pipelineCmdBitrate:
				if err := p.encoder.SetBitrate(cmd.bps); err != nil {
					p.sink.OnEvent(Event{Kind: EventCaptureError, Err: err})
				}
			}
		default:
			return
		}
	}
}

func (p *Pipeline) setDesc(d VideoDesc) {
	p.descMu.Lock()
	p.desc = d
	p.descMu.Unlock()
}

// deliver 更新统计并把帧交给上层。
func (p *Pipeline) deliver(f *encode.EncodedFrame) {
	p.frames.Add(1)
	p.bytes.Add(uint64(len(f.Data)))
	if f.IsKeyFrame {
		p.keyFrames.Add(1)
		if profile, level, ok := parseSPSProfileLevel(f.Data); ok {
			p.descMu.Lock()
			p.desc.Profile, p.desc.Level = profile, level
			p.descMu.Unlock()
		}
	}
	p.sink.OnFrame(f)
}

// parseSPSProfileLevel 从 Annex-B 片段中定位第一个 SPS，读出 profile_idc 与 level_idc。
// SPS NAL 的载荷布局：payload[0]=profile_idc，payload[1]=约束标志，payload[2]=level_idc。
func parseSPSProfileLevel(b []byte) (profile, level uint32, ok bool) {
	for i := 0; i < len(b); {
		j := bytes.Index(b[i:], []byte{0x00, 0x00, 0x01})
		if j < 0 {
			return 0, 0, false
		}
		pos := i + j
		// start code(3) + NAL header(1) + profile_idc + constraints + level_idc
		if pos+6 < len(b) && b[pos+3]&0x1F == 7 {
			return uint32(b[pos+4]), uint32(b[pos+6]), true
		}
		i = pos + 3
	}
	return 0, 0, false
}

const nominalFrameDurationHns = int64(10_000_000) / 30

func wrapTextureAsSample(frame *capture.CapturedFrame, hns, lastHns int64) (*mf.IMFSample, error) {
	var buffer *mf.IMFMediaBuffer
	if hr := mf.MFCreateDXGISurfaceBuffer(
		d3d11.IID_ID3D11Texture2D,
		unsafe.Pointer(frame.Texture),
		0, false, &buffer,
	); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("MFCreateDXGISurfaceBuffer: %w", d3d.HRESULT(hr))
	}

	var desc d3d11.D3D11_TEXTURE2D_DESC
	frame.Texture.GetDesc(&desc)
	buffer.SetCurrentLength(desc.Width * desc.Height * 4)

	var sample *mf.IMFSample
	if hr := mf.MFCreateSample(&sample); d3d.HRESULT(hr).Failed() {
		buffer.Release()
		return nil, fmt.Errorf("MFCreateSample: %w", d3d.HRESULT(hr))
	}
	sample.AddBuffer(buffer)
	buffer.Release()

	duration := nominalFrameDurationHns
	if lastHns >= 0 {
		if d := hns - lastHns; d > 0 {
			duration = d
		}
	}
	sample.SetSampleTime(hns)
	sample.SetSampleDuration(duration)

	return sample, nil
}
