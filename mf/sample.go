package mf

import (
	"structs"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/kirides/go-d3d/com"
)

// IMFSampleVtbl = IMFAttributes's 33 slots + 14 of its own.
type IMFSampleVtbl struct {
	_ structs.HostLayout
	IMFAttributesVtbl

	GetSampleFlags            uintptr
	SetSampleFlags            uintptr
	GetSampleTime             uintptr
	SetSampleTime             uintptr
	GetSampleDuration         uintptr
	SetSampleDuration         uintptr
	GetBufferCount            uintptr
	GetBufferByIndex          uintptr
	ConvertToContiguousBuffer uintptr
	AddBuffer                 uintptr
	RemoveBufferByIndex       uintptr
	RemoveAllBuffers          uintptr
	GetTotalLength            uintptr
	CopyToBuffer              uintptr
}

type IMFSample struct {
	_    structs.HostLayout
	vtbl *IMFSampleVtbl
}

func (obj *IMFSample) QueryInterface(iid windows.GUID, pp interface{}) int32 {
	return com.ReflectQueryInterface(obj, obj.vtbl.QueryInterface, &iid, pp)
}

func (obj *IMFSample) SetSampleTime(hnsSampleTime int64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetSampleTime, uintptr(unsafe.Pointer(obj)), uintptr(hnsSampleTime))
	return int32(ret)
}

func (obj *IMFSample) SetSampleDuration(hnsSampleDuration int64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetSampleDuration, uintptr(unsafe.Pointer(obj)), uintptr(hnsSampleDuration))
	return int32(ret)
}

func (obj *IMFSample) AddBuffer(buffer *IMFMediaBuffer) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.AddBuffer,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(buffer)),
	)
	return int32(ret)
}

func (obj *IMFSample) AddRef() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.AddRef, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *IMFSample) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *IMFSample) GetSampleTime(phnsSampleTime *int64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetSampleTime,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(phnsSampleTime)),
	)
	return int32(ret)
}

func (obj *IMFSample) GetSampleDuration(phnsSampleDuration *int64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetSampleDuration,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(phnsSampleDuration)),
	)
	return int32(ret)
}

func (obj *IMFSample) GetBufferCount(pdwBufferCount *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetBufferCount,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(pdwBufferCount)),
	)
	return int32(ret)
}

func (obj *IMFSample) GetBufferByIndex(dwIndex uint32, ppBuffer **IMFMediaBuffer) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetBufferByIndex,
		uintptr(unsafe.Pointer(obj)),
		uintptr(dwIndex),
		uintptr(unsafe.Pointer(ppBuffer)),
	)
	return int32(ret)
}

func (obj *IMFSample) ConvertToContiguousBuffer(ppBuffer **IMFMediaBuffer) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.ConvertToContiguousBuffer,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(ppBuffer)),
	)
	return int32(ret)
}

func (obj *IMFSample) GetUINT32(key windows.GUID, pValue *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetUINT32,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(pValue)),
	)
	return int32(ret)
}

// IMFMediaBufferVtbl: only Release is called directly; MFCreateDXGISurfaceBuffer/IMFSample.AddBuffer
// handle the rest internally, but the full method list is declared to keep slot offsets correct
// in case future code needs Lock/Unlock.
type IMFMediaBufferVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	Lock             uintptr
	Unlock           uintptr
	GetCurrentLength uintptr
	SetCurrentLength uintptr
	GetMaxLength     uintptr
}

type IMFMediaBuffer struct {
	_    structs.HostLayout
	vtbl *IMFMediaBufferVtbl
}

func (obj *IMFMediaBuffer) QueryInterface(iid windows.GUID, pp interface{}) int32 {
	return com.ReflectQueryInterface(obj, obj.vtbl.QueryInterface, &iid, pp)
}

func (obj *IMFMediaBuffer) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *IMFMediaBuffer) Lock(ppbBuffer *unsafe.Pointer, pcbMaxLength, pcbCurrentLength *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Lock,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(ppbBuffer)),
		uintptr(unsafe.Pointer(pcbMaxLength)),
		uintptr(unsafe.Pointer(pcbCurrentLength)),
	)
	return int32(ret)
}

func (obj *IMFMediaBuffer) Unlock() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Unlock, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *IMFMediaBuffer) GetCurrentLength(pcbCurrentLength *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetCurrentLength,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(pcbCurrentLength)),
	)
	return int32(ret)
}

// SetCurrentLength must be called explicitly for DXGI surface buffers: GetCurrentLength()
// defaults to 0 for a 2D buffer (it is not computed from the surface automatically), and
// IMFSinkWriter::WriteSample rejects a sample whose total length is 0 with E_INVALIDARG.
func (obj *IMFMediaBuffer) SetCurrentLength(length uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetCurrentLength, uintptr(unsafe.Pointer(obj)), uintptr(length))
	return int32(ret)
}
