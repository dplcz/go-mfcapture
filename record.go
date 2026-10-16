package mfcapture

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"

	"mfcapture/capture"
	"mfcapture/mf"
)

// Config configures the legacy Record() function, which writes directly to an MP4 file
// via IMFSinkWriter. For the streaming pipeline (raw H264 NAL via callback), use
// NewSession() + Session.Start() instead.
type Config struct {
	OutputPath string
	Duration   time.Duration
	BitrateBps uint32
	Output     uint   // monitor index
	MaxFPS     uint32 // 0 = unlimited
}

const (
	nominalFPSNum, nominalFPSDen = 30, 1
	recordAcquireTimeoutMs       = 500
	recordNominalFrameDurHns     = int64(10_000_000) * int64(nominalFPSDen) / int64(nominalFPSNum)
)

// Record runs the legacy zero-copy pipeline: DXGI desktop duplication -> GPU texture ->
// IMFSinkWriter (hardware H264 encoder) -> MP4 file.
func Record(ctx context.Context, cfg Config) error {
	if cfg.BitrateBps == 0 {
		cfg.BitrateBps = 8_000_000
	}

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

	if err := capture.EnableMultithreadProtection(device); err != nil {
		return fmt.Errorf("EnableMultithreadProtection: %w", err)
	}

	dup, err := capture.NewDuplicator(device, deviceCtx, cfg.Output)
	if err != nil {
		return fmt.Errorf("NewDuplicator: %w", err)
	}
	defer dup.Release()

	width, height := dup.Width(), dup.Height()
	if width == 0 || height == 0 {
		return fmt.Errorf("invalid output size %dx%d", width, height)
	}

	var resetToken uint32
	var devManager *mf.IMFDXGIDeviceManager
	if hr := mf.MFCreateDXGIDeviceManager(&resetToken, &devManager); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("MFCreateDXGIDeviceManager: %w", d3d.HRESULT(hr))
	}
	defer devManager.Release()

	if hr := devManager.ResetDevice(device, resetToken); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("IMFDXGIDeviceManager.ResetDevice: %w", d3d.HRESULT(hr))
	}

	outputType, err := mf.NewOutputMediaType(width, height, nominalFPSNum, nominalFPSDen, cfg.BitrateBps)
	if err != nil {
		return fmt.Errorf("NewOutputMediaType: %w", err)
	}
	defer outputType.Release()

	inputType, err := mf.NewInputMediaType(width, height, nominalFPSNum, nominalFPSDen)
	if err != nil {
		return fmt.Errorf("NewInputMediaType: %w", err)
	}
	defer inputType.Release()

	var sinkAttrs *mf.IMFAttributes
	if hr := mf.MFCreateAttributes(&sinkAttrs, 2); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("MFCreateAttributes: %w", d3d.HRESULT(hr))
	}
	defer sinkAttrs.Release()

	if hr := sinkAttrs.SetUnknown(mf.MF_SINK_WRITER_D3D_MANAGER, unsafe.Pointer(devManager)); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("SetUnknown(MF_SINK_WRITER_D3D_MANAGER): %w", d3d.HRESULT(hr))
	}
	if hr := sinkAttrs.SetUINT32(mf.MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS, 1); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("SetUINT32(MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS): %w", d3d.HRESULT(hr))
	}

	outputURL, err := windows.UTF16PtrFromString(cfg.OutputPath)
	if err != nil {
		return fmt.Errorf("UTF16PtrFromString: %w", err)
	}

	var sinkWriter *mf.IMFSinkWriter
	if hr := mf.MFCreateSinkWriterFromURL(outputURL, sinkAttrs, &sinkWriter); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("MFCreateSinkWriterFromURL: %w", d3d.HRESULT(hr))
	}
	defer sinkWriter.Release()

	var streamIndex uint32
	if hr := sinkWriter.AddStream(outputType, &streamIndex); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("AddStream: %w", d3d.HRESULT(hr))
	}

	if hr := sinkWriter.SetInputMediaType(streamIndex, inputType, nil); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("SetInputMediaType: %w", d3d.HRESULT(hr))
	}

	if hr := sinkWriter.BeginWriting(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("BeginWriting: %w", d3d.HRESULT(hr))
	}

	limiter := newFrameLimiter(cfg.MaxFPS)

	start := time.Now()
	deadline := start.Add(cfg.Duration)
	frameCount := 0
	lastHns := int64(-1)

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		default:
		}
		now := time.Now()
		if now.After(deadline) {
			break loop
		}

		frame, acqErr := dup.AcquireFrame(recordAcquireTimeoutMs)
		if acqErr != nil {
			if errors.Is(acqErr, capture.ErrTimeout) {
				continue
			}
			if errors.Is(acqErr, capture.ErrAccessLostRecovered) {
				continue
			}
			return fmt.Errorf("AcquireFrame: %w", acqErr)
		}

		hns := now.Sub(start).Nanoseconds() / 100
		if !limiter.allow(hns) {
			continue
		}

		var buffer *mf.IMFMediaBuffer
		if hr := mf.MFCreateDXGISurfaceBuffer(d3d11.IID_ID3D11Texture2D, unsafe.Pointer(frame.Texture), 0, false, &buffer); d3d.HRESULT(hr).Failed() {
			return fmt.Errorf("MFCreateDXGISurfaceBuffer: %w", d3d.HRESULT(hr))
		}
		if hr := buffer.SetCurrentLength(width * height * 4); d3d.HRESULT(hr).Failed() {
			buffer.Release()
			return fmt.Errorf("SetCurrentLength: %w", d3d.HRESULT(hr))
		}

		var sample *mf.IMFSample
		if hr := mf.MFCreateSample(&sample); d3d.HRESULT(hr).Failed() {
			buffer.Release()
			return fmt.Errorf("MFCreateSample: %w", d3d.HRESULT(hr))
		}

		if hr := sample.AddBuffer(buffer); d3d.HRESULT(hr).Failed() {
			buffer.Release()
			sample.Release()
			return fmt.Errorf("AddBuffer: %w", d3d.HRESULT(hr))
		}
		buffer.Release()

		duration := recordNominalFrameDurHns
		if lastHns >= 0 {
			if d := hns - lastHns; d > 0 {
				duration = d
			}
		}
		lastHns = hns

		sample.SetSampleTime(hns)
		sample.SetSampleDuration(duration)

		if hr := sinkWriter.WriteSample(streamIndex, sample); d3d.HRESULT(hr).Failed() {
			sample.Release()
			return fmt.Errorf("WriteSample: %w", d3d.HRESULT(hr))
		}
		sample.Release()
		frameCount++
	}

	if hr := sinkWriter.Finalize(); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("Finalize: %w", d3d.HRESULT(hr))
	}

	fmt.Printf("mfcapture: wrote %d frames to %s\n", frameCount, cfg.OutputPath)
	return nil
}
