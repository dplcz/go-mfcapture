package audio

import (
	"errors"
	"time"
)

// ErrTimeout 表示在超时时间内没有采集到数据。loopback 在没有进程播放声音时
// 不会产生任何包，这是正常状态，调用方应据此补静音而不是当作故障。
var ErrTimeout = errors.New("audio: capture timed out")

// ErrDeviceLost 表示默认音频端点被拔出、切换或被禁用，采集流已经失效。
// 调用方应销毁 Capturer 并重建；重建期间可以继续用 Framer 补静音。
var ErrDeviceLost = errors.New("audio: capture device lost")

// Capturer 抽象一个音频采集源，与视频侧的 capture.Capturer 对称。
type Capturer interface {
	// Format 返回采集源固定的 PCM 格式。音频链路要求 48000 Hz；非 48 kHz
	// 的端点需要先重采样，这一层目前不做。
	Format() Format
	// ReadFrame 最多等待 timeout。没有数据时返回 (nil, ErrTimeout)；
	// 其他错误表示采集源已经失效。
	//
	// 返回的 Frame.Data 只在下一次 ReadFrame 之前有效，需要长期持有必须复制。
	ReadFrame(timeout time.Duration) (*Frame, error)
	Release()
}
