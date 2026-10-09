package mf

import (
	"encoding/binary"
	"structs"
	"syscall"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/com"
	"golang.org/x/sys/windows"
)

// ICodecAPIVtbl order verified against strmif.h / codecapi.h.
// IUnknown (3) + 7 own methods.
type ICodecAPIVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	IsSupported        uintptr // 3
	IsModifiable       uintptr // 4
	GetParameterRange  uintptr // 5
	GetParameterValues uintptr // 6
	GetDefaultValue    uintptr // 7
	GetValue           uintptr // 8
	SetValue           uintptr // 9
}

type ICodecAPI struct {
	_    structs.HostLayout
	vtbl *ICodecAPIVtbl
}

func (obj *ICodecAPI) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *ICodecAPI) IsSupported(api *windows.GUID) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.IsSupported,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(api)),
	)
	return int32(ret)
}

func (obj *ICodecAPI) GetValue(api *windows.GUID, value *VARIANT) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetValue,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(api)),
		uintptr(unsafe.Pointer(value)),
	)
	return int32(ret)
}

func (obj *ICodecAPI) SetValue(api *windows.GUID, value *VARIANT) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetValue,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(api)),
		uintptr(unsafe.Pointer(value)),
	)
	return int32(ret)
}

// SetUINT32 is a convenience wrapper around SetValue for VT_UI4 properties.
func (obj *ICodecAPI) SetUINT32(api windows.GUID, value uint32) error {
	v := NewVariantUINT32(value)
	hr := obj.SetValue(&api, &v)
	if d3d.HRESULT(hr).Failed() {
		return d3d.HRESULT(hr)
	}
	return nil
}

// SetBool is a convenience wrapper around SetValue for VT_BOOL properties.
func (obj *ICodecAPI) SetBool(api windows.GUID, value bool) error {
	v := NewVariantBool(value)
	hr := obj.SetValue(&api, &v)
	if d3d.HRESULT(hr).Failed() {
		return d3d.HRESULT(hr)
	}
	return nil
}

// VARIANT is a minimal representation of the Windows VARIANT/PROPVARIANT type,
// sized to match the real 16-byte layout. Only VT_UI4 and VT_BOOL are supported.
type VARIANT struct {
	_   structs.HostLayout
	Vt  uint16
	_r1 uint16
	_r2 uint16
	_r3 uint16
	Val [8]byte
}

const (
	VT_BOOL = 11
	VT_UI4  = 19
)

func NewVariantUINT32(v uint32) VARIANT {
	var va VARIANT
	va.Vt = VT_UI4
	binary.LittleEndian.PutUint32(va.Val[:4], v)
	return va
}

func NewVariantBool(v bool) VARIANT {
	var va VARIANT
	va.Vt = VT_BOOL
	if v {
		binary.LittleEndian.PutUint16(va.Val[:2], 0xFFFF) // VARIANT_TRUE
	}
	return va
}
