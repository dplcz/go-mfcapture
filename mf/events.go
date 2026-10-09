package mf

import (
	"structs"
	"syscall"
	"unsafe"

	"github.com/kirides/go-d3d/com"
	"golang.org/x/sys/windows"
)

// IMFMediaEventGeneratorVtbl: IUnknown (3) + 4 own methods.
type IMFMediaEventGeneratorVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	GetEvent      uintptr // 3: (DWORD dwFlags, IMFMediaEvent **ppEvent)
	BeginGetEvent uintptr // 4
	EndGetEvent   uintptr // 5
	QueueEvent    uintptr // 6
}

type IMFMediaEventGenerator struct {
	_    structs.HostLayout
	vtbl *IMFMediaEventGeneratorVtbl
}

const (
	MF_EVENT_FLAG_NO_WAIT = 0x1
)

// GetEvent retrieves the next event from the event queue. Pass 0 to block,
// or MF_EVENT_FLAG_NO_WAIT to return immediately (MF_E_NO_EVENTS_AVAILABLE if empty).
func (obj *IMFMediaEventGenerator) GetEvent(dwFlags uint32, ppEvent **IMFMediaEvent) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetEvent,
		uintptr(unsafe.Pointer(obj)),
		uintptr(dwFlags),
		uintptr(unsafe.Pointer(ppEvent)),
	)
	return int32(ret)
}

func (obj *IMFMediaEventGenerator) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

// IMFMediaEventVtbl: IMFAttributes (33 slots) + 4 own methods.
type IMFMediaEventVtbl struct {
	_ structs.HostLayout
	IMFAttributesVtbl

	GetType         uintptr // 33
	GetExtendedType uintptr // 34
	GetStatus       uintptr // 35
	GetValue        uintptr // 36
}

type IMFMediaEvent struct {
	_    structs.HostLayout
	vtbl *IMFMediaEventVtbl
}

func (obj *IMFMediaEvent) GetType(pmet *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetType,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(pmet)),
	)
	return int32(ret)
}

func (obj *IMFMediaEvent) GetStatus(phrStatus *int32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetStatus,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(phrStatus)),
	)
	return int32(ret)
}

func (obj *IMFMediaEvent) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

// Media event types for async MFT.
const (
	METransformNeedInput     = 601
	METransformHaveOutput    = 602
	METransformDrainComplete = 603
	METransformMarker        = 604
)

// HRESULT for "no events available" when using MF_EVENT_FLAG_NO_WAIT.
const MF_E_NO_EVENTS_AVAILABLE = 0xC00D3E80

var IID_IMFMediaEventGenerator, _ = windows.GUIDFromString("{2cd0bd52-bcd5-4b89-b62c-eadc0c031e7d}")

// MFT_OUTPUT_STREAM_INFO flag: the MFT allocates its own output samples.
const MFT_OUTPUT_STREAM_PROVIDES_SAMPLES = 0x100
