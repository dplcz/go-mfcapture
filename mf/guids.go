package mf

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// GUID values cross-checked against mingw-w64's mfapi.h / mfobjects.h / mfreadwrite.h mirrors.
var (
	MF_MT_MAJOR_TYPE, _              = windows.GUIDFromString("{48eba18e-f8c9-4687-bf11-0a74c9f96a8f}")
	MF_MT_SUBTYPE, _                 = windows.GUIDFromString("{f7e34c9a-42e8-4714-b74b-cb29d72c35e5}")
	MF_MT_ALL_SAMPLES_INDEPENDENT, _ = windows.GUIDFromString("{c9173739-5e56-461c-b713-46fb995cb95f}")
	MF_MT_FRAME_SIZE, _              = windows.GUIDFromString("{1652c33d-d6b2-4012-b834-72030849a37d}")
	MF_MT_FRAME_RATE, _              = windows.GUIDFromString("{c459a2e8-3d2c-4e44-b132-fee5156c7bb0}")
	MF_MT_PIXEL_ASPECT_RATIO, _      = windows.GUIDFromString("{c6376a1e-8d0a-4027-be45-6d9a0ad39bb6}")
	MF_MT_INTERLACE_MODE, _          = windows.GUIDFromString("{e2724bb8-e676-4806-b4b2-a8d6efb44ccd}")
	MF_MT_DEFAULT_STRIDE, _          = windows.GUIDFromString("{644b4e48-1e02-4516-b0eb-c01ca9d49ac6}")
	MF_MT_AVG_BITRATE, _             = windows.GUIDFromString("{20332624-fb0d-4d9e-bd0d-cbf6786c102e}")

	MFMediaType_Video, _    = windows.GUIDFromString("{73646976-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_H264, _   = windows.GUIDFromString("{34363248-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_ARGB32, _ = windows.GUIDFromString("{00000015-0000-0010-8000-00aa00389b71}")

	// 编码器常见的输入子类型。GUID 前半部分是 FOURCC 的小端表示
	// （例如 NV12 -> 'N','V','1','2' -> 0x3231564E），与 mfapi.h 一致。
	MFVideoFormat_RGB32, _ = windows.GUIDFromString("{00000016-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_RGB24, _ = windows.GUIDFromString("{00000014-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_NV12, _  = windows.GUIDFromString("{3231564e-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_NV21, _  = windows.GUIDFromString("{3132564e-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_I420, _  = windows.GUIDFromString("{30323449-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_IYUV, _  = windows.GUIDFromString("{56555949-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_YV12, _  = windows.GUIDFromString("{32315659-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_YUY2, _  = windows.GUIDFromString("{32595559-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_UYVY, _  = windows.GUIDFromString("{59565955-0000-0010-8000-00aa00389b71}")
	MFVideoFormat_P010, _  = windows.GUIDFromString("{30313050-0000-0010-8000-00aa00389b71}")

	MF_SINK_WRITER_D3D_MANAGER, _              = windows.GUIDFromString("{ec822da2-e1e9-4b29-a0d8-563c719f5269}")
	MF_READWRITE_ENABLE_HARDWARE_TRANSFORMS, _ = windows.GUIDFromString("{a634a91c-822b-41b9-a494-4de4643612b0}")

	IID_IMFAttributes, _        = windows.GUIDFromString("{2cd2d921-c447-44a7-a13c-4adabfc247e3}")
	IID_IMFMediaType, _         = windows.GUIDFromString("{44ae0fa8-ea31-4109-8d2e-4cae4997c555}")
	IID_IMFSample, _            = windows.GUIDFromString("{c40a00f2-b93a-4d80-ae8c-5a1c634f58e4}")
	IID_IMFMediaBuffer, _       = windows.GUIDFromString("{045fa593-8799-42b8-bc8d-8968c6453507}")
	IID_IMFSinkWriter, _        = windows.GUIDFromString("{3137f1cd-fe5e-4805-a5d8-fb477448cb3d}")
	IID_IMFDXGIDeviceManager, _ = windows.GUIDFromString("{eb533d5d-2db6-40f8-97a9-494692014f07}")

	// IMFTransform / MFT enumeration
	IID_IMFTransform, _           = windows.GUIDFromString("{bf94c121-5b05-4e6f-8000-ba598961414d}")
	MFT_CATEGORY_VIDEO_ENCODER, _ = windows.GUIDFromString("{f79eac7d-e545-4387-bdee-d647d7bde42a}")
	MFT_CATEGORY_VIDEO_DECODER, _ = windows.GUIDFromString("{d6c02d4b-6833-45b4-971a-05a4b04bab91}")

	// Low-latency / async attributes
	MF_TRANSFORM_ASYNC_UNLOCK, _ = windows.GUIDFromString("{e5666d6b-3422-4eb6-a421-da7db1f8e207}")
	MF_LOW_LATENCY, _            = windows.GUIDFromString("{9c27891a-ed7a-40e1-88e8-b22727a024ee}")

	// ICodecAPI
	IID_ICodecAPI, _ = windows.GUIDFromString("{901db4c7-31ce-41a2-85dc-8fa0bf41b8da}")

	// MF_SA_D3D11_AWARE：MFT 是否接受 DXGI/D3D11 样本（软件编码器通常为 0）。
	MF_SA_D3D11_AWARE, _ = windows.GUIDFromString("{206b4fc8-fcf9-4c51-afe3-9764369e33a0}")

	// CodecAPI property GUIDs
	CODECAPI_AVLowLatencyMode, _           = windows.GUIDFromString("{9c27891a-ed7a-40e1-88e8-b22727a024ee}")
	CODECAPI_AVEncCommonRateControlMode, _ = windows.GUIDFromString("{1c0608e9-370c-4710-8a58-cb6181c42423}")
	CODECAPI_AVEncCommonMeanBitRate, _     = windows.GUIDFromString("{f7222374-2144-4815-b550-a37f8e12ee52}")
	CODECAPI_AVEncVideoForceKeyFrame, _    = windows.GUIDFromString("{398c1b98-8353-475a-9ef2-8f265d260345}")
	CODECAPI_AVEncMPVGOPSize, _            = windows.GUIDFromString("{95f31b26-95a4-41aa-9303-246a7fc6eef1}")
	CODECAPI_AVEncNumWorkerThreads, _      = windows.GUIDFromString("{b0c8bf60-16f7-4951-a30b-1db1609293d6}")

	// Sample attribute for keyframe detection
	MFSampleExtension_CleanPoint, _ = windows.GUIDFromString("{9cdf01d8-a0f0-43ba-b077-eaa06cbd728a}")
)

// MFVideoInterlace_Progressive is a UINT32 enum value (not a GUID) for MF_MT_INTERLACE_MODE.
const MFVideoInterlace_Progressive = 2

// eAVEncCommonRateControlMode enum values.
const (
	EAVEncCommonRateControlMode_CBR                = 0
	EAVEncCommonRateControlMode_PeakConstrainedVBR = 1
	EAVEncCommonRateControlMode_UnconstrainedVBR   = 2
	EAVEncCommonRateControlMode_Quality            = 3
)

// SubtypeName 把常见的视频子类型 GUID 变成可读名字，用于诊断输出。
// 未知 GUID 返回其 FOURCC 的十六进制形式，不会丢信息。
func SubtypeName(g windows.GUID) string {
	switch g {
	case MFVideoFormat_H264:
		return "H264"
	case MFVideoFormat_ARGB32:
		return "ARGB32"
	case MFVideoFormat_RGB32:
		return "RGB32"
	case MFVideoFormat_RGB24:
		return "RGB24"
	case MFVideoFormat_NV12:
		return "NV12"
	case MFVideoFormat_NV21:
		return "NV21"
	case MFVideoFormat_I420:
		return "I420"
	case MFVideoFormat_IYUV:
		return "IYUV"
	case MFVideoFormat_YV12:
		return "YV12"
	case MFVideoFormat_YUY2:
		return "YUY2"
	case MFVideoFormat_UYVY:
		return "UYVY"
	case MFVideoFormat_P010:
		return "P010"
	}
	return fmt.Sprintf("{%08x-...}", g.Data1)
}
