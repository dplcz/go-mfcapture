//go:build windows

package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// WASAPIRendererConfig 配置 WASAPI 共享模式播放。
type WASAPIRendererConfig struct {
	// Channels 是向设备请求的声道数（1 或 2），零值 2。设备回退到混音格式
	// 时以实际格式为准；Write 会自动在单声道与立体声之间转换。
	Channels int
}

// 渲染循环的等待参数。共享模式的事件大约每 10 ms 触发一次；连续 5 秒没有
// 收到事件说明端点已经卡死，此时返回错误而不是永久阻塞。
const (
	renderWaitSlice  = 500 * time.Millisecond
	renderStallLimit = 5 * time.Second
	renderDrainPoll  = 10 * time.Millisecond
)

// WASAPIRenderer 用 WASAPI 共享模式播放到默认渲染端点，实现 Renderer。
//
// 调用方必须在当前线程初始化 COM（COINIT_MULTITHREADED）并在同一线程调用
// 构造、Write、Drain 和 Release——与 WASAPICapturer 的约定相同。Release 不会
// 调用 CoUninitialize。
//
// 不是并发安全的。
type WASAPIRenderer struct {
	cfg         WASAPIRendererConfig
	format      Format // 设备实际接受的格式
	srcChannels int    // Write 输入的声道数

	enumerator *iMMDeviceEnumerator
	device     *iMMDevice
	client     *iAudioClient
	render     *iAudioRenderClient
	event      windows.Handle

	started  bool
	released bool
}

var _ Renderer = (*WASAPIRenderer)(nil)

// NewWASAPIRenderer 打开默认渲染端点并启动播放流。启动后设备会先播放静音，
// 直到第一次 Write。
//
// 与采集侧一样优先请求 48 kHz / F32 / cfg.Channels 并用 AUTOCONVERTPCM 让
// Windows 完成重采样与声道搬运；被拒绝时回退到设备混音格式，仅接受 48 kHz
// 且 1/2 声道的 16-bit PCM 或 32-bit float。
func NewWASAPIRenderer(cfg WASAPIRendererConfig) (*WASAPIRenderer, error) {
	if cfg.Channels == 0 {
		cfg.Channels = 2
	}
	if cfg.Channels != 1 && cfg.Channels != 2 {
		return nil, fmt.Errorf("wasapi: channels %d, want 1 or 2", cfg.Channels)
	}

	r := &WASAPIRenderer{cfg: cfg, srcChannels: cfg.Channels}
	if err := r.open(); err != nil {
		r.Release()
		return nil, err
	}
	return r, nil
}

func (r *WASAPIRenderer) open() error {
	enumerator, hr := coCreateEnumerator()
	if failedHR(hr) {
		return wasapiError("CoCreateInstance(IMMDeviceEnumerator)", hr)
	}
	r.enumerator = enumerator

	device, hr := enumerator.GetDefaultAudioEndpoint(eRender, eConsole)
	if failedHR(hr) {
		return wasapiError("GetDefaultAudioEndpoint(eRender, eConsole)", hr)
	}
	r.device = device

	client, err := r.activateClient()
	if err != nil {
		return err
	}
	r.client = client

	target := buildTargetFormat(r.cfg.Channels)
	flags := uint32(streamFlagEventCallback | streamFlagAutoConvertPCM | streamFlagSRCDefault)
	hr = client.Initialize(audioShareModeShared, flags, 0, 0, unsafe.Pointer(&target), nil)
	if failedHR(hr) {
		targetHR := hr

		// Initialize 失败后同一个 IAudioClient 可能永久返回
		// E_ALREADY_INITIALIZED，必须重新 Activate。
		client.Release()
		r.client = nil
		client, err = r.activateClient()
		if err != nil {
			return err
		}
		r.client = client

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
			return fmt.Errorf("wasapi: 48 kHz render target rejected (HRESULT 0x%08X), mix format unusable: %w",
				uint32(targetHR), parseErr)
		}

		mixHR = client.Initialize(audioShareModeShared,
			uint32(streamFlagEventCallback), 0, 0, mix, nil)
		coTaskMemFree(mix) // Initialize 返回后格式描述即可释放
		if failedHR(mixHR) {
			return fmt.Errorf("wasapi: Initialize with the mix format failed (HRESULT 0x%08X); "+
				"the 48 kHz target had failed with 0x%08X", uint32(mixHR), uint32(targetHR))
		}
		r.format = format
	} else {
		r.format = Format{SampleRate: OpusSampleRate, Channels: r.cfg.Channels, Float32: true}
	}

	render, hr := client.GetService(&iidAudioRenderClient)
	if failedHR(hr) {
		return wasapiError("GetService(IAudioRenderClient)", hr)
	}
	r.render = (*iAudioRenderClient)(render)

	event, err := windows.CreateEvent(nil, 0, 0, nil) // 自动重置、初始未触发
	if err != nil {
		return fmt.Errorf("wasapi: CreateEvent: %w", err)
	}
	r.event = event

	if hr := client.SetEventHandle(event); failedHR(hr) {
		return wasapiError("SetEventHandle", hr)
	}
	if hr := client.Start(); failedHR(hr) {
		return wasapiError("Start", hr)
	}
	r.started = true
	return nil
}

func (r *WASAPIRenderer) activateClient() (*iAudioClient, error) {
	var p unsafe.Pointer
	hr := r.device.Activate(&iidAudioClient, clsctxAll, nil, &p)
	if failedHR(hr) {
		return nil, wasapiError("IMMDevice.Activate(IAudioClient)", hr)
	}
	return (*iAudioClient)(p), nil
}

// Format 返回设备实际接受的 PCM 格式。
func (r *WASAPIRenderer) Format() Format { return r.format }

// Write 把交错 float32 PCM 写进设备缓冲。设备缓冲满时阻塞在 WASAPI 事件上，
// 因此调用方天然获得实时节拍，不需要自己 sleep。
func (r *WASAPIRenderer) Write(pcm []float32) error {
	if r.released {
		return errors.New("wasapi: renderer already released")
	}
	if r.render == nil {
		return errors.New("wasapi: renderer is not initialized")
	}
	if len(pcm) == 0 {
		return nil
	}
	if len(pcm)%r.srcChannels != 0 {
		return fmt.Errorf("wasapi: %d float samples is not a multiple of %d channels",
			len(pcm), r.srcChannels)
	}

	bufferSize, hr := r.client.GetBufferSize()
	if failedHR(hr) {
		return wasapiError("GetBufferSize", hr)
	}
	frames := len(pcm) / r.srcChannels
	stalled := time.Now()

	for frames > 0 {
		padding, hr := r.client.GetCurrentPadding()
		if failedHR(hr) {
			return wasapiError("GetCurrentPadding", hr)
		}
		avail := int(bufferSize) - int(padding)
		if avail <= 0 {
			woken, err := windows.WaitForSingleObject(r.event, uint32(renderWaitSlice/time.Millisecond))
			if err != nil {
				return fmt.Errorf("wasapi: WaitForSingleObject: %w", err)
			}
			switch woken {
			case windows.WAIT_OBJECT_0:
				stalled = time.Now()
			case waitTimeout:
				if time.Since(stalled) >= renderStallLimit {
					return fmt.Errorf("wasapi: render stalled for %s", renderStallLimit)
				}
			default:
				return fmt.Errorf("wasapi: WaitForSingleObject returned %#x", woken)
			}
			continue
		}

		n := min(frames, avail)
		data, hr := r.render.GetBuffer(uint32(n))
		if failedHR(hr) {
			return wasapiError("GetBuffer", hr)
		}
		if data == nil {
			return errors.New("wasapi: GetBuffer returned no data")
		}
		dst := unsafe.Slice((*byte)(data), n*r.format.BlockAlign())
		writeFloat32PCM(dst, pcm[:n*r.srcChannels], r.format, r.srcChannels)
		if hr := r.render.ReleaseBuffer(uint32(n), 0); failedHR(hr) {
			return wasapiError("ReleaseBuffer", hr)
		}

		pcm = pcm[n*r.srcChannels:]
		frames -= n
		stalled = time.Now()
	}
	return nil
}

// Drain 等待设备把已写入的数据播完。共享模式没有"播完通知"，只能轮询
// GetCurrentPadding；10 ms 一次足以覆盖引擎周期。
func (r *WASAPIRenderer) Drain(timeout time.Duration) error {
	if r.released {
		return errors.New("wasapi: renderer already released")
	}
	if r.client == nil {
		return errors.New("wasapi: renderer is not initialized")
	}
	deadline := time.Now().Add(timeout)
	for {
		padding, hr := r.client.GetCurrentPadding()
		if failedHR(hr) {
			return wasapiError("GetCurrentPadding", hr)
		}
		if padding == 0 {
			return nil
		}
		if timeout <= 0 || time.Now().After(deadline) {
			return ErrTimeout
		}
		time.Sleep(renderDrainPoll)
	}
}

// Release 停止播放并释放所有 COM 接口和事件句柄。可重复调用；不调用
// CoUninitialize——apartment 的初始化/清理属于调用方。
func (r *WASAPIRenderer) Release() {
	if r == nil || r.released {
		return
	}
	r.released = true

	if r.started && r.client != nil {
		_ = r.client.Stop()
		_ = r.client.Reset() // 丢弃还在设备缓冲里的数据
	}
	r.started = false

	if r.render != nil {
		_ = r.render.Release()
		r.render = nil
	}
	if r.client != nil {
		_ = r.client.Release()
		r.client = nil
	}
	if r.device != nil {
		_ = r.device.Release()
		r.device = nil
	}
	if r.enumerator != nil {
		_ = r.enumerator.Release()
		r.enumerator = nil
	}
	if r.event != 0 {
		_ = windows.CloseHandle(r.event)
		r.event = 0
	}
}

// writeFloat32PCM 把交错 float32 转换成设备格式的字节。srcChannels 与
// format.Channels 不同时在 1/2 声道之间搬运：单声道复制成双声道，双声道取
// 平均混成单声道。
//
// 与 f32ToS16 一样，越界值截断而不是回绕，NaN 按静音处理。
func writeFloat32PCM(dst []byte, src []float32, format Format, srcChannels int) {
	if format.Channels == srcChannels {
		if format.Float32 {
			for i, v := range src {
				binary.LittleEndian.PutUint32(dst[i*4:], math.Float32bits(v))
			}
			return
		}
		for i, v := range src {
			binary.LittleEndian.PutUint16(dst[i*2:], uint16(f32ToInt16(v)))
		}
		return
	}

	frames := len(src) / srcChannels
	if format.Float32 {
		for i := range frames {
			v := remapSample(src[i*srcChannels:], srcChannels, format.Channels)
			for c := range format.Channels {
				binary.LittleEndian.PutUint32(dst[(i*format.Channels+c)*4:], math.Float32bits(v))
			}
		}
		return
	}
	for i := range frames {
		v := remapSample(src[i*srcChannels:], srcChannels, format.Channels)
		s := f32ToInt16(v)
		for c := range format.Channels {
			binary.LittleEndian.PutUint16(dst[(i*format.Channels+c)*2:], uint16(s))
		}
	}
}

// remapSample 返回一帧在目标声道数下的采样值。只支持 1↔2。
func remapSample(frame []float32, srcChannels, dstChannels int) float32 {
	if srcChannels == dstChannels {
		return frame[0]
	}
	if srcChannels == 1 { // 单声道复制到左右
		return frame[0]
	}
	if dstChannels == 1 { // 双声道取平均
		return (frame[0] + frame[1]) / 2
	}
	return frame[0]
}

func f32ToInt16(v float32) int16 {
	if math.IsNaN(float64(v)) {
		return 0
	}
	s := v * 32767.0
	switch {
	case s >= 32767.0: // 同时覆盖 +Inf
		return 32767
	case s <= -32768.0: // 同时覆盖 -Inf
		return -32768
	default:
		return int16(s)
	}
}
