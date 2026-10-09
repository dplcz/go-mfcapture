package mf

import (
	"structs"
	"syscall"
	"unsafe"

	"github.com/kirides/go-d3d/com"
	"golang.org/x/sys/windows"
)

var IID_IMFDXGIBuffer, _ = windows.GUIDFromString("{e7174cfa-1c9e-48b1-8866-626226bfc258}")

type IMFDXGIBufferVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	GetResource         uintptr
	GetSubresourceIndex uintptr
	GetUnknownCount     uintptr
	GetUnknown          uintptr
}

type IMFDXGIBuffer struct {
	_    structs.HostLayout
	vtbl *IMFDXGIBufferVtbl
}

func (obj *IMFDXGIBuffer) GetResource(riid windows.GUID, ppvObject *unsafe.Pointer) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetResource,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&riid)),
		uintptr(unsafe.Pointer(ppvObject)),
	)
	return int32(ret)
}

func (obj *IMFDXGIBuffer) GetSubresourceIndex(puSubresource *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetSubresourceIndex,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(puSubresource)),
	)
	return int32(ret)
}

func (obj *IMFDXGIBuffer) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}
