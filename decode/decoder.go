package decode

import (
	"time"

	"golang.org/x/sys/windows"
)

// DecodedFrame 是一帧解码后的像素。
type DecodedFrame struct {
	Data      []byte       // 像素数据，布局由 Subtype 决定（NV12 时是 Y 平面 + 交错的 UV 平面）
	Subtype   windows.GUID // 例如 mf.MFVideoFormat_NV12
	Width     uint32
	Height    uint32
	Stride    int32 // 每行字节数；为 0 表示等于 Width（紧凑存放）
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
