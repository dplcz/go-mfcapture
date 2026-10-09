package decode

import (
	"sync/atomic"
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
	// owner 非 nil 时表示这一帧来自 Session：Release 不就地释放 COM 对象，
	// 而是把帧投递回 Session.Run 所在线程归还（DXVA 下释放必须和解码同线程）。
	owner *Session
	// released 保证重复 Release 是安全的空操作：Session 路径下归还动作是
	// 异步的，没有这个标记，渲染线程重复调用会把同一帧归还两次（引用计数下溢）。
	released atomic.Bool
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
//
// 帧若来自 Session，本方法可以在任意（渲染）线程调用：它不就地释放 COM 对象，
// 而是把帧投递回 Session.Run 所在线程归还，因此跨线程是安全的。
func (f *DecodedFrame) Release() {
	if f == nil {
		return
	}
	if !f.released.CompareAndSwap(false, true) {
		return // 已经归还过：重复调用是安全的空操作
	}
	if f.owner != nil {
		// 来自 Session：把帧投递回 Run 所在线程归还，不在本线程动 COM 对象。
		f.owner.returnFrame(f)
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
//
// 线程契约（Windows/MF 实现尤其重要，实现者必须自己满足）：
// New / Decode / Flush / Release 必须全程发生在**同一个 OS 线程**上，且该线程
// 已经 CoInitializeEx（MTA）+ MFStartup。原因是 goroutine 会被调度到任意 OS
// 线程，而每个要用 COM 的线程都得自己初始化；DXVA 路径还要求 MFT 交互固定单线程。
// 本接口**不**负责这件事——想要"我什么都不管"的调用方请用 Session，它在 Run 内部
// 完成 LockOSThread + COM/MF 初始化与清理，并把所有调用收敛到那一个线程上。
type Decoder interface {
	Decode(au []byte, ts, dur time.Duration) ([]*DecodedFrame, error)
	Flush() ([]*DecodedFrame, error)
	OutputFormat() (subtype windows.GUID, width, height uint32, stride int32)
	Release()
}
