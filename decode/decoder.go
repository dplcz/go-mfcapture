package decode

import (
	"time"

	"github.com/kirides/go-d3d/d3d11"
	"golang.org/x/sys/windows"
)

// DecodedFrame 是一帧解码后的像素。
type DecodedFrame struct {
	Data     []byte                 // CPU 路径：像素数据，布局由 Subtype 决定
	Texture  *d3d11.ID3D11Texture2D // GPU 路径（DXVA）：非 nil 时调用方须 Release
	SubIndex uint32                 // Texture 内的 subresource 索引
	Subtype  windows.GUID
	Width    uint32
	Height   uint32
	Stride   int32
	Timestamp time.Duration
	Duration  time.Duration
}

// Decoder abstracts a video decoder. Platform-specific implementations
// (Media Foundation MFT on Windows, ffmpeg on Linux, etc.) satisfy this
// interface so the application layer remains platform-agnostic.
type Decoder interface {
	Decode(au []byte, ts, dur time.Duration) ([]*DecodedFrame, error)
	Flush() ([]*DecodedFrame, error)
	OutputFormat() (subtype windows.GUID, width, height uint32, stride int32)
	Release()
}
