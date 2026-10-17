package encode

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"golang.org/x/sys/windows"

	"mfcapture/mf"
)

// H264EncoderConfig holds parameters for the hardware H264 encoder.
type H264EncoderConfig struct {
	Width      uint32
	Height     uint32
	FPSNum     uint32
	FPSDen     uint32
	BitrateBps uint32
	LowLatency bool
	GOPSize    uint32 // 0 = encoder default
}

// H264MFTEncoder wraps a Media Foundation hardware (or software fallback) H264
// encoder MFT, driven via IMFTransform for direct access to the raw H264 NAL
// bitstream. Supports both sync and async MFTs.
type H264MFTEncoder struct {
	transform  *mf.IMFTransform
	codecAPI   *mf.ICodecAPI
	devManager *mf.IMFDXGIDeviceManager
	eventGen   *mf.IMFMediaEventGenerator

	width, height      uint32
	IsHardware         bool
	isAsync            bool
	providesSamples    bool
	outputBufSize      uint32
	beginStreamingSent bool
	startOfStreamSent  bool

	mu           sync.Mutex
	forceNextIDR bool
}

// NewH264MFTEncoder enumerates, activates, and configures a hardware H264 encoder MFT.
// Falls back to software if no hardware MFT is available.
func NewH264MFTEncoder(cfg H264EncoderConfig, device *d3d11.ID3D11Device, devManager *mf.IMFDXGIDeviceManager) (*H264MFTEncoder, error) {
	if cfg.FPSNum == 0 {
		cfg.FPSNum = 30
	}
	if cfg.FPSDen == 0 {
		cfg.FPSDen = 1
	}
	if cfg.BitrateBps == 0 {
		cfg.BitrateBps = 8_000_000
	}

	transform, isHW, err := findAndActivateEncoder()
	if err != nil {
		return nil, err
	}

	enc := &H264MFTEncoder{
		transform:  transform,
		devManager: devManager,
		width:      cfg.Width,
		height:     cfg.Height,
		IsHardware: isHW,
	}

	if err := enc.configure(cfg); err != nil {
		enc.Release()
		return nil, err
	}

	return enc, nil
}

func findAndActivateEncoder() (*mf.IMFTransform, bool, error) {
	outputType := mf.MFT_REGISTER_TYPE_INFO{
		GuidMajorType: mf.MFMediaType_Video,
		GuidSubtype:   mf.MFVideoFormat_H264,
	}

	activates, err := mf.MFTEnumEx(
		mf.MFT_CATEGORY_VIDEO_ENCODER,
		mf.MFT_ENUM_FLAG_HARDWARE|mf.MFT_ENUM_FLAG_SORTANDFILTER,
		nil,
		&outputType,
	)
	if err != nil {
		return nil, false, fmt.Errorf("MFTEnumEx(hardware): %w", err)
	}
	if len(activates) > 0 {
		t, e := activateFirst(activates)
		if e == nil {
			return t, true, nil
		}
	}

	activates, err = mf.MFTEnumEx(
		mf.MFT_CATEGORY_VIDEO_ENCODER,
		mf.MFT_ENUM_FLAG_SYNCMFT|mf.MFT_ENUM_FLAG_SORTANDFILTER,
		nil,
		&outputType,
	)
	if err != nil {
		return nil, false, fmt.Errorf("MFTEnumEx(software): %w", err)
	}
	if len(activates) == 0 {
		return nil, false, fmt.Errorf("no H264 encoder MFT found")
	}
	t, e := activateFirst(activates)
	if e != nil {
		return nil, false, e
	}
	return t, false, nil
}

func activateFirst(activates []*mf.IMFActivate) (*mf.IMFTransform, error) {
	var transform *mf.IMFTransform
	var lastErr error
	for i, act := range activates {
		hr := act.ActivateObject(mf.IID_IMFTransform, unsafe.Pointer(&transform))
		act.Release()
		if d3d.HRESULT(hr).Failed() {
			lastErr = fmt.Errorf("ActivateObject: %w", d3d.HRESULT(hr))
			continue
		}
		for j := i + 1; j < len(activates); j++ {
			activates[j].Release()
		}
		return transform, nil
	}
	return nil, lastErr
}

func (enc *H264MFTEncoder) configure(cfg H264EncoderConfig) error {
	// Step 1: Unlock async MFT and set low-latency attributes FIRST.
	var attrs *mf.IMFAttributes
	if hr := enc.transform.GetAttributes(&attrs); !d3d.HRESULT(hr).Failed() && attrs != nil {
		attrs.SetUINT32(mf.MF_TRANSFORM_ASYNC_UNLOCK, 1)
		if cfg.LowLatency {
			attrs.SetUINT32(mf.MF_LOW_LATENCY, 1)
		}
		attrs.Release()
	}

	// Step 2: Detect if this is an async MFT by QI for IMFMediaEventGenerator.
	var eventGen *mf.IMFMediaEventGenerator
	qiHr := enc.transform.QueryInterface(mf.IID_IMFMediaEventGenerator, &eventGen)
	if !d3d.HRESULT(qiHr).Failed() && eventGen != nil {
		enc.eventGen = eventGen
		enc.isAsync = true
	}

	// Step 2b: GOP 必须在协商媒体类型之前设置。NVENC 的 MFT 在类型确定后会忽略
	// CODECAPI_AVEncMPVGOPSize（实测 GOP=10 时只有首帧是 IDR，之后再无关键帧），
	// 而远程桌面恰恰依赖周期性关键帧兜底丢包恢复。
	var codecAPI *mf.ICodecAPI
	qiHr = int32(enc.transform.QueryInterface(mf.IID_ICodecAPI, &codecAPI))
	if !d3d.HRESULT(qiHr).Failed() && codecAPI != nil {
		enc.codecAPI = codecAPI
		if cfg.GOPSize > 0 {
			codecAPI.SetUINT32(mf.CODECAPI_AVEncMPVGOPSize, cfg.GOPSize)
		}
	}

	// Step 3: Set output type (H264).
	outType, err := mf.NewOutputMediaType(cfg.Width, cfg.Height, cfg.FPSNum, cfg.FPSDen, cfg.BitrateBps)
	if err != nil {
		return fmt.Errorf("NewOutputMediaType: %w", err)
	}
	defer outType.Release()

	if hr := enc.transform.SetOutputType(0, outType, 0); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("SetOutputType: %w", d3d.HRESULT(hr))
	}

	// Step 4: Set input type (BGRA/ARGB32).
	inType, err := mf.NewInputMediaType(cfg.Width, cfg.Height, cfg.FPSNum, cfg.FPSDen)
	if err != nil {
		return fmt.Errorf("NewInputMediaType: %w", err)
	}
	defer inType.Release()

	if hr := enc.transform.SetInputType(0, inType, 0); d3d.HRESULT(hr).Failed() {
		// ARGB32 只是"硬件编码器通常支持"的格式，并非所有编码器都支持。
		// 把实际可用的输入类型一并报出来，否则调用方只看到 0xC00D36B4 无从下手。
		return fmt.Errorf("SetInputType(ARGB32 %dx%d@%d/%d): %w\n"+
			"该编码器实际接受的输入类型: %v\n"+
			"提示: 微软的**软件** H264 编码器 MFT 只接受 NV12/I420/IYUV/YV12/YUY2，不接受 ARGB32；"+
			"只有在有硬件编码器（Intel QSV / NVIDIA NVENC / AMD AMF）的机器上 ARGB32 才可用",
			cfg.Width, cfg.Height, cfg.FPSNum, cfg.FPSDen,
			d3d.HRESULT(hr), AvailableInputTypes(enc.transform))
	}

	// Step 5: Set D3D manager AFTER types are negotiated.
	hr := enc.transform.ProcessMessage(
		mf.MFT_MESSAGE_SET_D3D_MANAGER,
		uintptr(unsafe.Pointer(enc.devManager)),
	)
	if d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("ProcessMessage(SET_D3D_MANAGER): %w", d3d.HRESULT(hr))
	}

	// Step 6: Query output stream info for buffer allocation strategy.
	var streamInfo mf.MFT_OUTPUT_STREAM_INFO
	enc.transform.GetOutputStreamInfo(0, &streamInfo)
	enc.providesSamples = (streamInfo.DwFlags & mf.MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) != 0
	enc.outputBufSize = streamInfo.CbSize
	if enc.outputBufSize == 0 {
		enc.outputBufSize = 1024 * 1024
	}

	// Step 7: Configure codec properties via ICodecAPI (GOP was set in step 2b).
	if codecAPI := enc.codecAPI; codecAPI != nil {
		codecAPI.SetUINT32(mf.CODECAPI_AVEncCommonRateControlMode, mf.EAVEncCommonRateControlMode_CBR)
		codecAPI.SetUINT32(mf.CODECAPI_AVEncCommonMeanBitRate, cfg.BitrateBps)
		if cfg.LowLatency {
			codecAPI.SetBool(mf.CODECAPI_AVLowLatencyMode, true)
		}
	}

	// Step 8: Notify the MFT that streaming is about to begin.
	if hr := enc.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("ProcessMessage(NOTIFY_BEGIN_STREAMING): %w", d3d.HRESULT(hr))
	}
	enc.beginStreamingSent = true
	if hr := enc.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("ProcessMessage(NOTIFY_START_OF_STREAM): %w", d3d.HRESULT(hr))
	}
	enc.startOfStreamSent = true

	return nil
}

// AvailableInputTypes 列出编码器在输入流 0 上声明支持的媒体类型，形如 "NV12 1920x1080"。
// 必须在 SetOutputType 之后调用才有意义（编码器在输出类型确定前不会给出完整的输入类型列表）。
// 主要用于诊断和错误信息；失败时返回空切片。
func AvailableInputTypes(t *mf.IMFTransform) []string {
	var out []string
	for i := uint32(0); i < 64; i++ {
		var mt *mf.IMFMediaType
		if hr := t.GetInputAvailableType(0, i, &mt); d3d.HRESULT(hr).Failed() {
			break
		}
		var sub windows.GUID
		mt.GetGUID(mf.MF_MT_SUBTYPE, &sub)
		var frameSize uint64
		gotSize := !d3d.HRESULT(mt.GetUINT64(mf.MF_MT_FRAME_SIZE, &frameSize)).Failed()
		mt.Release()

		if gotSize {
			out = append(out, fmt.Sprintf("%s %dx%d",
				mf.SubtypeName(sub), uint32(frameSize>>32), uint32(frameSize&0xFFFFFFFF)))
		} else {
			out = append(out, mf.SubtypeName(sub))
		}
	}
	return out
}

// Encode sends one input sample to the encoder and returns encoded frames.
func (enc *H264MFTEncoder) Encode(sample *mf.IMFSample) (*EncodedFrame, error) {
	enc.mu.Lock()
	if enc.forceNextIDR && enc.codecAPI != nil {
		enc.codecAPI.SetUINT32(mf.CODECAPI_AVEncVideoForceKeyFrame, 1)
		enc.forceNextIDR = false
	}
	enc.mu.Unlock()

	if enc.isAsync {
		return enc.encodeAsync(sample)
	}
	return enc.encodeSync(sample)
}

// encodeSync is the straightforward sync MFT path: ProcessInput then ProcessOutput.
func (enc *H264MFTEncoder) encodeSync(sample *mf.IMFSample) (*EncodedFrame, error) {
	hr := enc.transform.ProcessInput(0, sample, 0)
	if d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("ProcessInput: %w", d3d.HRESULT(hr))
	}
	return enc.processOutput()
}

// encodeAsync uses the event-driven model required by async hardware MFTs:
// wait for METransformNeedInput → ProcessInput → wait for METransformHaveOutput → ProcessOutput.
func (enc *H264MFTEncoder) encodeAsync(sample *mf.IMFSample) (*EncodedFrame, error) {
	// Drain any pending METransformNeedInput event.
	if err := enc.waitForEventType(mf.METransformNeedInput); err != nil {
		return nil, fmt.Errorf("waiting for NeedInput: %w", err)
	}

	hr := enc.transform.ProcessInput(0, sample, 0)
	if d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("ProcessInput: %w", d3d.HRESULT(hr))
	}

	// Poll for METransformHaveOutput. The encoder may not have output ready yet
	// (it may need more input), so use a non-blocking check first, then block.
	eventType, err := enc.getNextEventType()
	if err != nil {
		return nil, fmt.Errorf("getNextEventType: %w", err)
	}

	if eventType == mf.METransformHaveOutput {
		return enc.processOutput()
	}
	// METransformNeedInput or other event — no output yet.
	return nil, nil
}

// waitForEventType blocks until the specified event type arrives, consuming other events.
func (enc *H264MFTEncoder) waitForEventType(target uint32) error {
	for {
		var event *mf.IMFMediaEvent
		hr := enc.eventGen.GetEvent(0, &event)
		if d3d.HRESULT(hr).Failed() {
			return fmt.Errorf("GetEvent: %w", d3d.HRESULT(hr))
		}
		var et uint32
		event.GetType(&et)
		event.Release()
		if et == target {
			return nil
		}
	}
}

// getNextEventType blocks and returns the type of the next event.
func (enc *H264MFTEncoder) getNextEventType() (uint32, error) {
	var event *mf.IMFMediaEvent
	hr := enc.eventGen.GetEvent(0, &event)
	if d3d.HRESULT(hr).Failed() {
		return 0, fmt.Errorf("GetEvent: %w", d3d.HRESULT(hr))
	}
	var et uint32
	event.GetType(&et)
	event.Release()
	return et, nil
}

// processOutput calls ProcessOutput with correct buffer allocation strategy.
func (enc *H264MFTEncoder) processOutput() (*EncodedFrame, error) {
	outData := mf.MFT_OUTPUT_DATA_BUFFER{}
	var allocatedSample *mf.IMFSample

	if !enc.providesSamples {
		// MFT does NOT provide its own samples — we must allocate.
		if hr := mf.MFCreateSample(&allocatedSample); d3d.HRESULT(hr).Failed() {
			return nil, fmt.Errorf("MFCreateSample: %w", d3d.HRESULT(hr))
		}
		var outBuf *mf.IMFMediaBuffer
		if hr := mf.MFCreateMemoryBuffer(enc.outputBufSize, &outBuf); d3d.HRESULT(hr).Failed() {
			allocatedSample.Release()
			return nil, fmt.Errorf("MFCreateMemoryBuffer: %w", d3d.HRESULT(hr))
		}
		allocatedSample.AddBuffer(outBuf)
		outBuf.Release()
		outData.PSample = allocatedSample
	}
	// else: PSample stays nil; MFT will fill it in.

	var status uint32
	hr := enc.transform.ProcessOutput(0, 1, &outData, &status)

	if uint32(hr) == mf.MF_E_TRANSFORM_NEED_MORE_INPUT {
		if allocatedSample != nil {
			allocatedSample.Release()
		}
		return nil, nil
	}
	if d3d.HRESULT(hr).Failed() {
		if allocatedSample != nil {
			allocatedSample.Release()
		}
		return nil, fmt.Errorf("ProcessOutput: %w", d3d.HRESULT(hr))
	}

	result := outData.PSample
	if result == nil {
		return nil, nil
	}
	defer result.Release()

	return enc.extractFrame(result)
}

func (enc *H264MFTEncoder) extractFrame(sample *mf.IMFSample) (*EncodedFrame, error) {
	var buf *mf.IMFMediaBuffer
	if hr := sample.ConvertToContiguousBuffer(&buf); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("ConvertToContiguousBuffer: %w", d3d.HRESULT(hr))
	}
	defer buf.Release()

	var pbuf unsafe.Pointer
	var curLen uint32
	if hr := buf.Lock(&pbuf, nil, &curLen); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("Lock: %w", d3d.HRESULT(hr))
	}
	data := make([]byte, curLen)
	copy(data, unsafe.Slice((*byte)(pbuf), curLen))
	buf.Unlock()

	var sampleTime int64
	sample.GetSampleTime(&sampleTime)
	var sampleDur int64
	sample.GetSampleDuration(&sampleDur)

	var cleanPoint uint32
	sample.GetUINT32(mf.MFSampleExtension_CleanPoint, &cleanPoint)

	return &EncodedFrame{
		Data:       data,
		Timestamp:  hnsToGoDuration(sampleTime),
		Duration:   hnsToGoDuration(sampleDur),
		IsKeyFrame: cleanPoint != 0,
	}, nil
}

func hnsToGoDuration(hns int64) time.Duration {
	return time.Duration(hns * 100)
}

// ForceKeyFrame requests that the next encoded frame be an IDR frame.
func (enc *H264MFTEncoder) ForceKeyFrame() error {
	enc.mu.Lock()
	defer enc.mu.Unlock()
	enc.forceNextIDR = true
	return nil
}

// SetBitrate changes the target bitrate at runtime.
func (enc *H264MFTEncoder) SetBitrate(bps uint32) error {
	if enc.codecAPI == nil {
		return fmt.Errorf("ICodecAPI not available")
	}
	return enc.codecAPI.SetUINT32(mf.CODECAPI_AVEncCommonMeanBitRate, bps)
}

// Flush drains any buffered output frames from the encoder.
func (enc *H264MFTEncoder) Flush() ([]*EncodedFrame, error) {
	enc.transform.ProcessMessage(mf.MFT_MESSAGE_COMMAND_DRAIN, 0)

	var frames []*EncodedFrame
	if enc.isAsync {
		for {
			et, err := enc.getNextEventType()
			if err != nil {
				return frames, nil
			}
			if et == mf.METransformDrainComplete {
				break
			}
			if et == mf.METransformHaveOutput {
				f, ferr := enc.processOutput()
				if ferr != nil {
					return frames, ferr
				}
				if f != nil {
					frames = append(frames, f)
				}
			}
		}
	} else {
		for {
			f, err := enc.processOutput()
			if err != nil || f == nil {
				break
			}
			frames = append(frames, f)
		}
	}
	return frames, nil
}

// Release frees all COM resources.
func (enc *H264MFTEncoder) Release() {
	if enc.transform != nil {
		if enc.startOfStreamSent {
			enc.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0)
			enc.startOfStreamSent = false
		}
		if enc.beginStreamingSent {
			enc.transform.ProcessMessage(mf.MFT_MESSAGE_NOTIFY_END_STREAMING, 0)
			enc.beginStreamingSent = false
		}
	}
	if enc.codecAPI != nil {
		enc.codecAPI.Release()
		enc.codecAPI = nil
	}
	if enc.eventGen != nil {
		enc.eventGen.Release()
		enc.eventGen = nil
	}
	if enc.transform != nil {
		enc.transform.Release()
		enc.transform = nil
	}
}

var _ Encoder = (*H264MFTEncoder)(nil)

var _ windows.GUID
