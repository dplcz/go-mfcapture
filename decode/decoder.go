package decode

import (
	"time"

	"github.com/kirides/go-d3d/d3d11"
	"golang.org/x/sys/windows"
)

// DecodedFrame 是一帧解码后的像素。
type DecodedFrame struct {
	Data      []byte                 // CPU 路径：像素数据，布局由 Subtype 决定
	Texture   *d3d11.ID3D11Texture2D // GPU 路径（DXVA）：解码器纹理，由 Release 归还
	SubIndex  uint32                 // Texture 内的 subresource 索引
	Subtype   windows.GUID
	Width     uint32
	Height    uint32
	Stride    int32
	Timestamp time.Duration
	Duration  time.Duration

	// release 非 nil 时由 Release 调用。0 拷贝路径下它同时归还解码器的
	// IMFSample——sample 活着，解码器就不会回收那张 surface。
	release func()
}

// Release 归还这一帧持有的解码器资源。
//
// GPU（DXVA 0 拷贝）路径下**必须**调用，而且要在渲染真正完成之后
// （VideoProcessorBlt + Present 都已提交）再调：Texture 是解码器
// surface 池里的纹理，提前归还会让下一帧盖掉还没显示的内容，不归还
// 则会耗尽解码器的 surface 池，表现为 ProcessInput 开始返回
// MF_E_NOTACCEPTING。同时在飞的帧保持在个位数。
//
// CPU 路径（Data 非 nil）不需要调用，调了也是安全的空操作。
func (f *DecodedFrame) Release() {
	if f == nil {
		return
	}
	if f.release != nil {
		f.release()
		f.release = nil
		f.Texture = nil
		return
	}
	if f.Texture != nil {
		f.Texture.Release()
		f.Texture = nil
	}
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
