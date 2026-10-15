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
	"github.com/kirides/go-d3d/d3d11"
	"golang.org/x/sys/windows"

	"mfcapture/gpu"
	"mfcapture/mf"
)

// DebugLog 若非 nil，会打印候选解码器的协商细节（尤其是为什么某个候选被淘汰），
// 用于排查"本机明明有硬件解码器，为什么选中的是软件解码器"。
var DebugLog func(string)

// FallbackLog 若非 nil，DXVA 中途出错、解码器被重建为纯软件解码时会打印原因。
//
// 这个回退是静默发生的（Decode 会自动重建后重试，调用方看不到错误），
// 所以"硬解用着用着就变软解了"这类问题必须靠它才能看见。
// 不开 DebugLog 也会打——它就是为这种场景准备的。
var FallbackLog func(string)

func warnf(format string, args ...any) {
	if FallbackLog != nil {
		FallbackLog(fmt.Sprintf(format, args...))
	}
	debugf(format, args...)
}

// maxDrainPerCall 是一次排空最多取回几帧。见 drainOutputs 的说明。
const maxDrainPerCall = 3

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
	// DisableLowLatency 置 true 时不开启低延迟模式（MF_LOW_LATENCY）。
	//
	// 默认 false，即**默认低延迟**：解码器每输入一帧就输出一帧。不开的话，微软的 H264
	// 解码器 MFT 会先攒满整个 DPB（实测约 16 帧）才开始输出——60fps 下凭空多出约 270ms
	// 延迟；而且 DXVA 的 surface 池只按 DPB 大小分配，调用方一旦持有正在显示的那一帧，
	// 解码器就等不到空闲 surface，ProcessOutput 永久阻塞（实测文件播放与网络流都能复现）。
	//
	// 低延迟模式假定码流没有 B 帧（输出顺序即解码顺序）。实时编码器（含本包的编码器）
	// 的输出满足这一点；播放带 B 帧的普通视频文件时才需要关掉它。
	DisableLowLatency bool
	// Device/DeviceCtx 可选传入一个已有的 D3D11 设备。若非 nil，解码器复用该设备
	// （不释放它），使解码器输出的纹理与调用方的渲染设备属于同一个 D3D11 Device，
	// 实现零拷贝渲染。若为 nil，解码器自行创建并管理设备（向后兼容）。
	Device    *d3d11.ID3D11Device
	DeviceCtx *d3d11.ID3D11DeviceContext
}

var _ Decoder = (*H264MFTDecoder)(nil)

// H264MFTDecoder 包装一个 Media Foundation H264 解码器 MFT。
// 支持同步与异步两种 MFT；默认把 D3D11 设备管理器交给它以启用 DXVA 硬件解码。
type H264MFTDecoder struct {
	cfg             H264DecoderConfig
	transform       *mf.IMFTransform
	eventGen        *mf.IMFMediaEventGenerator
	devMgr          *mf.IMFDXGIDeviceManager
	device          *d3d11.ID3D11Device
	deviceCtx       *d3d11.ID3D11DeviceContext
	ownsDevice      bool
	isAsync         bool
	dxva            bool
	fallbackDone    bool
	providesSamples bool
	outputBufSize   uint32
	draining        bool // 是否已发过 DRAIN（Flush 分批取，不能每次都重发）
	outSubtype      windows.GUID
	outWidth        uint32
	outHeight       uint32
	outStride       int32

	// stallHook 由 Session 设置：同步 MFT 背压重试期间调用（默认 nil，
	// 那时只是纯 sleep）。给上层一个"趁等待把已渲染的帧还回来"的机会。
	stallHook func()
	// degradeHook 由 Session 设置：DXVA 回退到软件解码时调用，错误原因是入参。
	degradeHook func(error)

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
		// 低延迟必须在设置媒体类型之前打开，见 H264DecoderConfig.DisableLowLatency。
		if !cfg.DisableLowLatency {
			attrs.SetUINT32(mf.MF_LOW_LATENCY, 1)
		}
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

	// 6. 查询输出流信息，决定缓冲区由谁分配。
	d.refreshOutputStreamInfo()

	d.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0)
	d.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0)
	return nil
}

// enableDXVA 创建 D3D11 设备与设备管理器并交给解码器 MFT，启用 DXVA 硬件解码。
// 任何一步失败都记录原因并保持未启用状态，调用方无需特殊处理。
func (d *H264MFTDecoder) enableDXVA() {
	var device *d3d11.ID3D11Device
	var deviceCtx *d3d11.ID3D11DeviceContext

	if d.cfg.Device != nil && d.cfg.DeviceCtx != nil {
		device = d.cfg.Device
		deviceCtx = d.cfg.DeviceCtx
		d.ownsDevice = false
	} else {
		var err error
		device, deviceCtx, err = gpu.NewVideoCapableDevice()
		if err != nil {
			debugf("  DXVA 不可用（建 D3D11 设备失败）: %v", err)
			return
		}
		d.ownsDevice = true
	}

	if err := gpu.EnableMultithreadProtection(device); err != nil {
		debugf("  DXVA 不可用（多线程保护失败）: %v", err)
		if d.ownsDevice {
			device.Release()
			deviceCtx.Release()
			d.ownsDevice = false
		}
		return
	}
	mgr, _, err := gpu.NewDXGIDeviceManager(device)
	if err != nil {
		debugf("  DXVA 不可用（建设备管理器失败）: %v", err)
		if d.ownsDevice {
			device.Release()
			deviceCtx.Release()
			d.ownsDevice = false
		}
		return
	}
	hr := d.transform.ProcessMessage(mf.MFT_MESSAGE_SET_D3D_MANAGER, uintptr(unsafe.Pointer(mgr)))
	if d3d.HRESULT(hr).Failed() {
		debugf("  DXVA 不可用（MFT 拒绝设备管理器）: %v", d3d.HRESULT(hr))
		mgr.Release()
		if d.ownsDevice {
			device.Release()
			deviceCtx.Release()
			d.ownsDevice = false
		}
		return
	}
	d.devMgr = mgr
	d.device = device
	d.deviceCtx = deviceCtx
	d.dxva = true
	debugf("  DXVA 已启用（D3D11 设备管理器已交给解码器）")
}

func (d *H264MFTDecoder) disableDXVA() {
	if d.devMgr != nil {
		d.devMgr.Release()
		d.devMgr = nil
	}
	if d.ownsDevice {
		if d.deviceCtx != nil {
			d.deviceCtx.Release()
		}
		if d.device != nil {
			d.device.Release()
		}
		d.ownsDevice = false
	}
	d.device = nil
	d.deviceCtx = nil
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

// outTypeCand 是解码器报告的一个可选输出类型。
type outTypeCand struct {
	mt      *mf.IMFMediaType
	sub     windows.GUID
	w, h    uint32
	hasSize bool
}

func (d *H264MFTDecoder) setOutputType(cfg H264DecoderConfig) error {
	var cands []outTypeCand
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
		c := outTypeCand{mt: mt, sub: sub, hasSize: hasSize}
		if hasSize {
			c.w, c.h = uint32(fs>>32), uint32(fs&0xFFFFFFFF)
			seen = append(seen, fmt.Sprintf("%s %dx%d", mf.SubtypeName(sub), c.w, c.h))
		} else {
			seen = append(seen, mf.SubtypeName(sub)+"(无尺寸)")
		}
		cands = append(cands, c)
	}
	debugf("  可选输出类型: %v", seen)
	if len(cands) == 0 {
		return fmt.Errorf("decoder exposes no output media type")
	}

	// 优先级：subtype 匹配且带 MF_MT_FRAME_SIZE > subtype 匹配 > 第一个。
	//
	// 为什么要挑带尺寸的：还没喂数据（SPS 还没解析）时，解码器报出来的
	// 往往只有一个占位尺寸（实测本机是 NV12 1920x1080，真实码流是
	// 2560x1440）。真正的尺寸要等 MF_E_TRANSFORM_STREAM_CHANGE 之后
	// 重新枚举才拿得到。所以只要有一个带尺寸的类型就必须优先选它。
	pick := -1
	for i, c := range cands {
		if c.sub == cfg.Output && c.hasSize {
			pick = i
			break
		}
	}
	if pick < 0 {
		for i, c := range cands {
			if c.sub == cfg.Output {
				pick = i
				break
			}
		}
	}
	if pick < 0 {
		pick = 0
	}
	chosen := cands[pick]
	for i, c := range cands {
		if i != pick {
			c.mt.Release()
		}
	}
	defer chosen.mt.Release()

	// 尺寸/stride 先清零再填：否则上一次协商（或占位尺寸）的旧值会残留，
	// 调 OutputFormat() 的人会拿到一个和真实码流不符的分辨率。
	d.outWidth, d.outHeight, d.outStride = 0, 0, 0
	chosen.mt.GetGUID(mf.MF_MT_SUBTYPE, &d.outSubtype)
	if chosen.hasSize {
		d.outWidth, d.outHeight = chosen.w, chosen.h
	}
	if d.outWidth == 0 {
		d.outWidth, d.outHeight = cfg.Width, cfg.Height
	}
	var stride uint32
	if !d3d.HRESULT(chosen.mt.GetUINT32(mf.MF_MT_DEFAULT_STRIDE, &stride)).Failed() {
		d.outStride = int32(stride)
	}
	if d.outWidth == 0 || d.outHeight == 0 {
		debugf("  输出类型 %s 没给尺寸（还没解析到 SPS），等 stream change 再定", mf.SubtypeName(d.outSubtype))
	}

	if hr := d.transform.SetOutputType(0, chosen.mt, 0); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("SetOutputType(%s): %w", mf.SubtypeName(d.outSubtype), d3d.HRESULT(hr))
	}
	return nil
}

func (d *H264MFTDecoder) refreshOutputStreamInfo() {
	var streamInfo mf.MFT_OUTPUT_STREAM_INFO
	d.transform.GetOutputStreamInfo(0, &streamInfo)
	d.providesSamples = (streamInfo.DwFlags & mf.MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) != 0
	d.outputBufSize = streamInfo.CbSize
	if d.outputBufSize == 0 {
		d.outputBufSize = 8 * 1024 * 1024
	}
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
		warnf("DXVA 解码出错(%v)，重建为软件解码器并重试", err)
		if d.degradeHook != nil {
			d.degradeHook(err)
		}
		rerr := d.rebuildWithoutDXVA()
		if rerr != nil {
			warnf("  重建软件解码器失败: %v", rerr)
			return frames, err
		}
		warnf("  已重建为软件解码器，后续帧都走 CPU（DecodedFrame.Texture 为 nil）")
		return d.decodeOnce(au, ts, dur)
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

	// 同步 MFT：ProcessInput 会因为背压返回 MF_E_NOTACCEPTING，意思是
	// "我还有输出没取走，先排空再来"。这是正常信号不是失败——实测全速/长时间
	// 播放时会偶发。正确做法是排空输出后重试同一个 sample，而不是整个重建
	// 解码器（重建会静默退化成软解）。排空出来的帧也要带回去，别丢。
	const maxNotAcceptingRetries = 50
	for attempt := 0; ; attempt++ {
		hr := d.transform.ProcessInput(0, sample, 0)
		if !d3d.HRESULT(hr).Failed() {
			break
		}
		if uint32(hr) != mf.MF_E_NOTACCEPTING || attempt >= maxNotAcceptingRetries {
			sample.Release()
			return frames, fmt.Errorf("ProcessInput: %w", d3d.HRESULT(hr))
		}
		got, derr := d.drainOutputs()
		frames = append(frames, got...)
		if derr != nil {
			sample.Release()
			return frames, derr
		}
		// 背压的根因常常是"已解码的帧还没被归还、surface 池满了"。给上层一个
		// 归还的机会再睡，否则只是把等待时间整段浪费掉。
		if d.stallHook != nil {
			d.stallHook()
		}
		time.Sleep(time.Millisecond)
	}
	sample.Release()
	tail, err := d.drainOutputs()
	return append(frames, tail...), err
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
//
// 一次最多取回 maxDrainPerCall 帧：0 拷贝路径下每取一帧就占住解码器的一张
// surface，要等调用方渲染完 Release 才归还。而解码器的 surface 池只有个位数
// （实测 ArraySize=6），一次抓太多会让下一次 ProcessOutput 等不到空闲
// surface 而永久阻塞——Flush 的时候就踩过这个坑。剩下的帧下一次再取。
func (d *H264MFTDecoder) drainOutputs() ([]*DecodedFrame, error) {
	var frames []*DecodedFrame
	for len(frames) < maxDrainPerCall {
		f, more, err := d.processOutput()
		if err != nil {
			return frames, err
		}
		if f == nil {
			return frames, nil
		}
		frames = append(frames, f)
		if !more {
			return frames, nil
		}
	}
	return frames, nil
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

	for streamChangeRetries := 0; streamChangeRetries < 3; streamChangeRetries++ {
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
			if d3d.HRESULT(hr).Failed() || outData.PSample == nil {
				allocated.Release()
			}
		}
		if uint32(hr) == mf.MF_E_TRANSFORM_NEED_MORE_INPUT {
			return nil, false, nil
		}
		if uint32(hr) == mf.MF_E_TRANSFORM_STREAM_CHANGE {
			if err := d.setOutputType(d.cfg); err != nil {
				return nil, false, fmt.Errorf("renegotiate output after stream change: %w", err)
			}
			d.refreshOutputStreamInfo()
			continue
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
	return nil, false, fmt.Errorf("too many consecutive stream changes")
}

func (d *H264MFTDecoder) extractFrame(sample *mf.IMFSample) (*DecodedFrame, error) {
	if d.dxva && d.device != nil && !d.ownsDevice {
		return d.extractFrameGPU(sample)
	}
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

// extractFrameGPU 取出 DXVA 解码器输出的纹理。
//
// 0 拷贝：不做 CopySubresourceRegion，直接把解码器 surface 池里的那张纹理
// 带出去。GetResource 返回的指针已经 AddRef 过，这里再 AddRef 一次
// IMFSample——只要 sample 还活着，解码器就不会把这张 surface 拿去复用。
// 代价是调用方必须渲染完再调 DecodedFrame.Release()，见该方法的注释。
func (d *H264MFTDecoder) extractFrameGPU(sample *mf.IMFSample) (*DecodedFrame, error) {
	var buf *mf.IMFMediaBuffer
	if hr := sample.GetBufferByIndex(0, &buf); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("GetBufferByIndex: %w", d3d.HRESULT(hr))
	}
	defer buf.Release()

	var dxgiBuf *mf.IMFDXGIBuffer
	if hr := buf.QueryInterface(mf.IID_IMFDXGIBuffer, &dxgiBuf); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("QueryInterface(IMFDXGIBuffer): %w", d3d.HRESULT(hr))
	}
	defer dxgiBuf.Release()

	var srcTexPtr unsafe.Pointer
	if hr := dxgiBuf.GetResource(d3d11.IID_ID3D11Texture2D, &srcTexPtr); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("GetResource(ID3D11Texture2D): %w", d3d.HRESULT(hr))
	}
	srcTex := (*d3d11.ID3D11Texture2D)(srcTexPtr)

	var subIdx uint32
	dxgiBuf.GetSubresourceIndex(&subIdx)

	var desc d3d11.D3D11_TEXTURE2D_DESC
	srcTex.GetDesc(&desc)

	// 抓住 sample：它是这张 surface 的"租约"，Release 之前解码器不会回收。
	sample.AddRef()

	var ts, dur int64
	sample.GetSampleTime(&ts)
	sample.GetSampleDuration(&dur)

	f := &DecodedFrame{
		Texture:   srcTex,
		SubIndex:  subIdx,
		Subtype:   d.outSubtype,
		Width:     desc.Width,
		Height:    desc.Height,
		Stride:    d.outStride,
		Timestamp: time.Duration(ts * 100),
		Duration:  time.Duration(dur * 100),
	}
	f.release = func() {
		srcTex.Release() // 归还 GetResource 给的引用
		sample.Release() // 归还 sample → surface 回到解码器池
	}
	return f, nil
}

// Flush 通知解码器排空缓冲并取回剩余帧。
//
// 一次最多返回 maxDrainPerCall 帧（原因见 drainOutputs 的注释：0 拷贝下每帧
// 都占着解码器 surface，一次抓太多会把池子占满导致 ProcessOutput 永久阻塞）。
// 调用方应当渲染 + Release 之后**反复调用**，直到返回 0 帧才算真的排空：
//
//	for {
//	    tail, _ := dec.Flush()
//	    if len(tail) == 0 { break }
//	    渲染并 Release(tail...)
//	}
func (d *H264MFTDecoder) Flush() ([]*DecodedFrame, error) {
	if !d.draining {
		d.transform.ProcessMessage(mf.MFT_MESSAGE_COMMAND_DRAIN, 0)
		d.draining = true
	}
	if d.isAsync {
		var frames []*DecodedFrame
		for {
			et, err := d.getNextEventType()
			if err != nil {
				d.draining = false
				return frames, err
			}
			if et == mf.METransformDrainComplete {
				d.draining = false
				return frames, nil
			}
			if et == mf.METransformHaveOutput {
				f, _, err := d.processOutput()
				if err != nil {
					d.draining = false
					return frames, err
				}
				if f != nil {
					frames = append(frames, f)
				}
				if len(frames) >= maxDrainPerCall {
					return frames, nil
				}
			}
		}
	}
	frames, err := d.drainOutputs()
	if len(frames) == 0 || err != nil {
		d.draining = false // 排空结束
	}
	return frames, err
}

const eventPollTimeout = 5 * time.Second

func (d *H264MFTDecoder) waitForEventType(target uint32) error {
	deadline := time.Now().Add(eventPollTimeout)
	for {
		et, err := d.getNextEvent()
		if err != nil {
			return err
		}
		if et == target {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("waitForEventType(%d): timed out after %v", target, eventPollTimeout)
		}
	}
}

func (d *H264MFTDecoder) getNextEventType() (uint32, error) {
	return d.getNextEvent()
}

func (d *H264MFTDecoder) getNextEvent() (uint32, error) {
	deadline := time.Now().Add(eventPollTimeout)
	for {
		var ev *mf.IMFMediaEvent
		hr := d.eventGen.GetEvent(mf.MF_EVENT_FLAG_NO_WAIT, &ev)
		if uint32(hr) == mf.MF_E_NO_EVENTS_AVAILABLE {
			if time.Now().After(deadline) {
				return 0, fmt.Errorf("getNextEvent: timed out after %v", eventPollTimeout)
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if d3d.HRESULT(hr).Failed() {
			return 0, fmt.Errorf("GetEvent: %w", d3d.HRESULT(hr))
		}
		var et uint32
		ev.GetType(&et)
		ev.Release()
		return et, nil
	}
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
	if d.ownsDevice {
		if d.deviceCtx != nil {
			d.deviceCtx.Release()
		}
		if d.device != nil {
			d.device.Release()
		}
		d.ownsDevice = false
	}
	d.device = nil
	d.deviceCtx = nil
}
