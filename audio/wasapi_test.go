//go:build windows

package audio

import (
	"errors"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWaveFormatExtensibleLayout(t *testing.T) {
	var w waveFormatExtensible
	if got := unsafe.Sizeof(w); got != 40 {
		t.Errorf("sizeof(WAVEFORMATEXTENSIBLE)=%d, want 40", got)
	}
	for _, c := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"cbSize", unsafe.Offsetof(w.CBSize), 16},
		{"Samples", unsafe.Offsetof(w.Samples), 18},
		{"ChannelMask", unsafe.Offsetof(w.ChannelMask), 20},
		{"SubFormat", unsafe.Offsetof(w.SubFormat), 24},
	} {
		if c.got != c.want {
			t.Errorf("%s offset=%d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestBuildTargetFormat(t *testing.T) {
	stereo := buildTargetFormat(2)
	if stereo.FormatTag != waveFormatTagExtensible {
		t.Errorf("FormatTag=0x%04X, want 0x%04X", stereo.FormatTag, waveFormatTagExtensible)
	}
	if stereo.Channels != 2 || stereo.SamplesPerSec != OpusSampleRate {
		t.Errorf("format = %d ch @ %d Hz, want 2 ch @ %d", stereo.Channels, stereo.SamplesPerSec, OpusSampleRate)
	}
	if stereo.BlockAlign != 8 || stereo.AvgBytesPerSec != 384000 {
		t.Errorf("block align/avg bytes = %d/%d, want 8/384000", stereo.BlockAlign, stereo.AvgBytesPerSec)
	}
	if stereo.BitsPerSample != 32 || stereo.Samples != 32 || stereo.CBSize != 22 {
		t.Errorf("bits/samples/cbSize = %d/%d/%d, want 32/32/22",
			stereo.BitsPerSample, stereo.Samples, stereo.CBSize)
	}
	if stereo.ChannelMask != speakerFrontLeft|speakerFrontRight {
		t.Errorf("ChannelMask=0x%X, want stereo mask", stereo.ChannelMask)
	}
	if stereo.SubFormat != ksDataFormatSubtypeIEEEFloat {
		t.Errorf("SubFormat=%v, want IEEE float", stereo.SubFormat)
	}

	mono := buildTargetFormat(1)
	if mono.BlockAlign != 4 || mono.AvgBytesPerSec != 192000 {
		t.Errorf("mono block align/avg bytes = %d/%d, want 4/192000", mono.BlockAlign, mono.AvgBytesPerSec)
	}
	if mono.ChannelMask != speakerFrontCenter {
		t.Errorf("mono ChannelMask=0x%X, want front center", mono.ChannelMask)
	}
}

func TestWaveFormatToFormat(t *testing.T) {
	cases := []struct {
		name string
		p    unsafe.Pointer
		want Format
		ok   bool
	}{
		{
			name: "plain IEEE float",
			p: unsafe.Pointer(&waveFormatEx{
				FormatTag: waveFormatTagIEEEFloat, Channels: 2, SamplesPerSec: 48000,
				BlockAlign: 8, BitsPerSample: 32,
			}),
			want: Format{SampleRate: 48000, Channels: 2, Float32: true},
			ok:   true,
		},
		{
			name: "plain PCM 16",
			p: unsafe.Pointer(&waveFormatEx{
				FormatTag: waveFormatTagPCM, Channels: 1, SamplesPerSec: 48000,
				BlockAlign: 2, BitsPerSample: 16,
			}),
			want: Format{SampleRate: 48000, Channels: 1},
			ok:   true,
		},
		{
			name: "extensible float",
			p: unsafe.Pointer(&waveFormatExtensible{
				FormatTag: waveFormatTagExtensible, Channels: 2, SamplesPerSec: 48000,
				BlockAlign: 8, BitsPerSample: 32, CBSize: 22, Samples: 32,
				SubFormat: ksDataFormatSubtypeIEEEFloat,
			}),
			want: Format{SampleRate: 48000, Channels: 2, Float32: true},
			ok:   true,
		},
		{
			name: "extensible PCM 16",
			p: unsafe.Pointer(&waveFormatExtensible{
				FormatTag: waveFormatTagExtensible, Channels: 2, SamplesPerSec: 48000,
				BlockAlign: 4, BitsPerSample: 16, CBSize: 22, Samples: 16,
				SubFormat: ksDataFormatSubtypePCM,
			}),
			want: Format{SampleRate: 48000, Channels: 2},
			ok:   true,
		},
		{
			name: "other sample rate is parsed",
			p: unsafe.Pointer(&waveFormatEx{
				FormatTag: waveFormatTagIEEEFloat, Channels: 2, SamplesPerSec: 44100,
				BlockAlign: 8, BitsPerSample: 32,
			}),
			want: Format{SampleRate: 44100, Channels: 2, Float32: true},
			ok:   true,
		},
		{
			name: "24-bit PCM",
			p: unsafe.Pointer(&waveFormatEx{
				FormatTag: waveFormatTagPCM, Channels: 2, SamplesPerSec: 48000,
				BlockAlign: 6, BitsPerSample: 24,
			}),
		},
		{
			name: "five point one",
			p: unsafe.Pointer(&waveFormatExtensible{
				FormatTag: waveFormatTagExtensible, Channels: 6, SamplesPerSec: 48000,
				BlockAlign: 24, BitsPerSample: 32, CBSize: 22, Samples: 32,
				SubFormat: ksDataFormatSubtypeIEEEFloat,
			}),
		},
		{
			name: "extensible without cbSize",
			p: unsafe.Pointer(&waveFormatExtensible{
				FormatTag: waveFormatTagExtensible, Channels: 2, SamplesPerSec: 48000,
				BlockAlign: 8, BitsPerSample: 32, CBSize: 0,
				SubFormat: ksDataFormatSubtypeIEEEFloat,
			}),
		},
		{
			name: "unknown subformat",
			p: unsafe.Pointer(&waveFormatExtensible{
				FormatTag: waveFormatTagExtensible, Channels: 2, SamplesPerSec: 48000,
				BlockAlign: 8, BitsPerSample: 32, CBSize: 22,
				SubFormat: windows.GUID{Data1: 0x0099},
			}),
		},
		{
			name: "unsupported tag",
			p: unsafe.Pointer(&waveFormatEx{
				FormatTag: 0x0055, Channels: 2, SamplesPerSec: 48000, BitsPerSample: 16,
			}),
		},
		{name: "nil", p: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := waveFormatToFormat(c.p)
			if c.ok {
				if err != nil {
					t.Fatalf("waveFormatToFormat: %v", err)
				}
				if got != c.want {
					t.Errorf("got %+v, want %+v", got, c.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("waveFormatToFormat(%+v)=%+v, want error", c.name, got)
			}
		})
	}
}

func TestQPCClockAnchorsToCaptureTime(t *testing.T) {
	c := qpcClock{
		epoch: time.Now(),
		nowFn: func() time.Duration { return 5 * time.Second },
		qpcFn: func() (uint64, bool) { return 1_000_000, true },
	}

	// 首采样比读包时刻早 10 ms：100_000 x 100 ns。
	if got, want := c.at(900_000, 10*time.Millisecond), 4990*time.Millisecond; got != want {
		t.Errorf("first PTS=%v, want %v", got, want)
	}
	if got, want := c.at(1_000_000, 10*time.Millisecond), 5*time.Second; got != want {
		t.Errorf("second PTS=%v, want %v", got, want)
	}
}

func TestQPCClockClampsBackwardTimestamps(t *testing.T) {
	c := qpcClock{
		nowFn: func() time.Duration { return 0 },
		qpcFn: func() (uint64, bool) { return 0, false },
	}
	if got := c.at(100, 10*time.Millisecond); got != 0 {
		t.Errorf("first PTS=%v, want 0", got)
	}
	// qpc 只前进了 5 ms，但这个包有 10 ms 数据：不能与上一包重叠。
	if got, want := c.at(150, 10*time.Millisecond), 10*time.Millisecond; got != want {
		t.Errorf("overlapping PTS=%v, want %v", got, want)
	}
	// qpc 倒退时保持单调。
	if got, want := c.at(90, 10*time.Millisecond), 20*time.Millisecond; got != want {
		t.Errorf("backward PTS=%v, want %v", got, want)
	}
}

func TestQPCClockWithoutQPCFallsBackToDuration(t *testing.T) {
	c := qpcClock{
		nowFn: func() time.Duration { return 2 * time.Second },
		qpcFn: func() (uint64, bool) { return 0, false },
	}
	if got := c.byDuration(10 * time.Millisecond); got != 2*time.Second {
		t.Errorf("first PTS=%v, want 2s", got)
	}
	if got := c.byDuration(10 * time.Millisecond); got != 2010*time.Millisecond {
		t.Errorf("second PTS=%v, want 2.01s", got)
	}
}

func TestQPCClockIgnoresQPCBeforeAnchor(t *testing.T) {
	c := qpcClock{
		nowFn: func() time.Duration { return time.Second },
		qpcFn: func() (uint64, bool) { return 500_000, true }, // 早于包时间戳：不可信
	}
	if got := c.at(600_000, 10*time.Millisecond); got != time.Second {
		t.Errorf("PTS=%v, want the read time (1s)", got)
	}
}

func TestDeviceLostErrors(t *testing.T) {
	code := uint32(audioClientDeviceInvalidated)
	hr := int32(code)
	err := wasapiError("GetBuffer", hr)
	if !errors.Is(err, ErrDeviceLost) {
		t.Errorf("wasapiError(%#x)=%v, want an ErrDeviceLost wrapper", code, err)
	}

	other := wasapiError("GetBuffer", -1)
	if errors.Is(other, ErrDeviceLost) {
		t.Errorf("wasapiError(-1)=%v, unexpectedly wraps ErrDeviceLost", other)
	}
}
