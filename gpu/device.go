// Package gpu 提供采集与解码共用的 D3D11 / DXGI 设备与设备管理器创建逻辑。
//
// 采集需要 D3D11 设备来做桌面复制；解码需要把同一个设备包装成
// IMFDXGIDeviceManager 交给解码器 MFT，否则 MFT 会退化成纯软件解码。
// 两边要的是同一个东西，所以放在这里，避免 capture 与 decode 互相依赖。
package gpu

import (
	"fmt"
	"structs"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/com"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/dxgi"

	"mfcapture/mf"
)

var (
	modD3D11              = windows.NewLazySystemDLL("d3d11.dll")
	procD3D11CreateDevice = modD3D11.NewProc("D3D11CreateDevice")
)

const (
	d3d11CreateDeviceVideoSupport = 0x800 // D3D11_CREATE_DEVICE_VIDEO_SUPPORT
	d3d11CreateDeviceBGRASupport  = 0x20  // D3D11_CREATE_DEVICE_BGRA_SUPPORT
	d3d11SDKVersion               = 7
	d3dDriverTypeUnknown          = 0
)

// NewVideoCapableDevice 创建带 VIDEO_SUPPORT|BGRA_SUPPORT 标志的 D3D11 设备。
//
// 这两个标志 go-d3d 自己的构造函数没有（它硬编码为 0 且不提供覆盖方式），
// 而 Media Foundation 的硬件编解码要求必须有。
func NewVideoCapableDevice() (*d3d11.ID3D11Device, *d3d11.ID3D11DeviceContext, error) {
	var factory1 *dxgi.IDXGIFactory1
	if err := dxgi.CreateDXGIFactory1(&factory1); err != nil {
		return nil, nil, fmt.Errorf("CreateDXGIFactory1: %w", err)
	}
	defer factory1.Release()

	var adapter1 *dxgi.IDXGIAdapter1
	var desc dxgi.DXGI_ADAPTER_DESC1

	ai := uint32(0)
	for {
		hr := factory1.EnumAdapters1(ai, &adapter1)
		if d3d.HRESULT(hr).Failed() {
			break
		}
		ai++

		hr = int32(adapter1.GetDesc1(&desc))
		if d3d.HRESULT(hr).Failed() {
			adapter1.Release()
			adapter1 = nil
			continue
		}

		if (desc.Flags & dxgi.DXGI_ADAPTER_FLAG_SOFTWARE) == 0 {
			break
		}
		adapter1.Release()
		adapter1 = nil
	}

	if adapter1 == nil {
		hr := factory1.EnumAdapters1(0, &adapter1)
		if d3d.HRESULT(hr).Failed() {
			return nil, nil, fmt.Errorf("no suitable adapter found")
		}
	}
	if adapter1 == nil {
		return nil, nil, fmt.Errorf("no suitable adapter found")
	}
	defer adapter1.Release()

	fflags := [...]uint32{
		0xc100, // D3D_FEATURE_LEVEL_12_1
		0xc000, // D3D_FEATURE_LEVEL_12_0
		0xb100, // D3D_FEATURE_LEVEL_11_1
		0xb000, // D3D_FEATURE_LEVEL_11_0
		0xa100, // D3D_FEATURE_LEVEL_10_1
		0xa000, // D3D_FEATURE_LEVEL_10_0
	}
	featureLevel := 0x9100
	flags := uint32(d3d11CreateDeviceVideoSupport | d3d11CreateDeviceBGRASupport)

	var device *d3d11.ID3D11Device
	var deviceCtx *d3d11.ID3D11DeviceContext

	ret, _, _ := syscall.SyscallN(
		procD3D11CreateDevice.Addr(),
		uintptr(unsafe.Pointer(adapter1)),
		uintptr(d3dDriverTypeUnknown),
		uintptr(0),
		uintptr(flags),
		uintptr(unsafe.Pointer(&fflags[0])),
		uintptr(len(fflags)),
		uintptr(d3d11SDKVersion),
		uintptr(unsafe.Pointer(&device)),
		uintptr(unsafe.Pointer(&featureLevel)),
		uintptr(unsafe.Pointer(&deviceCtx)),
	)
	if ret != 0 {
		return nil, nil, fmt.Errorf("D3D11CreateDevice: %w", d3d.HRESULT(ret))
	}
	if device == nil || deviceCtx == nil {
		return nil, nil, fmt.Errorf("D3D11CreateDevice returned no device")
	}
	return device, deviceCtx, nil
}

// iD3D10MultithreadVtbl: go-d3d 没有绑定 ID3D10Multithread，GUID 与 vtable 顺序
// 已对照 mingw-w64 的 d3d10.h 核对过。
type iD3D10MultithreadVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	Enter                   uintptr
	Leave                   uintptr
	SetMultithreadProtected uintptr
	GetMultithreadProtected uintptr
}

type iD3D10Multithread struct {
	_    structs.HostLayout
	vtbl *iD3D10MultithreadVtbl
}

func (obj *iD3D10Multithread) SetMultithreadProtected(protect bool) {
	var p uintptr
	if protect {
		p = 1
	}
	syscall.SyscallN(obj.vtbl.SetMultithreadProtected, uintptr(unsafe.Pointer(obj)), p)
}

func (obj *iD3D10Multithread) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

var iidID3D10Multithread, _ = windows.GUIDFromString("{9b7e4e00-342c-4106-a19f-4f2704f689f0}")

// EnableMultithreadProtection 在 D3D11 设备会被采集线程与 MF 内部工作线程共享时必须调用一次。
func EnableMultithreadProtection(device *d3d11.ID3D11Device) error {
	var mt *iD3D10Multithread
	hr := device.QueryInterface(iidID3D10Multithread, &mt)
	if d3d.HRESULT(hr).Failed() {
		return fmt.Errorf("QueryInterface(ID3D10Multithread): %w", d3d.HRESULT(hr))
	}
	defer mt.Release()
	mt.SetMultithreadProtected(true)
	return nil
}

// NewDXGIDeviceManager 把 D3D11 设备包装成 Media Foundation 的设备管理器，
// 交给编解码器 MFT 启用硬件加速（DXVA）。
func NewDXGIDeviceManager(device *d3d11.ID3D11Device) (*mf.IMFDXGIDeviceManager, uint32, error) {
	var resetToken uint32
	var mgr *mf.IMFDXGIDeviceManager
	if hr := mf.MFCreateDXGIDeviceManager(&resetToken, &mgr); d3d.HRESULT(hr).Failed() {
		return nil, 0, fmt.Errorf("MFCreateDXGIDeviceManager: %w", d3d.HRESULT(hr))
	}
	if hr := mgr.ResetDevice(device, resetToken); d3d.HRESULT(hr).Failed() {
		mgr.Release()
		return nil, 0, fmt.Errorf("IMFDXGIDeviceManager.ResetDevice: %w", d3d.HRESULT(hr))
	}
	return mgr, resetToken, nil
}
