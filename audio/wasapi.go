//go:build windows

package audio

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WASAPICapturerConfig 配置 loopback 采集。
type WASAPICapturerConfig struct {
	// Epoch 是 Frame.Timestamp 的零点，零值取 NewWASAPICapturer 的调用时刻。
	// 视频管线必须用同一个 Epoch，A/V 同步才有意义。
	Epoch time.Time
	// Channels 是目标格式的声道数（1 或 2），零值 2。系统混音格式回退时
	// 以设备实际声道数为准。
	Channels int
}

// WASAPICapturer 用 WASAPI loopback 采集默认渲染端点的全局混音，实现 Capturer。
//
// 调用方必须在当前线程初始化 COM（COINIT_MULTITHREADED，由 AudioPipeline.Run
// 负责），并在同一线程调用构造、ReadFrame 和 Release——WASAPI 的接口指针
// 与创建它的 apartment 绑定。Release 不会调用 CoUninitialize。
//
// 不是并发安全的。ReadFrame 返回的 Frame.Data 指向 WASAPI 内部缓冲，只在下
// 一次 ReadFrame（它会先 ReleaseBuffer）之前有效；Framer.Append 会复制。
type WASAPICapturer struct {
	cfg    WASAPICapturerConfig
	format Format

	enumerator *iMMDeviceEnumerator
	device     *iMMDevice
	client     *iAudioClient
	capture    *iAudioCaptureClient
	event      windows.Handle

	started  bool
	released bool
	pending  uint32 // 已被 GetBuffer 锁定、等待下一次 ReadFrame 释放的帧数
	silent   []byte // SILENT 包里 Data 不可读，用它提供全零 PCM

	clock qpcClock
}

var _ Capturer = (*WASAPICapturer)(nil)

// NewWASAPICapturer 打开默认渲染端点的 loopback 流并启动采集。
//
// 优先请求 48 kHz / F32 / cfg.Channels 并用 AUTOCONVERTPCM 让 Windows 完成
// 重采样与下混；若被拒绝，则回退到设备混音格式，仅接受 48 kHz 且 1/2 声道
// 的 16-bit PCM 或 32-bit float。其余情况返回错误——本层还没有重采样。
func NewWASAPICapturer(cfg WASAPICapturerConfig) (*WASAPICapturer, error) {
	if cfg.Channels == 0 {
		cfg.Channels = 2
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		return nil, fmt.Errorf("wasapi: channels %d, want 1 or 2", cfg.Channels)
	}
	if cfg.Epoch.IsZero() {
		cfg.Epoch = time.Now()
	}

	c := &WASAPICapturer{
		cfg:   cfg,
		clock: qpcClock{epoch: cfg.Epoch},
	}
	if err := c.open(); err != nil {
		c.Release()
		return nil, err
	}
	return c, nil
}

func (c *WASAPICapturer) open() error {
	enumerator, hr := coCreateEnumerator()
	if failedHR(hr) {
		return wasapiError("CoCreateInstance(IMMDeviceEnumerator)", hr)
	}
	c.enumerator = enumerator

	device, hr := enumerator.GetDefaultAudioEndpoint(eRender, eConsole)
	if failedHR(hr) {
		return wasapiError("GetDefaultAudioEndpoint(eRender, eConsole)", hr)
	}
	c.device = device

	client, err := c.activateClient()
	if err != nil {
		return err
	}
	c.client = client

	target := buildTargetFormat(c.cfg.Channels)
	flags := uint32(streamFlagLoopback | streamFlagEventCallback |
		streamFlagAutoConvertPCM | streamFlagSRCDefault)
	hr = client.Initialize(audioShareModeShared, flags, 0, 0, unsafe.Pointer(&target), nil)
	if failedHR(hr) {
		targetHR := hr

		// Initialize 失败后同一个 IAudioClient 可能永久返回
		// E_ALREADY_INITIALIZED（见 MSDN），必须重新 Activate。
		client.Release()
		c.client = nil
		client, err = c.activateClient()
		if err != nil {
			return err
		}
		c.client = client

		mix, mixHR := client.GetMixFormat()
		if failedHR(mixHR) {
			return wasapiError("GetMixFormat", mixHR)
		}
		format, parseErr := waveFormatToFormat(mix)
		if parseErr == nil && format.SampleRate != OpusSampleRate {
			parseErr = fmt.Errorf("sample rate %d, want %d (resampling is not implemented)",
				format.SampleRate, OpusSampleRate)
		}
		if parseErr != nil {
			coTaskMemFree(mix)
			return fmt.Errorf("wasapi: 48 kHz target rejected (HRESULT 0x%08X), mix format unusable: %w",
				uint32(targetHR), parseErr)
		}

		// mix 已经是设备混音格式，不需要 AUTOCONVERTPCM。
		mixFlags := uint32(streamFlagLoopback | streamFlagEventCallback)
		mixHR = client.Initialize(audioShareModeShared, mixFlags, 0, 0, mix, nil)
		coTaskMemFree(mix) // Initialize 返回后格式描述即可释放
		if failedHR(mixHR) {
			return fmt.Errorf("wasapi: Initialize with the mix format failed (HRESULT 0x%08X); "+
				"the 48 kHz target had failed with 0x%08X", uint32(mixHR), uint32(targetHR))
		}
		c.format = format
	} else {
		c.format = Format{SampleRate: OpusSampleRate, Channels: c.cfg.Channels, Float32: true}
	}

	capture, hr := client.GetService(&iidAudioCaptureClient)
	if failedHR(hr) {
		return wasapiError("GetService(IAudioCaptureClient)", hr)
	}
	c.capture = (*iAudioCaptureClient)(capture)

	event, err := windows.CreateEvent(nil, 0, 0, nil) // 自动重置、初始未触发
	if err != nil {
		return fmt.Errorf("wasapi: CreateEvent: %w", err)
	}
	c.event = event

	if hr := client.SetEventHandle(event); failedHR(hr) {
		return wasapiError("SetEventHandle", hr)
	}
	if hr := client.Start(); failedHR(hr) {
		return wasapiError("Start", hr)
	}
	c.started = true
	return nil
}

func (c *WASAPICapturer) activateClient() (*iAudioClient, error) {
	var p unsafe.Pointer
	hr := c.device.Activate(&iidAudioClient, clsctxAll, nil, &p)
	if failedHR(hr) {
		return nil, wasapiError("IMMDevice.Activate(IAudioClient)", hr)
	}
	return (*iAudioClient)(p), nil
}

// Format 返回实际协商到的 PCM 格式。目标格式被拒时可能是设备的混音格式，
// 例如 48 kHz 16-bit PCM；调用方用 Format.ToFloat32 统一转换。
func (c *WASAPICapturer) Format() Format { return c.format }

// ReadFrame 读取一个 WASAPI 包。没有数据时最多等待 timeout（<=0 表示只
// 检查一次），然后返回 ErrTimeout。设备失效时返回包裹 ErrDeviceLost 的错误。
//
// 事件驱动 + 自动重置事件：GetNextPacketSize 为 0 时阻塞在事件上，WASAPI
// 大约每个引擎周期（通常 10 ms）唤醒一次。shared 模式的包长由引擎决定，
// 可能是 10 ms 也可能是 30 ms，分帧交给 Framer。
func (c *WASAPICapturer) ReadFrame(timeout time.Duration) (*Frame, error) {
	if c.released {
		return nil, errors.New("wasapi: capturer already released")
	}
	if c.capture == nil {
		return nil, errors.New("wasapi: capturer is not initialized")
	}

	// 上一包的有效期到本次调用为止。
	if c.pending > 0 {
		pending := c.pending
		c.pending = 0
		if hr := c.capture.ReleaseBuffer(pending); failedHR(hr) {
			return nil, wasapiError("ReleaseBuffer", hr)
		}
	}

	deadline := time.Now().Add(timeout)
	for {
		frames, hr := c.capture.GetNextPacketSize()
		if failedHR(hr) {
			return nil, wasapiError("GetNextPacketSize", hr)
		}
		if frames > 0 {
			break
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrTimeout
		}
		ms := uint32((remaining + time.Millisecond - 1) / time.Millisecond)
		woken, err := windows.WaitForSingleObject(c.event, ms)
		if err != nil {
			return nil, fmt.Errorf("wasapi: WaitForSingleObject: %w", err)
		}
		switch woken {
		case windows.WAIT_OBJECT_0:
			// 有新的引擎周期，回到循环检查包大小。
		case waitTimeout:
			return nil, ErrTimeout
		default:
			return nil, fmt.Errorf("wasapi: WaitForSingleObject returned %#x", woken)
		}
	}

	data, frames, flags, qpc, hr := c.capture.GetBuffer()
	if failedHR(hr) {
		return nil, wasapiError("GetBuffer", hr)
	}
	if frames == 0 || uint32(hr) == audioClientBufferEmpty {
		// GetBuffer 成功但没给帧时仍要归还锁；S_BUFFER_EMPTY 没有锁，忽略错误即可。
		if frames == 0 {
			_ = c.capture.ReleaseBuffer(0)
		}
		return nil, ErrTimeout
	}

	dur := samplesToDuration(int(frames), c.format.SampleRate)
	var ts time.Duration
	if flags&bufferFlagTimestampError != 0 || qpc == 0 {
		// 引擎没能给出可靠的时间戳：按包时长往后接，保证时间线连续。
		ts = c.clock.byDuration(dur)
	} else {
		ts = c.clock.at(qpc, dur)
	}

	n := int(frames) * c.format.BlockAlign()
	if flags&bufferFlagSilent != 0 {
		// SILENT 包里的 data 指针不可读，必须自己给全零 PCM。
		if cap(c.silent) < n {
			c.silent = make([]byte, n)
		} else {
			c.silent = c.silent[:n]
		}
		c.pending = frames
		return &Frame{Data: c.silent, Timestamp: ts, Duration: dur, Silent: true}, nil
	}
	if data == nil {
		_ = c.capture.ReleaseBuffer(frames)
		return nil, errors.New("wasapi: GetBuffer returned no data without the SILENT flag")
	}

	c.pending = frames
	return &Frame{
		Data:      unsafe.Slice((*byte)(data), n),
		Timestamp: ts,
		Duration:  dur,
	}, nil
}

// Release 停止采集并释放所有 COM 接口和事件句柄。可重复调用；不调用
// CoUninitialize——apartment 的初始化/清理属于调用方。
func (c *WASAPICapturer) Release() {
	if c == nil || c.released {
		return
	}
	c.released = true
	c.releaseResources()
}

func (c *WASAPICapturer) releaseResources() {
	if c.pending > 0 && c.capture != nil {
		_ = c.capture.ReleaseBuffer(c.pending)
	}
	c.pending = 0

	if c.started && c.client != nil {
		_ = c.client.Stop()
	}
	c.started = false

	if c.capture != nil {
		_ = c.capture.Release()
		c.capture = nil
	}
	if c.client != nil {
		_ = c.client.Release()
		c.client = nil
	}
	if c.device != nil {
		_ = c.device.Release()
		c.device = nil
	}
	if c.enumerator != nil {
		_ = c.enumerator.Release()
		c.enumerator = nil
	}
	if c.event != 0 {
		_ = windows.CloseHandle(c.event)
		c.event = 0
	}
}

// buildTargetFormat 构造 48 kHz / F32 的 WAVEFORMATEXTENSIBLE。
func buildTargetFormat(channels int) waveFormatExtensible {
	blockAlign := uint16(4 * channels)
	mask := uint32(speakerFrontLeft | speakerFrontRight)
	if channels == 1 {
		mask = speakerFrontCenter
	}
	return waveFormatExtensible{
		FormatTag:      waveFormatTagExtensible,
		Channels:       uint16(channels),
		SamplesPerSec:  OpusSampleRate,
		AvgBytesPerSec: OpusSampleRate * uint32(blockAlign),
		BlockAlign:     blockAlign,
		BitsPerSample:  32,
		CBSize:         22,
		Samples:        32,
		ChannelMask:    mask,
		SubFormat:      ksDataFormatSubtypeIEEEFloat,
	}
}

// waveFormatToFormat 解析 GetMixFormat 返回的 WAVEFORMATEX 或
// WAVEFORMATEXTENSIBLE。只支持本链路能直接消费的两种采样编码。
func waveFormatToFormat(p unsafe.Pointer) (Format, error) {
	if p == nil {
		return Format{}, errors.New("wasapi: nil WAVEFORMATEX")
	}
	base := (*waveFormatEx)(p)
	f := Format{
		SampleRate: int(base.SamplesPerSec),
		Channels:   int(base.Channels),
	}

	switch base.FormatTag {
	case waveFormatTagIEEEFloat:
		if base.BitsPerSample != 32 {
			return Format{}, fmt.Errorf("wasapi: IEEE float with %d bits, want 32", base.BitsPerSample)
		}
		f.Float32 = true
	case waveFormatTagPCM:
		if base.BitsPerSample != 16 {
			return Format{}, fmt.Errorf("wasapi: PCM with %d bits, want 16", base.BitsPerSample)
		}
	case waveFormatTagExtensible:
		if base.CBSize < 22 {
			return Format{}, fmt.Errorf("wasapi: WAVEFORMATEXTENSIBLE cbSize=%d, want >= 22", base.CBSize)
		}
		ext := (*waveFormatExtensible)(p)
		switch ext.SubFormat {
		case ksDataFormatSubtypeIEEEFloat:
			if base.BitsPerSample != 32 {
				return Format{}, fmt.Errorf("wasapi: extensible float with %d bits, want 32", base.BitsPerSample)
			}
			f.Float32 = true
		case ksDataFormatSubtypePCM:
			if base.BitsPerSample != 16 {
				return Format{}, fmt.Errorf("wasapi: extensible PCM with %d bits, want 16", base.BitsPerSample)
			}
		default:
			return Format{}, fmt.Errorf("wasapi: unsupported WAVEFORMATEXTENSIBLE subformat %v", ext.SubFormat)
		}
	default:
		return Format{}, fmt.Errorf("wasapi: unsupported WAVE format tag 0x%04X", base.FormatTag)
	}

	if err := f.Validate(); err != nil {
		return Format{}, err
	}
	return f, nil
}

func failedHR(hr int32) bool { return hr < 0 }

func wasapiError(op string, hr int32) error {
	if isDeviceLostHR(hr) {
		return fmt.Errorf("%w: %s (HRESULT 0x%08X)", ErrDeviceLost, op, uint32(hr))
	}
	return fmt.Errorf("wasapi: %s (HRESULT 0x%08X)", op, uint32(hr))
}

func isDeviceLostHR(hr int32) bool {
	switch uint32(hr) {
	case audioClientDeviceInvalidated, audioClientResourcesInvalidated,
		audioClientServiceNotRunning, audioClientEndpointCreateFail:
		return true
	}
	return false
}

// qpcClock 把 WASAPI 的 QPC 时间戳换算成相对 epoch 的 PTS。
//
// WASAPI 给出的 qpcPosition 是以 100 ns 为单位的性能计数器时刻（首采样被
// 引擎记录的时间），不是原始计数器值。首包还要扣掉"从采集到我们读到它"
// 的延迟，否则音频会整体晚一个引擎周期，影响唇音同步。
//
// nowFn / qpcFn 只为单测注入；正常路径分别用 time.Since(epoch) 和
// QueryPerformanceCounter。
type qpcClock struct {
	epoch time.Time
	nowFn func() time.Duration
	qpcFn func() (uint64, bool)

	started bool
	qpc     uint64        // 锚点包的 qpcPosition
	pts     time.Duration // 锚点包首采样的 PTS
	last    time.Duration // 最近一个包的 PTS
	lastDur time.Duration // 最近一个包的时长
}

func (c *qpcClock) now() time.Duration {
	if c.nowFn != nil {
		return c.nowFn()
	}
	return time.Since(c.epoch)
}

func (c *qpcClock) qpcNow() (uint64, bool) {
	if c.qpcFn != nil {
		return c.qpcFn()
	}
	return queryQPC100ns()
}

// at 返回 qpc 对应的 PTS，并保证不早于上一个包的结束。
func (c *qpcClock) at(qpc uint64, dur time.Duration) time.Duration {
	if !c.started {
		c.started = true
		now := c.now()
		if nowQPC, ok := c.qpcNow(); ok && nowQPC >= qpc {
			now -= time.Duration(nowQPC-qpc) * 100 * time.Nanosecond
		}
		if now < 0 {
			now = 0 // epoch 之前开始的包不应该把 PTS 拉成负数
		}
		c.qpc, c.pts, c.last = qpc, now, now
		c.lastDur = dur
		return now
	}

	pts := c.pts
	if qpc >= c.qpc {
		pts += time.Duration(qpc-c.qpc) * 100 * time.Nanosecond
	}
	if floor := c.last + c.lastDur; pts < floor {
		pts = floor
	}
	c.last = pts
	c.lastDur = dur
	return pts
}

// byDuration 在引擎没给可靠时间戳时按包时长推进时间线。
func (c *qpcClock) byDuration(dur time.Duration) time.Duration {
	if !c.started {
		c.started = true
		c.last = c.now()
		if c.last < 0 {
			c.last = 0
		}
		c.pts = c.last
	} else {
		c.last += c.lastDur
	}
	c.lastDur = dur
	return c.last
}
