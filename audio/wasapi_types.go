//go:build windows

package audio

import (
	"structs"
	"syscall"
	"unsafe"

	"github.com/kirides/go-d3d/com"
	"golang.org/x/sys/windows"
)

// 本文件只放 WASAPI 的 ABI 定义：常量、GUID、WAVEFORMATEX(EXTENSIBLE) 和
// COM vtable。方法顺序按 audioclient.h / mmdeviceapi.h 逐个核对过，注释里的
// 数字是 vtable 索引（IUnknown 占 0~2）。

var (
	modOle32    = windows.NewLazySystemDLL("ole32.dll")
	modKernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procCoCreateInstance = modOle32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = modOle32.NewProc("CoTaskMemFree")

	procQueryPerformanceCounter   = modKernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = modKernel32.NewProc("QueryPerformanceFrequency")
)

const (
	clsctxAll = 0x17

	// EDataFlow / ERole：取默认渲染端点做 loopback。
	eRender  = 0
	eConsole = 0

	audioShareModeShared = 0

	streamFlagLoopback       = 0x00020000
	streamFlagEventCallback  = 0x00040000
	streamFlagAutoConvertPCM = 0x80000000
	streamFlagSRCDefault     = 0x08000000

	waveFormatTagPCM        = 0x0001
	waveFormatTagIEEEFloat  = 0x0003
	waveFormatTagExtensible = 0xFFFE

	speakerFrontLeft   = 0x1
	speakerFrontRight  = 0x2
	speakerFrontCenter = 0x4

	bufferFlagDataDiscontinuity = 0x1
	bufferFlagSilent            = 0x2
	bufferFlagTimestampError    = 0x4

	// waitTimeout 是 WaitForSingleObject 的 WAIT_TIMEOUT；x/sys/windows
	// 只定义了 WAIT_OBJECT_0 / WAIT_FAILED。
	waitTimeout = 258

	// HRESULT 值来自 audioclient.h 的 AUDCLNT_ERR 宏。
	audioClientBufferEmpty          = 0x08890001 // AUDCLNT_S_BUFFER_EMPTY
	audioClientDeviceInvalidated    = 0x88890004
	audioClientEndpointCreateFail   = 0x8889000F
	audioClientServiceNotRunning    = 0x88890010
	audioClientResourcesInvalidated = 0x88890026
)

var (
	clsidMMDeviceEnumerator = windows.GUID{
		Data1: 0xBCDE0395, Data2: 0xE52F, Data3: 0x467C,
		Data4: [8]byte{0x8E, 0x3D, 0xC4, 0x57, 0x92, 0x91, 0x69, 0x2E},
	}
	iidMMDeviceEnumerator = windows.GUID{
		Data1: 0xA95664D2, Data2: 0x9614, Data3: 0x4F35,
		Data4: [8]byte{0xA7, 0x46, 0xDE, 0x8D, 0xB6, 0x36, 0x17, 0xE6},
	}
	iidMMDevice = windows.GUID{
		Data1: 0xD666063F, Data2: 0x1587, Data3: 0x4E43,
		Data4: [8]byte{0x81, 0xF1, 0xB9, 0x48, 0xE8, 0x07, 0x36, 0x3F},
	}
	iidAudioClient = windows.GUID{
		Data1: 0x1CB9AD4C, Data2: 0xDBFA, Data3: 0x4C32,
		Data4: [8]byte{0xB1, 0x78, 0xC2, 0xF5, 0x68, 0xA7, 0x03, 0xB2},
	}
	iidAudioCaptureClient = windows.GUID{
		Data1: 0xC8ADBD64, Data2: 0xE71E, Data3: 0x48A0,
		Data4: [8]byte{0xA4, 0xDE, 0x18, 0x5C, 0x39, 0x5C, 0xD3, 0x17},
	}

	// KSDATAFORMAT_SUBTYPE_*：WAVEFORMATEXTENSIBLE.SubFormat 的取值。
	ksDataFormatSubtypeIEEEFloat = windows.GUID{
		Data1: 0x00000003, Data2: 0x0000, Data3: 0x0010,
		Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71},
	}
	ksDataFormatSubtypePCM = windows.GUID{
		Data1: 0x00000001, Data2: 0x0000, Data3: 0x0010,
		Data4: [8]byte{0x80, 0x00, 0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71},
	}
)

// waveFormatEx 对应 WAVEFORMATEX。Go 会因为 4 字节对齐在末尾补 2 字节，
// 所以 sizeof 是 20 而不是 18——读取 COM 返回的单块内存时字段偏移仍然正确，
// 但绝不能拿它做数组元素。构造扩展格式请用 waveFormatExtensible。
type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CBSize         uint16
}

// waveFormatExtensible 对应 WAVEFORMATEXTENSIBLE，字段按顺序平铺而不是嵌入
// waveFormatEx：嵌入会让 Go 的对齐填充把扩展字段推到偏移 20，而 Windows
// 期望的偏移是 18。平铺后 sizeof 正好 40，布局与 SDK 一致（有单测断言）。
type waveFormatExtensible struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	CBSize         uint16
	// Samples 与 wValidBitsPerSample 共用同一位置。
	Samples     uint16       // offset 18
	ChannelMask uint32       // offset 20
	SubFormat   windows.GUID // offset 24
}

// IMMDeviceEnumerator: IUnknown(0~2) + 5 methods.
type iMMDeviceEnumeratorVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	EnumAudioEndpoints                     uintptr // 3
	GetDefaultAudioEndpoint                uintptr // 4
	GetDevice                              uintptr // 5
	RegisterEndpointNotificationCallback   uintptr // 6
	UnregisterEndpointNotificationCallback uintptr // 7
}

type iMMDeviceEnumerator struct {
	_    structs.HostLayout
	vtbl *iMMDeviceEnumeratorVtbl
}

func (o *iMMDeviceEnumerator) Release() int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Release, uintptr(unsafe.Pointer(o)))
	return int32(ret)
}

func (o *iMMDeviceEnumerator) GetDefaultAudioEndpoint(dataFlow, role uint32) (*iMMDevice, int32) {
	var dev *iMMDevice
	ret, _, _ := syscall.SyscallN(o.vtbl.GetDefaultAudioEndpoint,
		uintptr(unsafe.Pointer(o)),
		uintptr(dataFlow),
		uintptr(role),
		uintptr(unsafe.Pointer(&dev)),
	)
	return dev, int32(ret)
}

// IMMDevice: IUnknown(0~2) + 4 methods.
type iMMDeviceVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	Activate          uintptr // 3
	OpenPropertyStore uintptr // 4
	GetID             uintptr // 5
	GetState          uintptr // 6
}

type iMMDevice struct {
	_    structs.HostLayout
	vtbl *iMMDeviceVtbl
}

func (o *iMMDevice) Release() int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Release, uintptr(unsafe.Pointer(o)))
	return int32(ret)
}

func (o *iMMDevice) Activate(riid *windows.GUID, clsCtx uint32, activationParams unsafe.Pointer, ppv *unsafe.Pointer) int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Activate,
		uintptr(unsafe.Pointer(o)),
		uintptr(unsafe.Pointer(riid)),
		uintptr(clsCtx),
		uintptr(activationParams),
		uintptr(unsafe.Pointer(ppv)),
	)
	return int32(ret)
}

// IAudioClient: IUnknown(0~2) + 12 methods. format 可以是 WAVEFORMATEX 或
// WAVEFORMATEXTENSIBLE，所以用 unsafe.Pointer 而不是具体类型。
type iAudioClientVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	Initialize        uintptr // 3
	GetBufferSize     uintptr // 4
	GetStreamLatency  uintptr // 5
	GetCurrentPadding uintptr // 6
	IsFormatSupported uintptr // 7
	GetMixFormat      uintptr // 8
	GetDevicePeriod   uintptr // 9
	Start             uintptr // 10
	Stop              uintptr // 11
	Reset             uintptr // 12
	SetEventHandle    uintptr // 13
	GetService        uintptr // 14
}

type iAudioClient struct {
	_    structs.HostLayout
	vtbl *iAudioClientVtbl
}

func (o *iAudioClient) Release() int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Release, uintptr(unsafe.Pointer(o)))
	return int32(ret)
}

func (o *iAudioClient) Initialize(shareMode, flags uint32, bufferDuration, periodicity int64, format unsafe.Pointer, sessionGuid *windows.GUID) int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Initialize,
		uintptr(unsafe.Pointer(o)),
		uintptr(shareMode),
		uintptr(flags),
		uintptr(bufferDuration),
		uintptr(periodicity),
		uintptr(format),
		uintptr(unsafe.Pointer(sessionGuid)),
	)
	return int32(ret)
}

func (o *iAudioClient) GetBufferSize() (uint32, int32) {
	var frames uint32
	ret, _, _ := syscall.SyscallN(o.vtbl.GetBufferSize,
		uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&frames)))
	return frames, int32(ret)
}

func (o *iAudioClient) GetMixFormat() (unsafe.Pointer, int32) {
	var format unsafe.Pointer
	ret, _, _ := syscall.SyscallN(o.vtbl.GetMixFormat,
		uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&format)))
	return format, int32(ret)
}

func (o *iAudioClient) SetEventHandle(event windows.Handle) int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.SetEventHandle,
		uintptr(unsafe.Pointer(o)), uintptr(event))
	return int32(ret)
}

func (o *iAudioClient) Start() int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Start, uintptr(unsafe.Pointer(o)))
	return int32(ret)
}

func (o *iAudioClient) Stop() int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Stop, uintptr(unsafe.Pointer(o)))
	return int32(ret)
}

func (o *iAudioClient) GetService(iid *windows.GUID) (unsafe.Pointer, int32) {
	var p unsafe.Pointer
	ret, _, _ := syscall.SyscallN(o.vtbl.GetService,
		uintptr(unsafe.Pointer(o)),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&p)),
	)
	return p, int32(ret)
}

// IAudioCaptureClient: IUnknown(0~2) + 3 methods.
type iAudioCaptureClientVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	GetBuffer         uintptr // 3
	ReleaseBuffer     uintptr // 4
	GetNextPacketSize uintptr // 5
}

type iAudioCaptureClient struct {
	_    structs.HostLayout
	vtbl *iAudioCaptureClientVtbl
}

func (o *iAudioCaptureClient) Release() int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.Release, uintptr(unsafe.Pointer(o)))
	return int32(ret)
}

// GetBuffer 返回的 data 在 ReleaseBuffer 之前有效；flags 含 SILENT 时 data
// 不可读。qpc 是首采样的性能计数器时刻，单位 100 ns（WASAPI 已换算过，
// 不是原始 QPC 计数）。
func (o *iAudioCaptureClient) GetBuffer() (data unsafe.Pointer, frames, flags uint32, qpc uint64, hr int32) {
	var devicePos uint64 // 需要传指针占位，但内容不用
	ret, _, _ := syscall.SyscallN(o.vtbl.GetBuffer,
		uintptr(unsafe.Pointer(o)),
		uintptr(unsafe.Pointer(&data)),
		uintptr(unsafe.Pointer(&frames)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&devicePos)),
		uintptr(unsafe.Pointer(&qpc)),
	)
	return data, frames, flags, qpc, int32(ret)
}

func (o *iAudioCaptureClient) ReleaseBuffer(frames uint32) int32 {
	ret, _, _ := syscall.SyscallN(o.vtbl.ReleaseBuffer,
		uintptr(unsafe.Pointer(o)), uintptr(frames))
	return int32(ret)
}

func (o *iAudioCaptureClient) GetNextPacketSize() (uint32, int32) {
	var frames uint32
	ret, _, _ := syscall.SyscallN(o.vtbl.GetNextPacketSize,
		uintptr(unsafe.Pointer(o)), uintptr(unsafe.Pointer(&frames)))
	return frames, int32(ret)
}

func coCreateEnumerator() (*iMMDeviceEnumerator, int32) {
	var p unsafe.Pointer
	ret, _, _ := syscall.SyscallN(procCoCreateInstance.Addr(),
		uintptr(unsafe.Pointer(&clsidMMDeviceEnumerator)),
		0,
		uintptr(clsctxAll),
		uintptr(unsafe.Pointer(&iidMMDeviceEnumerator)),
		uintptr(unsafe.Pointer(&p)),
	)
	return (*iMMDeviceEnumerator)(p), int32(ret)
}

func coTaskMemFree(p unsafe.Pointer) {
	if p != nil {
		syscall.SyscallN(procCoTaskMemFree.Addr(), uintptr(p))
	}
}

// queryQPC100ns 把当前 QueryPerformanceCounter 换算成 100 ns 单位，用来
// 计算首包在读取时刻之前多久就被采集到了。
func queryQPC100ns() (uint64, bool) {
	var counter, freq int64
	if ret, _, _ := syscall.SyscallN(procQueryPerformanceCounter.Addr(),
		uintptr(unsafe.Pointer(&counter))); ret == 0 {
		return 0, false
	}
	if ret, _, _ := syscall.SyscallN(procQueryPerformanceFrequency.Addr(),
		uintptr(unsafe.Pointer(&freq))); ret == 0 || freq <= 0 {
		return 0, false
	}
	return uint64(counter) * 10_000_000 / uint64(freq), true
}
