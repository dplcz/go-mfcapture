package mf

import (
	"structs"
	"syscall"
	"unsafe"

	"github.com/kirides/go-d3d/com"
)

// IMFSinkWriterVtbl order verified against mingw-w64 mfreadwrite.h.
type IMFSinkWriterVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	AddStream           uintptr
	SetInputMediaType   uintptr
	BeginWriting        uintptr
	WriteSample         uintptr
	SendStreamTick      uintptr
	PlaceMarker         uintptr
	NotifyEndOfSegment  uintptr
	Flush               uintptr
	Finalize            uintptr
	GetServiceForStream uintptr
	GetStatistics       uintptr
}

type IMFSinkWriter struct {
	_    structs.HostLayout
	vtbl *IMFSinkWriterVtbl
}

func (obj *IMFSinkWriter) AddStream(mediaType *IMFMediaType, streamIndex *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.AddStream,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(mediaType)),
		uintptr(unsafe.Pointer(streamIndex)),
	)
	return int32(ret)
}

func (obj *IMFSinkWriter) SetInputMediaType(streamIndex uint32, mediaType *IMFMediaType, encodingParams *IMFAttributes) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetInputMediaType,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamIndex),
		uintptr(unsafe.Pointer(mediaType)),
		uintptr(unsafe.Pointer(encodingParams)),
	)
	return int32(ret)
}

func (obj *IMFSinkWriter) BeginWriting() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.BeginWriting, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *IMFSinkWriter) WriteSample(streamIndex uint32, sample *IMFSample) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.WriteSample,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamIndex),
		uintptr(unsafe.Pointer(sample)),
	)
	return int32(ret)
}

func (obj *IMFSinkWriter) Finalize() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Finalize, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

func (obj *IMFSinkWriter) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}
