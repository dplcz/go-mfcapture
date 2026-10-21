package main

import (
	"encoding/binary"
	"fmt"
	"io"

	"mfcapture/audio"
)

// oggOpusWriter 是最小的 RFC 7845 Ogg Opus 封装：写 OpusHead、OpusTags，
// 然后给每个 Opus 包写一页。只用于 cmd/mfaudio 的冒烟输出，不做多路复用、
// 不做 seek、不处理 chained streams。
//
// granule position 按 48 kHz 采样计，从第一个包的第一个采样开始，包含
// pre-skip——播放器的起播时间由 OpusHead 里的 pre-skip 扣除。
type oggOpusWriter struct {
	w      io.Writer
	serial uint32
	seq    uint32

	granule    int64
	haveAudio  bool
	pending    []byte // 最后一包延迟写入，Close 时才能给它打 EOS
	pendingDur int    // 待写包的采样数（48 kHz）
}

// Ogg 页头标志。
const (
	oggFlagBOS = 0x02 // beginning of stream
	oggFlagEOS = 0x04 // end of stream
)

// newOggOpusWriter 写 OpusHead 与 OpusTags 两个头页，返回可写音频页的
// writer。channels 必须与后续包的声道数一致，preSkip 来自编码器。
func newOggOpusWriter(w io.Writer, channels, preSkip int, serial uint32) (*oggOpusWriter, error) {
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("ogg: channels %d, want 1 or 2", channels)
	}
	if preSkip < 0 || preSkip > 0xFFFF {
		return nil, fmt.Errorf("ogg: pre-skip %d does not fit the header", preSkip)
	}

	o := &oggOpusWriter{w: w, serial: serial}

	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8] = 1 // version
	head[9] = byte(channels)
	binary.LittleEndian.PutUint16(head[10:], uint16(preSkip))
	binary.LittleEndian.PutUint32(head[12:], audio.OpusSampleRate)
	binary.LittleEndian.PutUint16(head[16:], 0) // output gain
	head[18] = 0                                // mapping family 0：单/立体声
	if err := o.writePage(head, 0, oggFlagBOS); err != nil {
		return nil, err
	}

	vendor := []byte("mfcapture")
	tags := make([]byte, 8+4+len(vendor)+4)
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:], uint32(len(vendor)))
	copy(tags[12:], vendor)
	binary.LittleEndian.PutUint32(tags[12+len(vendor):], 0) // user comment count
	if err := o.writePage(tags, 0, 0); err != nil {
		return nil, err
	}
	return o, nil
}

// writePacket 把一个 Opus 包排到下一页。samples 是包内 48 kHz 采样数
// （20 ms = 960）。为了保证最后一页能带 EOS，包会延迟一页写入。
func (o *oggOpusWriter) writePacket(pkt []byte, samples int) error {
	if samples <= 0 {
		return fmt.Errorf("ogg: packet duration %d samples, want a positive value", samples)
	}
	if err := o.flushPending(false); err != nil {
		return err
	}
	o.pending = append(o.pending[:0], pkt...)
	o.pendingDur = samples
	return nil
}

// Close 把最后一页标成 EOS 写出。没有任何音频包时补一个空的 EOS 页，
// 让文件仍然是一个完整终止的 Ogg 流。
func (o *oggOpusWriter) Close() error {
	if err := o.flushPending(true); err != nil {
		return err
	}
	if !o.haveAudio {
		// 页内没有完整包，按规范 granule position 写 -1。
		return o.writePage(nil, -1, oggFlagEOS)
	}
	return nil
}

func (o *oggOpusWriter) flushPending(eos bool) error {
	if o.pending == nil {
		return nil
	}
	o.granule += int64(o.pendingDur)
	var flags byte
	if eos {
		flags = oggFlagEOS
	}
	if err := o.writePage(o.pending, o.granule, flags); err != nil {
		return err
	}
	o.pending = o.pending[:0]
	o.pendingDur = 0
	o.haveAudio = true
	return nil
}

// writePage 写一个只含一个包的 Ogg 页。pkt 为 nil 时写出零 segment 的空页。
func (o *oggOpusWriter) writePage(pkt []byte, granule int64, flags byte) error {
	segs := oggLacing(len(pkt))
	page := make([]byte, 27+len(segs)+len(pkt))
	copy(page, "OggS")
	page[4] = 0 // 版本
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:], uint64(granule))
	binary.LittleEndian.PutUint32(page[14:], o.serial)
	binary.LittleEndian.PutUint32(page[18:], o.seq)
	// 校验和字段先保持 0，算完再填回去。
	page[26] = byte(len(segs))
	copy(page[27:], segs)
	copy(page[27+len(segs):], pkt)
	binary.LittleEndian.PutUint32(page[22:], oggCRC(page))

	if _, err := o.w.Write(page); err != nil {
		return fmt.Errorf("ogg: write page %d: %w", o.seq, err)
	}
	o.seq++
	return nil
}

// oggLacing 把包长拆成 Ogg 的 segment 表：每个 255 表示"后面还有"，
// 最后一个小于 255 的值结束一个包。零长度包不产生 segment。
func oggLacing(n int) []byte {
	if n == 0 {
		return nil
	}
	segs := make([]byte, 0, n/255+1)
	for n >= 255 {
		segs = append(segs, 255)
		n -= 255
	}
	return append(segs, byte(n))
}

// oggCRCTable 是直接（非反射）CRC-32，多项式 0x04C11DB7、初值 0、无结果异或。
// Ogg 用的不是标准库里的 IEEE 反射实现，所以这里自带一份。
var oggCRCTable = func() (t [256]uint32) {
	for i := range t {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04C11DB7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(p []byte) uint32 {
	var crc uint32
	for _, b := range p {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}
