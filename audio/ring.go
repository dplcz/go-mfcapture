package audio

import (
	"fmt"
	"time"
)

// defaultMaxLatency 是 Ring/Framer 的默认积压上限。超过这个时长的音频已经
// 没有实时价值，丢弃最旧的整帧比继续排队更能保护唇音同步。
const defaultMaxLatency = 200 * time.Millisecond

// Ring 是固定容量的交错 PCM 字节 FIFO。
//
// 它只做字节搬运，不解释时间戳：分帧、静音与 PTS 由 Framer 负责。容量按
// Format 和 maxLatency 换算成完整 20 ms 帧；写入溢出时丢弃最旧的整帧，
// 而不是把某一帧切成两半（半帧会造成一个 20 ms 的破音）。
//
// 不是并发安全的。
type Ring struct {
	format         Format
	buf            []byte
	head, tail     int
	frameBytes     int // 一个 20 ms 帧的字节数，也是积压丢弃的对齐单位
	droppedSamples int // 累计丢弃的采样帧数（每声道）
}

// NewRing 创建一个缓冲 maxLatency 时长的 Ring。maxLatency <= 0 时取 200 ms。
func NewRing(format Format, maxLatency time.Duration) (*Ring, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}
	if maxLatency <= 0 {
		maxLatency = defaultMaxLatency
	}
	frameSamples := format.SampleRate * OpusFrameMs / 1000
	frameDur := time.Duration(frameSamples) * time.Second / time.Duration(format.SampleRate)
	frames := int((maxLatency + frameDur - 1) / frameDur)
	if frames < 1 {
		frames = 1
	}
	frameBytes := format.FrameBytes(frameSamples)
	return &Ring{
		format:     format,
		buf:        make([]byte, frames*frameBytes),
		frameBytes: frameBytes,
	}, nil
}

// Append 复制一段交错 PCM 到 ring 尾部。ring 放不下时丢弃最旧的整帧，
// 返回丢弃的采样帧数（每声道，按 Format.Samples 计）。
//
// 单个包比整个 ring 还大时只保留最新的一段。这是病理场景（WASAPI 包通常
// 10~100 ms），但必须处理，否则要么死循环要么丢新数据。
func (r *Ring) Append(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	ba := r.format.BlockAlign()
	if len(data)%ba != 0 {
		return 0, fmt.Errorf("audio: PCM length %d is not a multiple of the %d-byte block align",
			len(data), ba)
	}

	dropped := 0
	if len(data) > len(r.buf) {
		// 向上取整到整帧，保证剩下的部分仍从帧边界开始，且一定能放进 ring。
		dropBytes := len(data) - len(r.buf)
		dropBytes = (dropBytes + r.frameBytes - 1) / r.frameBytes * r.frameBytes
		if dropBytes > len(data) {
			dropBytes = len(data)
		}
		data = data[dropBytes:]
		dropped += dropBytes / ba
	}

	for r.tail-r.head+len(data) > len(r.buf) {
		avail := r.tail - r.head
		if avail >= r.frameBytes {
			r.head += r.frameBytes
			dropped += r.frameBytes / ba
			continue
		}
		// 只剩不足一帧的尾巴。容量是整帧的倍数，正常节奏下走不到这里；
		// 只有"半帧积压 + 接近满容量的大包"才会出现，丢掉这点尾巴。
		r.head = r.tail
		dropped += avail / ba
	}

	if r.tail+len(data) > len(r.buf) {
		copy(r.buf, r.buf[r.head:r.tail])
		r.tail -= r.head
		r.head = 0
	}
	copy(r.buf[r.tail:], data)
	r.tail += len(data)

	r.droppedSamples += dropped
	return dropped, nil
}

// Available 返回当前可读的采样帧数（每声道）。
func (r *Ring) Available() int {
	return (r.tail - r.head) / r.format.BlockAlign()
}

// Take 取出 sampleFrames 个采样帧。返回的切片别名 ring 内部缓冲，
// 只在下一次 Append / Take / Drop 之前有效；需要跨调用保留必须复制。
func (r *Ring) Take(sampleFrames int) ([]byte, error) {
	if sampleFrames < 0 {
		return nil, fmt.Errorf("audio: take %d frames, want a non-negative count", sampleFrames)
	}
	if sampleFrames > r.Available() {
		return nil, fmt.Errorf("audio: take %d frames, only %d available", sampleFrames, r.Available())
	}
	n := r.format.FrameBytes(sampleFrames)
	out := r.buf[r.head : r.head+n]
	r.head += n
	if r.head == r.tail {
		r.head, r.tail = 0, 0
	}
	return out, nil
}

// Drop 从头部丢弃最多 sampleFrames 个采样帧，返回实际丢弃数。
func (r *Ring) Drop(sampleFrames int) int {
	if sampleFrames <= 0 {
		return 0
	}
	if avail := r.Available(); sampleFrames > avail {
		sampleFrames = avail
	}
	r.head += r.format.FrameBytes(sampleFrames)
	if r.head == r.tail {
		r.head, r.tail = 0, 0
	}
	return sampleFrames
}

// Cap 返回 ring 的容量（采样帧数）。
func (r *Ring) Cap() int {
	return len(r.buf) / r.format.BlockAlign()
}

// DroppedSamples 返回因溢出累计丢弃的采样帧数（每声道）。
func (r *Ring) DroppedSamples() int { return r.droppedSamples }

// Reset 清空缓冲和丢弃统计。用于切换采集设备或重新开始一条流。
func (r *Ring) Reset() {
	r.head, r.tail = 0, 0
	r.droppedSamples = 0
}
