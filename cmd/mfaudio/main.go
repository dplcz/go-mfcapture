package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"mfcapture"
	"mfcapture/audio"
)

func main() {
	duration := flag.Duration("duration", 5*time.Second, "capture duration")
	output := flag.String("o", "out.opus", "output .opus path")
	bitrate := flag.Int("bitrate", 96_000, "target bitrate in bits per second")
	channels := flag.Int("channels", 2, "capture channels (1 or 2)")
	dtx := flag.Bool("dtx", false, "enable Opus DTX for long silences")
	flag.Parse()

	if err := run(*duration, *output, *bitrate, *channels, *dtx); err != nil {
		fmt.Fprintln(os.Stderr, "audio capture failed:", err)
		os.Exit(1)
	}
}

func run(duration time.Duration, output string, bitrate, channels int, dtx bool) error {
	f, err := os.Create(output)
	if err != nil {
		return err
	}
	defer f.Close()

	bw := bufio.NewWriterSize(f, 64*1024)
	defer bw.Flush()

	sink := &fileSink{w: bw, serial: uint32(time.Now().UnixNano())}
	p, err := mfcapture.NewAudioPipeline(mfcapture.AudioPipelineConfig{
		Epoch:      time.Now(),
		BitrateBps: bitrate,
		Channels:   channels,
		DTX:        dtx,
	}, sink)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 采集端点打开需要一百多毫秒；从 AudioDescReady 起计时，-duration 才是
	// 实际录到的音频长度。
	sink.onReady = func() { time.AfterFunc(duration, cancel) }

	runErr := p.Run(ctx)
	p.Close()
	closeErr := sink.Close()

	if runErr != nil && !errors.Is(runErr, context.DeadlineExceeded) &&
		!errors.Is(runErr, context.Canceled) {
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}
	if sink.err != nil {
		return sink.err
	}
	if err := bw.Flush(); err != nil {
		return err
	}

	stats := p.Stats()
	fmt.Printf("wrote %d packets / %d bytes (%.1f kbps avg) to %s\n",
		sink.packets, stats.Bytes, averageKbps(stats.Bytes, duration), output)
	fmt.Printf("  desc: %d Hz, %d ch, %d ms/frame, pre-skip %d samples\n",
		sink.desc.SampleRate, sink.desc.Channels, sink.desc.FrameMs, sink.desc.PreSkip)
	fmt.Printf("  silence frames: %d, dropped samples: %d, encode: %d us/frame\n",
		stats.SilentFrames, stats.Dropped, stats.EncodeUs)
	return nil
}

func averageKbps(bytes uint64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds() / 1000
}

// fileSink 把编码帧写进 Ogg Opus 文件。回调全部发生在 AudioPipeline.Run
// 所在线程，因此不需要加锁。
type fileSink struct {
	w      *bufio.Writer
	serial uint32
	ogg    *oggOpusWriter
	desc   mfcapture.AudioDesc

	packets int
	err     error
	// onReady 在描述就绪、真正开始录制时回调一次，用来启动 -duration 计时。
	onReady func()
}

func (s *fileSink) OnAudioEvent(e mfcapture.AudioEvent) {
	switch e.Kind {
	case mfcapture.AudioDescReady:
		s.start(e.Desc)
	case mfcapture.AudioDeviceChanged:
		if e.Err != nil {
			// 管线会继续退避重试；失败细节只打印，不改变输出格式。
			fmt.Fprintln(os.Stderr, "mfaudio: reopening capture device:", e.Err)
			return
		}
		if s.ogg == nil {
			s.start(e.Desc)
			return
		}
		if e.Desc.Channels != s.desc.Channels {
			// Ogg 头已经写出，无法在不破坏已写数据的情况下更换声道数。
			fmt.Fprintf(os.Stderr,
				"mfaudio: device now reports %d channels, but the Ogg header says %d; "+
					"restart the capture to switch layout\n", e.Desc.Channels, s.desc.Channels)
		}
	case mfcapture.AudioCaptureError:
		fmt.Fprintln(os.Stderr, "mfaudio: capture error:", e.Err)
	}
}

func (s *fileSink) start(d mfcapture.AudioDesc) {
	if s.ogg != nil || s.err != nil {
		return
	}
	ogg, err := newOggOpusWriter(s.w, d.Channels, d.PreSkip, s.serial)
	if err != nil {
		s.err = err
		return
	}
	s.ogg = ogg
	s.desc = d
	if s.onReady != nil {
		s.onReady()
	}
}

func (s *fileSink) OnAudioFrame(f *audio.EncodedFrame) {
	if s.err != nil || s.ogg == nil {
		return
	}
	samples := int(f.Duration * audio.OpusSampleRate / time.Second)
	if err := s.ogg.writePacket(f.Data, samples); err != nil {
		s.err = err
		return
	}
	s.packets++
}

func (s *fileSink) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.ogg == nil {
		return nil
	}
	return s.ogg.Close()
}
