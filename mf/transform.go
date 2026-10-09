package mf

import (
	"fmt"
	"structs"
	"syscall"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/com"
	"golang.org/x/sys/windows"
)

// IMFTransformVtbl order verified against mingw-w64 mftransform.h.
// IUnknown (3) + 23 own methods = 26 total slots.
type IMFTransformVtbl struct {
	_ structs.HostLayout
	com.IUnknownVtbl

	GetStreamLimits           uintptr // 3
	GetStreamCount            uintptr // 4
	GetStreamIDs              uintptr // 5
	GetInputStreamInfo        uintptr // 6
	GetOutputStreamInfo       uintptr // 7
	GetAttributes             uintptr // 8
	GetInputStreamAttributes  uintptr // 9
	GetOutputStreamAttributes uintptr // 10
	DeleteInputStream         uintptr // 11
	AddInputStreams           uintptr // 12
	GetInputAvailableType     uintptr // 13
	GetOutputAvailableType    uintptr // 14
	SetInputType              uintptr // 15
	SetOutputType             uintptr // 16
	GetInputCurrentType       uintptr // 17
	GetOutputCurrentType      uintptr // 18
	GetInputStatus            uintptr // 19
	GetOutputStatus           uintptr // 20
	SetOutputBounds           uintptr // 21
	ProcessEvent              uintptr // 22
	ProcessMessage            uintptr // 23
	ProcessInput              uintptr // 24
	ProcessOutput             uintptr // 25
}

type IMFTransform struct {
	_    structs.HostLayout
	vtbl *IMFTransformVtbl
}

func (obj *IMFTransform) QueryInterface(iid windows.GUID, pp interface{}) int32 {
	return com.ReflectQueryInterface(obj, obj.vtbl.QueryInterface, &iid, pp)
}

func (obj *IMFTransform) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

// GetInputAvailableType 枚举输入流第 typeIndex 个可选媒体类型。
// 枚举结束通常返回 MF_E_NO_MORE_TYPES（0xC00D36B9）或 MF_E_INVALIDSTREAMNUMBER。
// 注意：对编码器而言，必须先 SetOutputType，否则这里返回的类型是残缺的。
func (obj *IMFTransform) GetInputAvailableType(streamID, typeIndex uint32, ppType **IMFMediaType) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetInputAvailableType,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamID),
		uintptr(typeIndex),
		uintptr(unsafe.Pointer(ppType)),
	)
	return int32(ret)
}

// GetOutputAvailableType 枚举输出流第 typeIndex 个可选媒体类型。
// 解码器通常要先 SetInputType，输出类型列表才有意义。
func (obj *IMFTransform) GetOutputAvailableType(streamID, typeIndex uint32, ppType **IMFMediaType) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetOutputAvailableType,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamID),
		uintptr(typeIndex),
		uintptr(unsafe.Pointer(ppType)),
	)
	return int32(ret)
}

func (obj *IMFTransform) GetAttributes(ppAttributes **IMFAttributes) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetAttributes,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(ppAttributes)),
	)
	return int32(ret)
}

func (obj *IMFTransform) GetOutputStreamInfo(streamID uint32, pStreamInfo *MFT_OUTPUT_STREAM_INFO) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetOutputStreamInfo,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamID),
		uintptr(unsafe.Pointer(pStreamInfo)),
	)
	return int32(ret)
}

func (obj *IMFTransform) SetInputType(streamID uint32, pType *IMFMediaType, dwFlags uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetInputType,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamID),
		uintptr(unsafe.Pointer(pType)),
		uintptr(dwFlags),
	)
	return int32(ret)
}

func (obj *IMFTransform) SetOutputType(streamID uint32, pType *IMFMediaType, dwFlags uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetOutputType,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamID),
		uintptr(unsafe.Pointer(pType)),
		uintptr(dwFlags),
	)
	return int32(ret)
}

func (obj *IMFTransform) ProcessMessage(eMessage MFT_MESSAGE_TYPE, ulParam uintptr) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.ProcessMessage,
		uintptr(unsafe.Pointer(obj)),
		uintptr(eMessage),
		ulParam,
	)
	return int32(ret)
}

func (obj *IMFTransform) ProcessInput(streamID uint32, pSample *IMFSample, dwFlags uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.ProcessInput,
		uintptr(unsafe.Pointer(obj)),
		uintptr(streamID),
		uintptr(unsafe.Pointer(pSample)),
		uintptr(dwFlags),
	)
	return int32(ret)
}

func (obj *IMFTransform) ProcessOutput(dwFlags uint32, cOutputBufferCount uint32, pOutputSamples *MFT_OUTPUT_DATA_BUFFER, pdwStatus *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.ProcessOutput,
		uintptr(unsafe.Pointer(obj)),
		uintptr(dwFlags),
		uintptr(cOutputBufferCount),
		uintptr(unsafe.Pointer(pOutputSamples)),
		uintptr(unsafe.Pointer(pdwStatus)),
	)
	return int32(ret)
}

// MFT_MESSAGE_TYPE values used for ProcessMessage.
type MFT_MESSAGE_TYPE uint32

const (
	MFT_MESSAGE_COMMAND_FLUSH          MFT_MESSAGE_TYPE = 0
	MFT_MESSAGE_COMMAND_DRAIN          MFT_MESSAGE_TYPE = 1
	MFT_MESSAGE_SET_D3D_MANAGER        MFT_MESSAGE_TYPE = 2
	MFT_MESSAGE_NOTIFY_BEGIN_STREAMING MFT_MESSAGE_TYPE = 0x10000000
	MFT_MESSAGE_NOTIFY_END_STREAMING   MFT_MESSAGE_TYPE = 0x10000001
	MFT_MESSAGE_NOTIFY_END_OF_STREAM   MFT_MESSAGE_TYPE = 0x10000002
	MFT_MESSAGE_NOTIFY_START_OF_STREAM MFT_MESSAGE_TYPE = 0x10000003
)

// MFT_OUTPUT_STREAM_INFO is returned by GetOutputStreamInfo.
type MFT_OUTPUT_STREAM_INFO struct {
	_           structs.HostLayout
	DwFlags     uint32
	CbSize      uint32
	CbAlignment uint32
}

// MFT_OUTPUT_DATA_BUFFER is used by ProcessOutput.
type MFT_OUTPUT_DATA_BUFFER struct {
	_          structs.HostLayout
	DwStreamID uint32
	PSample    *IMFSample
	DwStatus   uint32
	PEvents    uintptr // IMFCollection*, unused
}

// HRESULT codes for MFT operations.
const (
	MF_E_TRANSFORM_NEED_MORE_INPUT = 0xC00D6D72
	MF_E_TRANSFORM_STREAM_CHANGE   = 0xC00D6D61
	MF_E_INVALIDMEDIATYPE          = 0xC00D36B4
	// MF_E_NOTACCEPTING：同步 MFT 暂时收不下输入，意思是"先把我的输出
	// 排空再来"。这是正常的背压信号，不是失败——排空后重试同一个输入即可。
	MF_E_NOTACCEPTING              = 0xC00D36B5
	MFT_OUTPUT_STATUS_SAMPLE_READY = 0x1
)

// MFT_OUTPUT_DATA_BUFFER flags.
const (
	MFT_OUTPUT_DATA_BUFFER_NO_SAMPLE     = 0x300
	MFT_OUTPUT_DATA_BUFFER_INCOMPLETE    = 0x1000000
	MFT_OUTPUT_DATA_BUFFER_FORMAT_CHANGE = 0x100
)

// IMFActivateVtbl: IMFAttributes (33 slots) + 3 own methods.
type IMFActivateVtbl struct {
	_ structs.HostLayout
	IMFAttributesVtbl

	ActivateObject uintptr
	ShutdownObject uintptr
	DetachObject   uintptr
}

type IMFActivate struct {
	_    structs.HostLayout
	vtbl *IMFActivateVtbl
}

func (obj *IMFActivate) ActivateObject(riid windows.GUID, ppv unsafe.Pointer) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.ActivateObject,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&riid)),
		uintptr(ppv),
	)
	return int32(ret)
}

func (obj *IMFActivate) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

// MFT_REGISTER_TYPE_INFO is used by MFTEnumEx to filter by media type.
type MFT_REGISTER_TYPE_INFO struct {
	_             structs.HostLayout
	GuidMajorType windows.GUID
	GuidSubtype   windows.GUID
}

// MFTEnumEx flags.
const (
	MFT_ENUM_FLAG_SYNCMFT        = 0x00000001
	MFT_ENUM_FLAG_ASYNCMFT       = 0x00000002
	MFT_ENUM_FLAG_HARDWARE       = 0x00000004
	MFT_ENUM_FLAG_FIELDOFUSE     = 0x00000008
	MFT_ENUM_FLAG_LOCALMFT       = 0x00000010
	MFT_ENUM_FLAG_TRANSCODE_ONLY = 0x00000020
	MFT_ENUM_FLAG_SORTANDFILTER  = 0x00000040
	MFT_ENUM_FLAG_ALL            = 0x0000003F
)

var procMFTEnumEx = modMFPlat.NewProc("MFTEnumEx")

// MFTEnumEx enumerates Media Foundation transforms matching the given category and type filters.
// Caller must Release() each returned IMFActivate and free the array with CoTaskMemFree.
func MFTEnumEx(guidCategory windows.GUID, flags uint32, pInputType, pOutputType *MFT_REGISTER_TYPE_INFO) ([]*IMFActivate, error) {
	var pppActivate unsafe.Pointer
	var count uint32

	ret, _, _ := syscall.SyscallN(procMFTEnumEx.Addr(),
		uintptr(unsafe.Pointer(&guidCategory)),
		uintptr(flags),
		uintptr(unsafe.Pointer(pInputType)),
		uintptr(unsafe.Pointer(pOutputType)),
		uintptr(unsafe.Pointer(&pppActivate)),
		uintptr(unsafe.Pointer(&count)),
	)
	if d3d.HRESULT(ret).Failed() {
		return nil, fmt.Errorf("MFTEnumEx: %w", d3d.HRESULT(ret))
	}
	if count == 0 || pppActivate == nil {
		return nil, nil
	}

	activates := unsafe.Slice((**IMFActivate)(pppActivate), count)
	result := make([]*IMFActivate, count)
	copy(result, activates)

	CoTaskMemFree(pppActivate)
	return result, nil
}

var procCoTaskMemFree = modOle32.NewProc("CoTaskMemFree")

func CoTaskMemFree(pv unsafe.Pointer) {
	syscall.SyscallN(procCoTaskMemFree.Addr(), uintptr(pv))
}
