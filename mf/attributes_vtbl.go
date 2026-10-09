package mf

import (
	"structs"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/kirides/go-d3d/com"
)

// IMFAttributesVtbl mirrors the full real vtable (30 methods after IUnknown) so that the
// fields we actually call (SetUINT32/SetUINT64/SetGUID/SetUnknown) land at the correct
// memory offsets. Unused methods are still declared, in order, as plain placeholders.
type IMFAttributesVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	GetItem            uintptr
	GetItemType        uintptr
	CompareItem        uintptr
	Compare            uintptr
	GetUINT32          uintptr
	GetUINT64          uintptr
	GetDouble          uintptr
	GetGUID            uintptr
	GetStringLength    uintptr
	GetString          uintptr
	GetAllocatedString uintptr
	GetBlobSize        uintptr
	GetBlob            uintptr
	GetAllocatedBlob   uintptr
	GetUnknown         uintptr
	SetItem            uintptr
	DeleteItem         uintptr
	DeleteAllItems     uintptr
	SetUINT32          uintptr
	SetUINT64          uintptr
	SetDouble          uintptr
	SetGUID            uintptr
	SetString          uintptr
	SetBlob            uintptr
	SetUnknown         uintptr
	LockStore          uintptr
	UnlockStore        uintptr
	GetCount           uintptr
	GetItemByIndex     uintptr
	CopyAllItems       uintptr
}

type IMFAttributes struct {
	_    structs.HostLayout
	vtbl *IMFAttributesVtbl
}

func (obj *IMFAttributes) QueryInterface(iid windows.GUID, pp interface{}) int32 {
	return com.ReflectQueryInterface(obj, obj.vtbl.QueryInterface, &iid, pp)
}

// GetUINT32 读取一个 UINT32 属性（例如 MF_SA_D3D11_AWARE）。
func (obj *IMFAttributes) GetUINT32(key windows.GUID, value *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetUINT32,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(value)),
	)
	return int32(ret)
}

func (obj *IMFAttributes) SetUINT32(key windows.GUID, value uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetUINT32,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(value),
	)
	return int32(ret)
}

func (obj *IMFAttributes) SetUINT64(key windows.GUID, value uint64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetUINT64,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(value),
	)
	return int32(ret)
}

func (obj *IMFAttributes) SetGUID(key windows.GUID, value windows.GUID) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetGUID,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(&value)),
	)
	return int32(ret)
}

func (obj *IMFAttributes) SetUnknown(key windows.GUID, value unsafe.Pointer) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetUnknown,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(value),
	)
	return int32(ret)
}

func (obj *IMFAttributes) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}
