package mf

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modMFPlat      = windows.NewLazySystemDLL("mfplat.dll")
	modMFReadWrite = windows.NewLazySystemDLL("mfreadwrite.dll")
	modOle32       = windows.NewLazySystemDLL("ole32.dll")

	procMFStartup                 = modMFPlat.NewProc("MFStartup")
	procMFShutdown                = modMFPlat.NewProc("MFShutdown")
	procMFCreateDXGIDeviceManager = modMFPlat.NewProc("MFCreateDXGIDeviceManager")
	procMFCreateAttributes        = modMFPlat.NewProc("MFCreateAttributes")
	procMFCreateMediaTypeRaw      = modMFPlat.NewProc("MFCreateMediaType")
	procMFCreateSampleRaw         = modMFPlat.NewProc("MFCreateSample")
	procMFCreateDXGISurfaceBuffer = modMFPlat.NewProc("MFCreateDXGISurfaceBuffer")
	procMFCreateMemoryBuffer      = modMFPlat.NewProc("MFCreateMemoryBuffer")

	procMFCreateSinkWriterFromURL = modMFReadWrite.NewProc("MFCreateSinkWriterFromURL")

	procCoInitializeEx = modOle32.NewProc("CoInitializeEx")
	procCoUninitialize = modOle32.NewProc("CoUninitialize")
)

const (
	// MF_VERSION = (MF_SDK_VERSION<<16 | MF_API_VERSION) = (0x0002<<16 | 0x0070)
	MF_VERSION           = 0x00020070
	MFSTARTUP_FULL       = 0
	COINIT_MULTITHREADED = 0x0
)

func MFStartup() int32 {
	ret, _, _ := syscall.SyscallN(procMFStartup.Addr(), uintptr(MF_VERSION), uintptr(MFSTARTUP_FULL))
	return int32(ret)
}

func MFShutdown() int32 {
	ret, _, _ := syscall.SyscallN(procMFShutdown.Addr())
	return int32(ret)
}

func CoInitializeEx() int32 {
	ret, _, _ := syscall.SyscallN(procCoInitializeEx.Addr(), 0, uintptr(COINIT_MULTITHREADED))
	return int32(ret)
}

func CoUninitialize() {
	syscall.SyscallN(procCoUninitialize.Addr())
}

func MFCreateDXGIDeviceManager(resetToken *uint32, ppManager **IMFDXGIDeviceManager) int32 {
	ret, _, _ := syscall.SyscallN(procMFCreateDXGIDeviceManager.Addr(),
		uintptr(unsafe.Pointer(resetToken)),
		uintptr(unsafe.Pointer(ppManager)),
	)
	return int32(ret)
}

func MFCreateAttributes(ppAttributes **IMFAttributes, initialSize uint32) int32 {
	ret, _, _ := syscall.SyscallN(procMFCreateAttributes.Addr(),
		uintptr(unsafe.Pointer(ppAttributes)),
		uintptr(initialSize),
	)
	return int32(ret)
}

func MFCreateMediaType(ppType **IMFMediaType) int32 {
	ret, _, _ := syscall.SyscallN(procMFCreateMediaTypeRaw.Addr(), uintptr(unsafe.Pointer(ppType)))
	return int32(ret)
}

func MFCreateSample(ppSample **IMFSample) int32 {
	ret, _, _ := syscall.SyscallN(procMFCreateSampleRaw.Addr(), uintptr(unsafe.Pointer(ppSample)))
	return int32(ret)
}

func MFCreateDXGISurfaceBuffer(riid windows.GUID, surface unsafe.Pointer, subresourceIndex uint32, bottomUpWhenLinear bool, ppBuffer **IMFMediaBuffer) int32 {
	var bul uintptr
	if bottomUpWhenLinear {
		bul = 1
	}
	ret, _, _ := syscall.SyscallN(procMFCreateDXGISurfaceBuffer.Addr(),
		uintptr(unsafe.Pointer(&riid)),
		uintptr(surface),
		uintptr(subresourceIndex),
		bul,
		uintptr(unsafe.Pointer(ppBuffer)),
	)
	return int32(ret)
}

// MFCreateSinkWriterFromURL always passes a nil byte stream and nil IMFActivate (pByteStream),
// since this PoC only ever writes directly to a file URL/path.
func MFCreateSinkWriterFromURL(outputURL *uint16, attributes *IMFAttributes, ppSinkWriter **IMFSinkWriter) int32 {
	ret, _, _ := syscall.SyscallN(procMFCreateSinkWriterFromURL.Addr(),
		uintptr(unsafe.Pointer(outputURL)),
		0,
		uintptr(unsafe.Pointer(attributes)),
		uintptr(unsafe.Pointer(ppSinkWriter)),
	)
	return int32(ret)
}

func MFCreateMemoryBuffer(cbMaxLength uint32, ppBuffer **IMFMediaBuffer) int32 {
	ret, _, _ := syscall.SyscallN(procMFCreateMemoryBuffer.Addr(),
		uintptr(cbMaxLength),
		uintptr(unsafe.Pointer(ppBuffer)),
	)
	return int32(ret)
}
