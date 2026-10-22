//go:build windows

package audio

import (
	"encoding/binary"
	"math"
	"testing"
	"unsafe"
)

func TestAudioRenderClientVtblLayout(t *testing.T) {
	var vtbl iAudioRenderClientVtbl
	ptr := unsafe.Sizeof(uintptr(0))
	if got := unsafe.Sizeof(vtbl); got != 5*ptr {
		t.Errorf("sizeof(IAudioRenderClientVtbl)=%d, want %d", got, 5*ptr)
	}
	if got := unsafe.Offsetof(vtbl.GetBuffer); got != 3*ptr {
		t.Errorf("GetBuffer offset=%d, want %d", got, 3*ptr)
	}
	if got := unsafe.Offsetof(vtbl.ReleaseBuffer); got != 4*ptr {
		t.Errorf("ReleaseBuffer offset=%d, want %d", got, 4*ptr)
	}
}

func TestWriteFloat32PCMFloatStereo(t *testing.T) {
	src := []float32{-1, -0.5, 0, 0.25, 0.5, 1}
	dst := make([]byte, len(src)*4)
	writeFloat32PCM(dst, src, Format{SampleRate: OpusSampleRate, Channels: 2, Float32: true}, 2)

	for i, want := range src {
		got := math.Float32frombits(binary.LittleEndian.Uint32(dst[i*4:]))
		if got != want {
			t.Errorf("sample %d = %v, want %v", i, got, want)
		}
	}
}

func TestWriteFloat32PCMPCM16(t *testing.T) {
	src := []float32{0, 0.5, -0.5, 2, -2, float32(math.NaN()), 1, -1}
	dst := make([]byte, len(src)*2)
	writeFloat32PCM(dst, src, Format{SampleRate: OpusSampleRate, Channels: 2, Float32: false}, 2)

	want := []int16{0, 16383, -16383, 32767, -32768, 0, 32767, -32767}
	for i, w := range want {
		got := int16(binary.LittleEndian.Uint16(dst[i*2:]))
		if got != w {
			t.Errorf("sample %d = %d, want %d", i, got, w)
		}
	}
}

func TestWriteFloat32PCMChannelRemap(t *testing.T) {
	// 单声道 → 双声道：每个采样复制到左右。
	mono := []float32{0.25, -0.5}
	dst := make([]byte, len(mono)*2*4)
	writeFloat32PCM(dst, mono, Format{SampleRate: OpusSampleRate, Channels: 2, Float32: true}, 1)
	for i, want := range []float32{0.25, 0.25, -0.5, -0.5} {
		got := math.Float32frombits(binary.LittleEndian.Uint32(dst[i*4:]))
		if got != want {
			t.Errorf("upmix sample %d = %v, want %v", i, got, want)
		}
	}

	// 双声道 → 单声道：取平均。
	stereo := []float32{-1, 1, 0.5, 0.5, 1, -1}
	dst = make([]byte, len(stereo)/2*2)
	writeFloat32PCM(dst, stereo, Format{SampleRate: OpusSampleRate, Channels: 1, Float32: false}, 2)
	for i, want := range []int16{0, 16383, 0} {
		got := int16(binary.LittleEndian.Uint16(dst[i*2:]))
		if got != want {
			t.Errorf("downmix sample %d = %d, want %d", i, got, want)
		}
	}
}
