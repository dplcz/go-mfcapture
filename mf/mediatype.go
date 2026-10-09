package mf

import (
	"fmt"
	"structs"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/com"
)

// IMFMediaTypeVtbl = IMFAttributes's 33 slots + 5 of its own.
type IMFMediaTypeVtbl struct {
	_ structs.HostLayout
	IMFAttributesVtbl

	GetMajorType       uintptr
	IsCompressedFormat uintptr
	IsEqual            uintptr
	GetRepresentation  uintptr
	FreeRepresentation uintptr
}

type IMFMediaType struct {
	_    structs.HostLayout
	vtbl *IMFMediaTypeVtbl
}

func (obj *IMFMediaType) QueryInterface(iid windows.GUID, pp interface{}) int32 {
	return com.ReflectQueryInterface(obj, obj.vtbl.QueryInterface, &iid, pp)
}

func (obj *IMFMediaType) SetUINT32(key windows.GUID, value uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetUINT32,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(value),
	)
	return int32(ret)
}

func (obj *IMFMediaType) SetUINT64(key windows.GUID, value uint64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetUINT64,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(value),
	)
	return int32(ret)
}

func (obj *IMFMediaType) SetGUID(key windows.GUID, value windows.GUID) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.SetGUID,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(&value)),
	)
	return int32(ret)
}

// GetGUID 读取一个 GUID 属性（例如 MF_MT_SUBTYPE）。
func (obj *IMFMediaType) GetGUID(key windows.GUID, value *windows.GUID) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetGUID,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(value)),
	)
	return int32(ret)
}

// GetUINT32 读取一个 UINT32 属性（例如 MF_MT_DEFAULT_STRIDE）。
func (obj *IMFMediaType) GetUINT32(key windows.GUID, value *uint32) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetUINT32,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(value)),
	)
	return int32(ret)
}

// GetUINT64 读取一个 UINT64 属性（例如 MF_MT_FRAME_SIZE，高 32 位宽、低 32 位高）。
func (obj *IMFMediaType) GetUINT64(key windows.GUID, value *uint64) int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.GetUINT64,
		uintptr(unsafe.Pointer(obj)),
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(value)),
	)
	return int32(ret)
}

func (obj *IMFMediaType) Release() int32 {
	ret, _, _ := syscall.SyscallN(obj.vtbl.Release, uintptr(unsafe.Pointer(obj)))
	return int32(ret)
}

// NewH264DecodeInputType 构造解码器的输入类型（压缩的 H264）。
//
// width/height 传 0 时表示"由码流里的 SPS 自己推断"——多数 H264 解码器 MFT
// 都接受这种写法，某些则要求在输入类型上就给出分辨率，所以调用方应做好两种都试的准备。
func NewH264DecodeInputType(width, height, fpsNum, fpsDen uint32) (*IMFMediaType, error) {
	var mt *IMFMediaType
	if hr := MFCreateMediaType(&mt); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("MFCreateMediaType: %w", d3d.HRESULT(hr))
	}
	type step struct {
		name string
		hr   int32
	}
	steps := []step{
		{"SetGUID(MF_MT_MAJOR_TYPE)", mt.SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video)},
		{"SetGUID(MF_MT_SUBTYPE)", mt.SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264)},
		{"SetUINT32(MF_MT_INTERLACE_MODE)", mt.SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive)},
	}
	if width > 0 && height > 0 {
		steps = append(steps, step{"SetUINT64(MF_MT_FRAME_SIZE)",
			mt.SetUINT64(MF_MT_FRAME_SIZE, pack2UINT32AsUINT64(width, height))})
	}
	if fpsNum > 0 && fpsDen > 0 {
		steps = append(steps, step{"SetUINT64(MF_MT_FRAME_RATE)",
			mt.SetUINT64(MF_MT_FRAME_RATE, pack2UINT32AsUINT64(fpsNum, fpsDen))})
	}
	for _, s := range steps {
		if d3d.HRESULT(s.hr).Failed() {
			mt.Release()
			return nil, fmt.Errorf("%s: %w", s.name, d3d.HRESULT(s.hr))
		}
	}
	return mt, nil
}

// NewUncompressedVideoType 构造未压缩视频类型，用于解码器输出。
func NewUncompressedVideoType(subtype windows.GUID, width, height, fpsNum, fpsDen uint32) (*IMFMediaType, error) {
	var mt *IMFMediaType
	if hr := MFCreateMediaType(&mt); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("MFCreateMediaType: %w", d3d.HRESULT(hr))
	}
	type step struct {
		name string
		hr   int32
	}
	steps := []step{
		{"SetGUID(MF_MT_MAJOR_TYPE)", mt.SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video)},
		{"SetGUID(MF_MT_SUBTYPE)", mt.SetGUID(MF_MT_SUBTYPE, subtype)},
		{"SetUINT32(MF_MT_INTERLACE_MODE)", mt.SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive)},
		{"SetUINT64(MF_MT_FRAME_SIZE)", mt.SetUINT64(MF_MT_FRAME_SIZE, pack2UINT32AsUINT64(width, height))},
		{"SetUINT64(MF_MT_PIXEL_ASPECT_RATIO)", mt.SetUINT64(MF_MT_PIXEL_ASPECT_RATIO, pack2UINT32AsUINT64(1, 1))},
	}
	if fpsNum > 0 && fpsDen > 0 {
		steps = append(steps, step{"SetUINT64(MF_MT_FRAME_RATE)",
			mt.SetUINT64(MF_MT_FRAME_RATE, pack2UINT32AsUINT64(fpsNum, fpsDen))})
	}
	for _, s := range steps {
		if d3d.HRESULT(s.hr).Failed() {
			mt.Release()
			return nil, fmt.Errorf("%s: %w", s.name, d3d.HRESULT(s.hr))
		}
	}
	return mt, nil
}

func pack2UINT32AsUINT64(high, low uint32) uint64 {
	return uint64(high)<<32 | uint64(low)
}

// NewOutputMediaType builds the fixed-parameter H264 output type: native resolution, fixed bitrate, no adaptive QoS.
func NewOutputMediaType(width, height, fpsNum, fpsDen, bitrateBps uint32) (*IMFMediaType, error) {
	var mt *IMFMediaType
	if hr := MFCreateMediaType(&mt); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("MFCreateMediaType: %w", d3d.HRESULT(hr))
	}
	type kv struct {
		name string
		hr   int32
	}
	steps := []kv{
		{"SetGUID(MF_MT_MAJOR_TYPE)", mt.SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video)},
		{"SetGUID(MF_MT_SUBTYPE)", mt.SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264)},
		{"SetUINT32(MF_MT_AVG_BITRATE)", mt.SetUINT32(MF_MT_AVG_BITRATE, bitrateBps)},
		{"SetUINT32(MF_MT_INTERLACE_MODE)", mt.SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive)},
		{"SetUINT64(MF_MT_FRAME_SIZE)", mt.SetUINT64(MF_MT_FRAME_SIZE, pack2UINT32AsUINT64(width, height))},
		{"SetUINT64(MF_MT_FRAME_RATE)", mt.SetUINT64(MF_MT_FRAME_RATE, pack2UINT32AsUINT64(fpsNum, fpsDen))},
		{"SetUINT64(MF_MT_PIXEL_ASPECT_RATIO)", mt.SetUINT64(MF_MT_PIXEL_ASPECT_RATIO, pack2UINT32AsUINT64(1, 1))},
	}
	for _, s := range steps {
		if d3d.HRESULT(s.hr).Failed() {
			mt.Release()
			return nil, fmt.Errorf("%s: %w", s.name, d3d.HRESULT(s.hr))
		}
	}
	return mt, nil
}

// NewInputMediaType builds the uncompressed BGRA (ARGB32) input type matching DXGI desktop duplication output.
func NewInputMediaType(width, height, fpsNum, fpsDen uint32) (*IMFMediaType, error) {
	var mt *IMFMediaType
	if hr := MFCreateMediaType(&mt); d3d.HRESULT(hr).Failed() {
		return nil, fmt.Errorf("MFCreateMediaType: %w", d3d.HRESULT(hr))
	}
	type kv struct {
		name string
		hr   int32
	}
	steps := []kv{
		{"SetGUID(MF_MT_MAJOR_TYPE)", mt.SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video)},
		{"SetGUID(MF_MT_SUBTYPE)", mt.SetGUID(MF_MT_SUBTYPE, MFVideoFormat_ARGB32)},
		{"SetUINT32(MF_MT_INTERLACE_MODE)", mt.SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive)},
		{"SetUINT32(MF_MT_ALL_SAMPLES_INDEPENDENT)", mt.SetUINT32(MF_MT_ALL_SAMPLES_INDEPENDENT, 1)},
		{"SetUINT64(MF_MT_FRAME_SIZE)", mt.SetUINT64(MF_MT_FRAME_SIZE, pack2UINT32AsUINT64(width, height))},
		{"SetUINT64(MF_MT_FRAME_RATE)", mt.SetUINT64(MF_MT_FRAME_RATE, pack2UINT32AsUINT64(fpsNum, fpsDen))},
		{"SetUINT64(MF_MT_PIXEL_ASPECT_RATIO)", mt.SetUINT64(MF_MT_PIXEL_ASPECT_RATIO, pack2UINT32AsUINT64(1, 1))},
	}
	for _, s := range steps {
		if d3d.HRESULT(s.hr).Failed() {
			mt.Release()
			return nil, fmt.Errorf("%s: %w", s.name, d3d.HRESULT(s.hr))
		}
	}
	return mt, nil
}
