package mf

import (
	"structs"
	"syscall"
	"unsafe"

	"github.com/kirides/go-d3d/com"
	"github.com/kirides/go-d3d/d3d11"
)

// IMFDXGIDeviceManagerVtbl: methods are alphabetically ordered in the real vtable, NOT
// declaration order from MSDN prose. Verified against mingw-w64 mfobjects.h/mfidl.h mirror:
// CloseDeviceHandle, GetVideoService, LockDevice, OpenDeviceHandle, ResetDevice, TestDevice, UnlockDevice.
type IMFDXGIDeviceManagerVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	CloseDeviceHandle uintptr
	GetVideoService   uintptr
	LockDevice        uintptr
	OpenDeviceHandle  uintptr
	ResetDevice       uintptr
	TestDevice        uintptr
	UnlockDevice      uintptr
}

type IMFDXGIDeviceManager struct {
	_    structs.HostLayout
	vtbl *IMFDXGIDeviceManagerVtbl
}

func (obj *IMFDXGIDeviceManager) ResetDevice(device *d3d11.ID3D11Device, resetToken uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.ResetDevice,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(device)),
		uintptr(resetToken),
	)
	return int32(ret)
}

func (obj *IMFDXGIDeviceManager) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}
