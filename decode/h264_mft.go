// Package decode 提供 H264 解码能力，供控制端（客户端）把收到的码流还原成像素。
//
// 它和 encode 包平级：encode 是"纹理 -> H264 字节"，decode 是"H264 字节 -> 像素"。
// 底层都是 Media Foundation 的 MFT，一个走编码器类别，一个走解码器类别。
package decode

import (
	"fmt"
	"time"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"golang.org/x/sys/windows"

	"mfcapture/gpu"
	"mfcapture/mf"
)

// DebugLog 若非 nil，会打印候选解码器的协商细节（尤其是为什么某个候选被淘汰），
// 用于排查"本机明明有硬件解码器，为什么选中的是软件解码器"。
var DebugLog func(string)

func debugf(format string, args ...any) {
	if DebugLog != nil {
		DebugLog(fmt.Sprintf(format, args...))
	}
}

// H264DecoderConfig 配置一个 H264 解码器。
type H264DecoderConfig struct {
	// Width/Height 是码流分辨率。传 0 表示让解码器从 SPS 自己推断
	// （多数 MFT 支持，少数要求在输入类型上就给出，本包会自动两种都试）。
	Width, Height uint32
	FPSNum        uint32
	FPSDen        uint32
	// Output 指定期望的输出像素格式，零值表示自动挑选（默认 NV12，因为硬件解码器几乎都输出它）。
	Output windows.GUID
	// DisableHardware 置 true 时不做 DXVA，纯软件解码。
	//
	// 默认 false，即**默认开启硬件加速**。这里要澄清一个容易误解的点：
	// Windows 的 H264 解码器 MFT 是同步 MFT，用 MFT_ENUM_FLAG_HARDWARE 枚举
	// 通常一个都枚举不到（实测本机就是 0 个）。它是否走硬件，取决于你有没有
	// 通过 MFT_MESSAGE_SET_D3D_MANAGER 把 D3D11 设备交给它——不交就内部退化成软件解码。
	// 所以"开启硬件加速"的正确做法不是换 MFT，而是给它设备管理器。
	DisableHardware bool
}

// DecodedFrame 是一帧解码后的像素。
type DecodedFrame struct {
	Data      []byte       // 像素数据，布局由 Subtype 决定（NV12 时是 Y 平面 + 交错的 UV 平面）
	Subtype   windows.GUID // 例如 mf.MFVideoFormat_NV12
	Width     uint32
	Height    uint32
	Stride    int32 // 每行字节数；为 0 表示等于 Width（紧凑存放）
	Timestamp time.Duration
	Duration  time.Duration
}

// H264MFTDecoder 包装一个 Media Foundation H264 解码器 MFT。
// 支持同步与异步两种 MFT；默认把 D3D11 设备管理器交给它以启用 DXVA 硬件解码。
type H264MFTDecoder struct {
	cfg             H264DecoderConfig // 分辨率变化后重新协商输出类型时要用到
	transform       *mf.IMFTransform
	eventGen        *mf.IMFMediaEventGenerator
	devMgr          *mf.IMFDXGIDeviceManager
	isAsync         bool
	dxva            bool
	fallbackDone    bool // DXVA 失败回退只尝试一次，避免每帧都重建
	providesSamples bool
	outputBufSize   uint32
	outSubtype      windows.GUID
	outWidth        uint32
	outHeight       uint32
	outStride       int32

	// IsHardware 表示是否真的启用了硬件加速（DXVA）。
	// 它不等于"选中的是硬件 MFT"——见 H264DecoderConfig.DisableHardware 的说明。
	IsHardware bool
}

// NewH264MFTDecoder 枚举并激活一个 H264 解码器 MFT，完成输入输出类型协商。
// 逐个候选尝试，全部失败才报错。
func NewH264MFTDecoder(cfg H264DecoderConfig) (*H264MFTDecoder, error) {
	if cfg.FPSNum == 0 {
		cfg.FPSNum = 30
	}
	if cfg.FPSDen == 0 {
		cfg.FPSDen = 1
	}
	if cfg.Output == (windows.GUID{}) {
		cfg.Output = mf.MFVideoFormat_NV12
	}

	candidates, err := enumDecoders()
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no H264 decoder MFT found")
	}

	var lastErr error
	for _, c := range candidates {
		kind := "软件"
		if c.hardware {
			kind = "硬件"
		}
		debugf("尝试%s解码器…", kind)
		d := &H264MFTDecoder{cfg: cfg, transform: c.transform, IsHardware: c.hardware}
		if err := d.configure(cfg); err != nil {
			debugf("  %s解码器不可用: %v", kind, err)
			lastErr = err
			d.transform.Release()
			d.transform = nil
			continue
		}
		d.IsHardware = d.dxva
		debugf("  %s解码器协商成功 (DXVA=%v)", kind, d.dxva)
		return d, nil
	}
	return nil, fmt.Errorf("no usable H264 decoder MFT: %w", lastErr)
}

type decoderCandidate struct {
	transform *mf.IMFTransform
	hardware  bool
}

// enumDecoders 先枚举硬件解码器，再枚举同步（通常是软件）解码器。
func enumDecoders() ([]decoderCandidate, error) {
	input := mf.MFT_REGISTER_TYPE_INFO{
		GuidMajorType: mf.MFMediaType_Video,
		GuidSubtype:   mf.MFVideoFormat_H264,
	}

	var out []decoderCandidate
	for _, g := range []struct {
		name  string
		flags uint32
		hw    bool
	}{
		{"硬件(HARDWARE)", mf.MFT_ENUM_FLAG_HARDWARE | mf.MFT_ENUM_FLAG_SORTANDFILTER, true},
		{"同步(SYNCMFT|LOCALMFT)", mf.MFT_ENUM_FLAG_SYNCMFT | mf.MFT_ENUM_FLAG_LOCALMFT | mf.MFT_ENUM_FLAG_SORTANDFILTER, false},
	} {
		acts, err := mf.MFTEnumEx(mf.MFT_CATEGORY_VIDEO_DECODER, g.flags, &input, nil)
		if err != nil {
			debugf("枚举[%s] 失败: %v", g.name, err)
			continue
		}
		debugf("枚举[%s] 找到 %d 个", g.name, len(acts))
		for _, act := range acts {
			var t *mf.IMFTransform
			hr := act.ActivateObject(mf.IID_IMFTransform, unsafe.Pointer(&t))
			act.Release()
			if d3d.HRESULT(hr).Failed() {
				debugf("  %s 激活失败: %v", g.name, d3d.HRESULT(hr))
				continue
			}
			out = append(out, decoderCandidate{transform: t, hardware: g.hw})
		}
	}
	return out, nil
}

func (d *H264MFTDecoder) configure(cfg H264DecoderConfig) error {
	// 1. 解锁异步 MFT（异步硬件解码器必须先解锁，否则后续 SetXxxType 一律失败）。
	var attrs *mf.IMFAttributes
	if hr := d.transform.GetAttributes(&attrs); !d3d.HRESULT(hr).Failed() && attrs != nil {
		var aware uint32
		if !d3d.HRESULT(attrs.GetUINT32(mf.MF_SA_D3D11_AWARE, &aware)).Failed() {
			debugf("  D3D11-aware=%d", aware)
		} else {
			debugf("  D3D11-aware 读不到（通常意味着这是纯软件 MFT）")
		}
		attrs.SetUINT32(mf.MF_TRANSFORM_ASYNC_UNLOCK, 1)
		attrs.Release()
	}

	// 2. 判断是否异步 MFT。
	var eventGen *mf.IMFMediaEventGenerator
	if hr := d.transform.QueryInterface(mf.IID_IMFMediaEventGenerator, &eventGen); !d3d.HRESULT(hr).Failed() && eventGen != nil {
		d.eventGen = eventGen
		d.isAsync = true
	}
	debugf("  async=%v", d.isAsync)

	// 3. 输入类型：优先带分辨率；失败则退化为不带分辨率（让解码器从 SPS 推断）。
	if err := d.setInputType(cfg, true); err != nil {
		if err2 := d.setInputType(cfg, false); err2 != nil {
			return fmt.Errorf("SetInputType: %w (带分辨率) / %v (不带分辨率)", err, err2)
		}
	}

	// 4. 硬件加速：把 D3D11 设备管理器交给 MFT（DXVA）。
	//    必须在输入类型之后、输出类型之前——顺序错了 MFT 会拒绝。
	if !cfg.DisableHardware {
		d.enableDXVA()
	}

	// 5. 输出类型：从解码器可选类型里挑一个（优先 cfg.Output）。
	if err := d.setOutputType(cfg); err != nil {
		if !d.dxva {
			return err
		}
		// 带上 DXVA 协商失败就撤掉设备管理器再试一次，宁可软件解码也别直接失败。
		debugf("  带 DXVA 的输出类型协商失败(%v)，回退到无 DXVA", err)
		d.disableDXVA()
		if err2 := d.setOutputType(cfg); err2 != nil {
			return err2
		}
	}

	// 5. 查询输出流信息，决定缓冲区由谁分配。
	var streamInfo mf.MFT_OUTPUT_STREAM_INFO
	d.transform.GetOutputStreamInfo(0, &streamInfo)
	d.providesSamples = (streamInfo.DwFlags & mf.MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) != 0
	d.outputBufSize = streamInfo.CbSize
	if d.outputBufSize == 0 {
		d.outputBufSize = 8 * 1024 * 1024
	}

	d.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0)
	d.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0)
	return nil
}

// enableDXVA 创建 D3D11 设备与设备管理器并交给解码器 MFT，启用 DXVA 硬件解码。
// 任何一步失败都记录原因并保持未启用状态，调用方无需特殊处理。
func (d *H264MFTDecoder) enableDXVA() {
	device, deviceCtx, err := gpu.NewVideoCapableDevice()
	if err != nil {
		debugf("  DXVA 不可用（建 D3D11 设备失败）: %v", err)
		return
	}
	defer device.Release()
	defer deviceCtx.Release()

	if err := gpu.EnableMultithreadProtection(device); err != nil {
		debugf("  DXVA 不可用（多线程保护失败）: %v", err)
		return
	}
	mgr, _, err := gpu.NewDXGIDeviceManager(device)
	if err != nil {
		debugf("  DXVA 不可用（建设备管理器失败）: %v", err)
		return
	}
	hr := d.transform.ProcessMessage(mf.MFT_MESSAGE_SET_D3D_MANAGER, uintptr(unsafe.Pointer(mgr)))
	if d3d.HRESULT(hr).Failed() {
		debugf("  DXVA 不可用（MFT 拒绝设备管理器）: %v", d3d.HRESULT(hr))
		mgr.Release()
		return
	}
	// 设备管理器自己持有设备的引用，这里可以放心释放我们的那份。
	d.devMgr = mgr
	d.dxva = true
	debugf("  DXVA 已启用（D3D11 设备管理器已交给解码器）")
}

func (d *H264MFTDecoder) disableDXVA() {
	if d.devMgr != nil {
		d.devMgr.Release()
		d.devMgr = nil
	}
	d.dxva = false
}

func (d *H264MFTDecoder) setInputType(cfg H264DecoderConfig, withSize bool) error {
	w, h := cfg.Width, cfg.Height
	if !withSize {
		w, h = 0, 0
	}
	mt, err := mf.NewH264DecodeInputType(w, h, cfg.FPSNum, cfg.FPSDen)
	if err != nil {
		return err
	}
	defer mt.Release()
	if hr := d.transform.SetInputType(0, mt, 0); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("%w", d3d.HRESULT(hr))
	}
	return nil
}

func (d *H264MFTDecoder) setOutputType(cfg H264DecoderConfig) error {
	var chosen *mf.IMFMediaType
	var fallback *mf.IMFMediaType
	var seen []string
	for i := uint32(0); i < 64; i++ {
		var mt *mf.IMFMediaType
		if hr := d.transform.GetOutputAvailableType(0, i, &mt); d3d.HRESULT(hr).Failed() {
			break
		}
		var sub windows.GUID
		mt.GetGUID(mf.MF_MT_SUBTYPE, &sub)
		var fs uint64
		hasSize := !d3d.HRESULT(mt.GetUINT64(mf.MF_MT_FRAME_SIZE, &fs)).Failed()
		if hasSize {
			seen = append(seen, fmt.Sprintf("%s %dx%d", mf.SubtypeName(sub), uint32(fs>>32), uint32(fs&0xFFFFFFFF)))
		} else {
			seen = append(seen, mf.SubtypeName(sub))
		}
		if sub == cfg.Output {
			chosen = mt
			break
		}
		if fallback == nil {
			fallback = mt
		} else {
			mt.Release()
		}
	}
	debugf("  可选输出类型: %v", seen)
	if chosen == nil {
		chosen = fallback
	}
	if chosen == nil {
		return fmt.Errorf("decoder exposes no output media type")
	}
	defer chosen.Release()

	// 记下实际的输出格式与尺寸，供调用方解释 Data。
	chosen.GetGUID(mf.MF_MT_SUBTYPE, &d.outSubtype)
	var fs uint64
	if !d3d.HRESULT(chosen.GetUINT64(mf.MF_MT_FRAME_SIZE, &fs)).Failed() {
		d.outWidth, d.outHeight = uint32(fs>>32), uint32(fs&0xFFFFFFFF)
	}
	if d.outWidth == 0 {
		d.outWidth, d.outHeight = cfg.Width, cfg.Height
	}
	var stride uint32
	if !d3d.HRESULT(chosen.GetUINT32(mf.MF_MT_DEFAULT_STRIDE, &stride)).Failed() {
		d.outStride = int32(stride)
	}

	if hr := d.transform.SetOutputType(0, chosen, 0); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("SetOutputType(%s): %w", mf.SubtypeName(d.outSubtype), d3d.HRESULT(hr))
	}
	return nil
}

// OutputFormat 返回协商后的输出像素格式与尺寸。
func (d *H264MFTDecoder) OutputFormat() (subtype windows.GUID, width, height uint32, stride int32) {
	return d.outSubtype, d.outWidth, d.outHeight, d.outStride
}

// Decode 送入一个完整的访问单元（Annex-B 字节，含起始码），返回解码出的帧。
// 一次输入可能产出 0 帧（解码器还在攒参考帧）或多帧，所以返回的是切片。
func (d *H264MFTDecoder) Decode(au []byte, ts, dur time.Duration) ([]*DecodedFrame, error) {
	frames, err := d.decodeOnce(au, ts, dur)
	if err != nil && d.dxva && !d.fallbackDone {
		// DXVA 出错时这个 MFT 基本就废了（后续 ProcessInput 一律 MF_E_NOTACCEPTING，
		// flush 也救不回来），只能整个重建一个不带 DXVA 的解码器。
		d.fallbackDone = true
		debugf("DXVA 解码出错(%v)，重建为软件解码器并重试", err)
		if rerr := d.rebuildWithoutDXVA(); rerr == nil {
			return d.decodeOnce(au, ts, dur)
		}
	}
	return frames, err
}

// rebuildWithoutDXVA 释放当前 MFT，重新枚举并创建一个不做 DXVA 的解码器。
// 只在 DXVA 路径确认不可用、且仅尝试一次时调用。
func (d *H264MFTDecoder) rebuildWithoutDXVA() error {
	d.Release()

	cfg := d.cfg
	cfg.DisableHardware = true
	cands, err := enumDecoders()
	if err != nil {
		return err
	}
	for _, c := range cands {
		d.cfg = cfg
		d.transform = c.transform
		d.eventGen = nil
		d.devMgr = nil
		d.isAsync = false
		d.dxva = false
		if err := d.configure(cfg); err != nil {
			d.transform.Release()
			d.transform = nil
			continue
		}
		d.IsHardware = false
		debugf("  已重建为软件解码器")
		return nil
	}
	return fmt.Errorf("无法重建解码器")
}

func (d *H264MFTDecoder) decodeOnce(au []byte, ts, dur time.Duration) ([]*DecodedFrame, error) {
	sample, err := d.makeInputSample(au)
	if err != nil {
		return nil, err
	}
	sample.SetSampleTime(int64(ts / 100))
	sample.SetSampleDuration(int64(dur / 100))

	var frames []*DecodedFrame
	if d.isAsync {
		if err := d.waitForEventType(mf.METransformNeedInput); err != nil {
			sample.Release()
			return nil, fmt.Errorf("waiting for NeedInput: %w", err)
		}
		if hr := d.transform.ProcessInput(0, sample, 0); d3d.HRESULT(hr).Failed() {
			sample.Release()
			return nil, fmt.Errorf("ProcessInput: %w", d3d.HRESULT(hr))
		}
		sample.Release()
		frames, err = d.drainOutputs()
		if err != nil {
			return frames, err
		}
		return frames, nil
	}

	if hr := d.transform.ProcessInput(0, sample, 0); d3d.HRESULT(hr).Failed() {
		sample.Release()
		return nil, fmt.Errorf("ProcessInput: %w", d3d.HRESULT(hr))
	}
	sample.Release()
	return d.drainOutputs()
}

func (d *H264MFTDecoder) makeInputSample(au []byte) (*mf.IMFSample, error) {
	var buf *mf.IMFMediaBuffer
	if hr := mf.MFCreateMemoryBuffer(uint32(len(au)), &buf); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("MFCreateMemoryBuffer: %w", d3d.HRESULT(hr))
	}
	var p unsafe.Pointer
	var maxLen, curLen uint32
	if hr := buf.Lock(&p, &maxLen, &curLen); d3d.HRESULT(hr).Failed() {
		buf.Release()
		return nil, fmt.Errorf("IMFMediaBuffer.Lock: %w", d3d.HRESULT(hr))
	}
	if maxLen < uint32(len(au)) {
		buf.Unlock()
		buf.Release()
		return nil, fmt.Errorf("buffer too small: %d < %d", maxLen, len(au))
	}
	copy(unsafe.Slice((*byte)(p), len(au)), au)
	buf.Unlock()
	if hr := buf.SetCurrentLength(uint32(len(au))); d3d.HRESULT(hr).Failed() {
		buf.Release()
		return nil, fmt.Errorf("SetCurrentLength: %w", d3d.HRESULT(hr))
	}

	var sample *mf.IMFSample
	if hr := mf.MFCreateSample(&sample); d3d.HRESULT(hr).Failed() {
		buf.Release()
		return nil, fmt.Errorf("MFCreateSample: %w", d3d.HRESULT(hr))
	}
	if hr := sample.AddBuffer(buf); d3d.HRESULT(hr).Failed() {
		sample.Release()
		buf.Release()
		return nil, fmt.Errorf("AddBuffer: %w", d3d.HRESULT(hr))
	}
	buf.Release()
	return sample, nil
}

// drainOutputs 反复 ProcessOutput，直到本次没有更多输出为止。
func (d *H264MFTDecoder) drainOutputs() ([]*DecodedFrame, error) {
	var frames []*DecodedFrame
	for {
		f, more, err := d.processOutput()
		if err != nil {
			return frames, err
		}
		if f == nil {
			return frames, nil
		}
		frames = append(frames, f)
		if !more {
			// 保险起见再试一次，但最多再取一帧，避免死循环。
			if f2, _, err2 := d.processOutput(); err2 == nil && f2 != nil {
				frames = append(frames, f2)
			}
			return frames, nil
		}
	}
}

func (d *H264MFTDecoder) processOutput() (frame *DecodedFrame, more bool, err error) {
	if d.isAsync {
		et, err := d.getNextEventType()
		if err != nil {
			return nil, false, err
		}
		if et != mf.METransformHaveOutput {
			return nil, false, nil
		}
	}

	outData := mf.MFT_OUTPUT_DATA_BUFFER{}
	var allocated *mf.IMFSample
	if !d.providesSamples {
		if hr := mf.MFCreateSample(&allocated); d3d.HRESULT(hr).Failed() {
			return nil, false, fmt.Errorf("MFCreateSample: %w", d3d.HRESULT(hr))
		}
		var outBuf *mf.IMFMediaBuffer
		if hr := mf.MFCreateMemoryBuffer(d.outputBufSize, &outBuf); d3d.HRESULT(hr).Failed() {
			allocated.Release()
			return nil, false, fmt.Errorf("MFCreateMemoryBuffer: %w", d3d.HRESULT(hr))
		}
		allocated.AddBuffer(outBuf)
		outBuf.Release()
		outData.PSample = allocated
	}

	var status uint32
	hr := d.transform.ProcessOutput(0, 1, &outData, &status)
	if allocated != nil {
		// ProcessOutput 成功时 PSample 由 MFT 填好；失败或无输出时我们要自己回收。
		if d3d.HRESULT(hr).Failed() || outData.PSample == nil {
			allocated.Release()
		}
	}
	if uint32(hr) == mf.MF_E_TRANSFORM_NEED_MORE_INPUT {
		return nil, false, nil
	}
	if uint32(hr) == mf.MF_E_TRANSFORM_STREAM_CHANGE {
		// 码流分辨率变了（被控端改分辨率 / 换显示器）：必须重新协商输出类型再取帧。
		if err := d.setOutputType(d.cfg); err != nil {
			return nil, false, fmt.Errorf("renegotiate output after stream change: %w", err)
		}
		return nil, false, nil
	}
	if d3d.HRESULT(hr).Failed() {
		return nil, false, fmt.Errorf("ProcessOutput: %w", d3d.HRESULT(hr))
	}
	if outData.PSample == nil {
		return nil, false, nil
	}
	defer outData.PSample.Release()

	f, err := d.extractFrame(outData.PSample)
	if err != nil {
		return nil, false, err
	}
	return f, (outData.DwStatus & mf.MFT_OUTPUT_DATA_BUFFER_INCOMPLETE) != 0, nil
}

func (d *H264MFTDecoder) extractFrame(sample *mf.IMFSample) (*DecodedFrame, error) {
	var buf *mf.IMFMediaBuffer
	if hr := sample.ConvertToContiguousBuffer(&buf); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("ConvertToContiguousBuffer: %w", d3d.HRESULT(hr))
	}
	defer buf.Release()

	var p unsafe.Pointer
	var maxLen, curLen uint32
	if hr := buf.Lock(&p, &maxLen, &curLen); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("Lock: %w", d3d.HRESULT(hr))
	}
	data := make([]byte, curLen)
	copy(data, unsafe.Slice((*byte)(p), curLen))
	buf.Unlock()

	var ts, dur int64
	sample.GetSampleTime(&ts)
	sample.GetSampleDuration(&dur)

	return &DecodedFrame{
		Data:      data,
		Subtype:   d.outSubtype,
		Width:     d.outWidth,
		Height:    d.outHeight,
		Stride:    d.outStride,
		Timestamp: time.Duration(ts * 100),
		Duration:  time.Duration(dur * 100),
	}, nil
}

// Flush 通知解码器排空缓冲并取回剩余帧。
func (d *H264MFTDecoder) Flush() ([]*DecodedFrame, error) {
	d.transform.ProcessMessage(mf.MFT_MESSAGE_COMMAND_DRAIN, 0)
	if d.isAsync {
		var frames []*DecodedFrame
		for {
			et, err := d.getNextEventType()
			if err != nil {
				return frames, nil
			}
			if et == mf.METransformDrainComplete {
				return frames, nil
			}
			if et == mf.METransformHaveOutput {
				f, _, err := d.processOutput()
				if err != nil {
					return frames, err
				}
				if f != nil {
					frames = append(frames, f)
				}
			}
		}
	}
	var frames []*DecodedFrame
	for {
		f, _, err := d.processOutput()
		if err != nil || f == nil {
			return frames, nil
		}
		frames = append(frames, f)
	}
}

func (d *H264MFTDecoder) waitForEventType(target uint32) error {
	for {
		var ev *mf.IMFMediaEvent
		if hr := d.eventGen.GetEvent(0, &ev); d3d.HRESULT(hr).Failed() {
			return fmt.Errorf("GetEvent: %w", d3d.HRESULT(hr))
		}
		var et uint32
		ev.GetType(&et)
		ev.Release()
		if et == target {
			return nil
		}
	}
}

func (d *H264MFTDecoder) getNextEventType() (uint32, error) {
	var ev *mf.IMFMediaEvent
	if hr := d.eventGen.GetEvent(0, &ev); d3d.HRESULT(hr).Failed() {
		return 0, fmt.Errorf("GetEvent: %w", d3d.HRESULT(hr))
	}
	var et uint32
	ev.GetType(&et)
	ev.Release()
	return et, nil
}

// Release 释放所有 COM 资源。
func (d *H264MFTDecoder) Release() {
	if d.eventGen != nil {
		d.eventGen.Release()
		d.eventGen = nil
	}
	if d.devMgr != nil {
		d.devMgr.Release()
		d.devMgr = nil
	}
	if d.transform != nil {
		d.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0)
		d.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_END_STREAMING, 0)
		d.transform.Release()
		d.transform = nil
	}
}
