package capture

import (
	"errors"
	"fmt"
	"time"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/dxgi"
)

// ErrTimeout is returned by AcquireFrame when AcquireNextFrame times out without a new frame.
var ErrTimeout = errors.New("mfcapture/capture: no new frame within timeout")

// ErrAccessLostRecovered is returned after the duplication session was automatically
// recreated following a DXGI_ERROR_ACCESS_LOST (lock screen, mode change, etc.).
// Callers should re-query Width()/Height() (resolution may have changed) and force
// an IDR frame from the encoder.
var ErrAccessLostRecovered = errors.New("mfcapture/capture: duplication recreated after ACCESS_LOST")

// Duplicator wraps DXGI Desktop Duplication, implementing the Capturer interface.
// It copies each new desktop frame GPU→GPU into a caller-owned texture, extracts
// cursor info, and automatically recovers from DXGI_ERROR_ACCESS_LOST.
type Duplicator struct {
	device            *d3d11.ID3D11Device
	deviceCtx         *d3d11.ID3D11DeviceContext
	outputDuplication *dxgi.IDXGIOutputDuplication
	dxgiOutput        *dxgi.IDXGIOutput5
	acquiredFrame     bool
	outputIndex       uint

	privTex *d3d11.ID3D11Texture2D
	start   time.Time

	width, height uint32
}

func (d *Duplicator) Width() uint32  { return d.width }
func (d *Duplicator) Height() uint32 { return d.height }

// NewDuplicator creates a Duplicator for the given monitor index, using the provided
// D3D11 device. The device must have been created with VIDEO_SUPPORT | BGRA_SUPPORT flags.
func NewDuplicator(device *d3d11.ID3D11Device, deviceCtx *d3d11.ID3D11DeviceContext, output uint) (*Duplicator, error) {
	dup := &Duplicator{
		device:      device,
		deviceCtx:   deviceCtx,
		outputIndex: output,
		start:       time.Now(),
	}
	if err := dup.initDuplication(); err != nil {
		return nil, err
	}
	if err := dup.initPrivateTexture(); err != nil {
		dup.releaseDuplication()
		return nil, err
	}
	return dup, nil
}

func (d *Duplicator) initDuplication() error {
	var dxgiDevice1 *dxgi.IDXGIDevice1
	hr := d.device.QueryInterface(dxgi.IID_IDXGIDevice1, &dxgiDevice1)
	if d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("QueryInterface(IDXGIDevice1): %w", d3d.HRESULT(hr))
	}
	defer dxgiDevice1.Release()

	var pdxgiAdapter unsafe.Pointer
	hr = dxgiDevice1.GetParent(dxgi.IID_IDXGIAdapter1, &pdxgiAdapter)
	if d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("GetParent(IDXGIAdapter1): %w", d3d.HRESULT(hr))
	}
	dxgiAdapter := (*dxgi.IDXGIAdapter1)(pdxgiAdapter)
	defer dxgiAdapter.Release()

	var dxgiOutput *dxgi.IDXGIOutput
	hr = int32(dxgiAdapter.EnumOutputs(uint32(d.outputIndex), &dxgiOutput))
	if d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("EnumOutputs: %w", d3d.HRESULT(hr))
	}
	defer dxgiOutput.Release()

	var dxgiOutput5 *dxgi.IDXGIOutput5
	hr = dxgiOutput.QueryInterface(dxgi.IID_IDXGIOutput5, &dxgiOutput5)
	if d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("QueryInterface(IDXGIOutput5): %w", d3d.HRESULT(hr))
	}

	var dup *dxgi.IDXGIOutputDuplication
	hr = dxgiOutput5.DuplicateOutput1(dxgiDevice1, 0, []dxgi.DXGI_FORMAT{
		dxgi.DXGI_FORMAT_B8G8R8A8_UNORM,
	}, &dup)
	if d3d.HRESULT(hr).Failed() {
		var dxgiOutput1 *dxgi.IDXGIOutput1
		hr2 := dxgiOutput.QueryInterface(dxgi.IID_IDXGIOutput1, &dxgiOutput1)
		if d3d.HRESULT(hr2).Failed() {
			dxgiOutput5.Release()
			return fmt.Errorf("QueryInterface(IDXGIOutput1): %w", d3d.HRESULT(hr2))
		}
		defer dxgiOutput1.Release()
		hr = dxgiOutput1.DuplicateOutput(dxgiDevice1, &dup)
		if d3d.HRESULT(hr).Failed() {
			dxgiOutput5.Release()
			return fmt.Errorf("DuplicateOutput: %w", d3d.HRESULT(hr))
		}
	}

	var outDesc dxgi.DXGI_OUTPUT_DESC
	if hr := dxgiOutput5.GetDesc(&outDesc); d3d.HRESULT(hr).Failed() {
		dup.Release()
		dxgiOutput5.Release()
		return fmt.Errorf("GetDesc: %w", d3d.HRESULT(hr))
	}

	d.outputDuplication = dup
	d.dxgiOutput = dxgiOutput5
	d.width = uint32(outDesc.DesktopCoordinates.Right - outDesc.DesktopCoordinates.Left)
	d.height = uint32(outDesc.DesktopCoordinates.Bottom - outDesc.DesktopCoordinates.Top)
	return nil
}

func (d *Duplicator) initPrivateTexture() error {
	privTexDesc := d3d11.D3D11_TEXTURE2D_DESC{
		Width:      d.width,
		Height:     d.height,
		MipLevels:  1,
		ArraySize:  1,
		Format:     uint32(dxgi.DXGI_FORMAT_B8G8R8A8_UNORM),
		SampleDesc: dxgi.DXGI_SAMPLE_DESC{Count: 1},
		Usage:      d3d11.D3D11_USAGE_DEFAULT,
		BindFlags:  d3d11BindRenderTarget | d3d11BindShaderResource,
	}
	var tex *d3d11.ID3D11Texture2D
	if hr := d.device.CreateTexture2D(&privTexDesc, &tex); d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("CreateTexture2D: %w", d3d.HRESULT(hr))
	}
	if d.privTex != nil {
		d.privTex.Release()
	}
	d.privTex = tex
	return nil
}

const (
	d3d11BindRenderTarget   = 0x20
	d3d11BindShaderResource = 0x8
)

func (d *Duplicator) releaseDuplication() {
	d.releaseFrame()
	if d.outputDuplication != nil {
		d.outputDuplication.Release()
		d.outputDuplication = nil
	}
	if d.dxgiOutput != nil {
		d.dxgiOutput.Release()
		d.dxgiOutput = nil
	}
}

func (d *Duplicator) releaseFrame() {
	if d.acquiredFrame {
		d.outputDuplication.ReleaseFrame()
		d.acquiredFrame = false
	}
}

func (d *Duplicator) recreate() error {
	oldW, oldH := d.width, d.height
	d.releaseDuplication()
	if err := d.initDuplication(); err != nil {
		return err
	}
	if d.width != oldW || d.height != oldH {
		if err := d.initPrivateTexture(); err != nil {
			d.releaseDuplication()
			return err
		}
	}
	return nil
}

// AcquireFrame implements Capturer. It waits up to timeoutMs for a new desktop frame,
// copies the texture GPU→GPU, and extracts cursor info.
func (d *Duplicator) AcquireFrame(timeoutMs uint32) (*CapturedFrame, error) {
	var frameInfo dxgi.DXGI_OUTDUPL_FRAME_INFO
	var desktop *dxgi.IDXGIResource

	d.releaseFrame()
	hrF := d.outputDuplication.AcquireNextFrame(timeoutMs, &frameInfo, &desktop)
	hr := d3d.HRESULT(hrF)

	if errors.Is(hr, d3d.DXGI_ERROR_ACCESS_LOST) {
		if err := d.recreate(); err != nil {
			return nil, fmt.Errorf("recreate after ACCESS_LOST: %w", err)
		}
		return nil, ErrAccessLostRecovered
	}

	if hr.Failed() {
		if errors.Is(hr, d3d.DXGI_ERROR_WAIT_TIMEOUT) {
			return nil, ErrTimeout
		}
		return nil, fmt.Errorf("AcquireNextFrame: %w", hr)
	}
	d.acquiredFrame = true
	defer d.releaseFrame()
	defer desktop.Release()

	if frameInfo.AccumulatedFrames == 0 {
		return nil, ErrTimeout
	}

	var desktop2d *d3d11.ID3D11Texture2D
	hr2 := d3d.HRESULT(desktop.QueryInterface(d3d11.IID_ID3D11Texture2D, &desktop2d))
	if hr2.Failed() {
		return nil, fmt.Errorf("QueryInterface(ID3D11Texture2D): %w", hr2)
	}
	defer desktop2d.Release()

	ret := d.deviceCtx.CopyResource2D(d.privTex, desktop2d)
	if d3d.HRESULT(ret).Failed() {
		return nil, fmt.Errorf("CopyResource: %w", d3d.HRESULT(ret))
	}

	frame := &CapturedFrame{
		Texture:   d.privTex,
		Timestamp: time.Since(d.start),
	}

	return frame, nil
}

func (d *Duplicator) Release() {
	d.releaseDuplication()
	if d.privTex != nil {
		d.privTex.Release()
		d.privTex = nil
	}
}
