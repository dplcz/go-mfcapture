package audio

import (
	"fmt"
	"time"
)

// fakeChunk 是采集源在 at 时刻产出的一个包。at 同时作为包内首采样的 PTS，
// 模拟 WASAPI 用 qpcPosition 锚定 epoch 后的结果。
type fakeChunk struct {
	at   time.Duration
	data []byte
}

// fakeCapturer 用虚拟时钟回放预设的包，用来在无 Windows 依赖的情况下验证
// 分帧、静音补齐和积压丢弃。虚拟时钟只在 ReadFrame 中推进，因此测试完全
// 确定，不依赖真实调度。
type fakeCapturer struct {
	format   Format
	now      time.Duration
	chunks   []fakeChunk
	next     int
	released bool
}

func newFakeCapturer(format Format, chunks ...fakeChunk) *fakeCapturer {
	return &fakeCapturer{format: format, chunks: chunks}
}

var _ Capturer = (*fakeCapturer)(nil)

func (c *fakeCapturer) Format() Format { return c.format }

func (c *fakeCapturer) ReadFrame(timeout time.Duration) (*Frame, error) {
	if c.released {
		return nil, fmt.Errorf("audio: fake capturer released")
	}
	if c.next < len(c.chunks) && c.chunks[c.next].at <= c.now+timeout {
		ch := c.chunks[c.next]
		if ch.at > c.now {
			c.now = ch.at
		}
		c.next++
		return &Frame{
			Data:      ch.data,
			Timestamp: ch.at,
			Duration:  samplesToDuration(c.format.Samples(ch.data), c.format.SampleRate),
		}, nil
	}
	// 模拟阻塞到超时：虚拟时钟前进整个 timeout。
	c.now += timeout
	return nil, ErrTimeout
}

func (c *fakeCapturer) Release() { c.released = true }
