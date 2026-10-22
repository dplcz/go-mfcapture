// Command mfav 同时采集屏幕与系统声音，把视频写成 Annex-B H264、把音频写成
// Ogg Opus，用来做 P6 的音画同步与长稳验证：
//
//	go run ./cmd/mfav -duration 10s -o-video av.h264 -o-audio av.opus
//	go run ./cmd/mfav -duration 1h -stats 1m -o-video long.h264 -o-audio long.opus
//	go run ./cmd/mfav -no-video -duration 5s          # 只录声音
//	go run ./cmd/mfav -no-audio -duration 5s          # 只录画面
//
// 两条管线共享同一个 Epoch，因此两边输出的 PTS 可以直接比较。把 av.h264 交给
// go-mfplay 的 viewer、av.opus 交给 oggplay 播放，屏幕上拍手/秒表的画面与声音
// 应对得上；结尾打印的首/末帧 PTS 用来排除时间线本身错位的可能。
//
// 采集与编码在两个 goroutine 上并行运行，各自绑定 OS 线程并初始化 MTA。
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"mfcapture"
	"mfcapture/audio"
	"mfcapture/capture"
	"mfcapture/encode"
	"mfcapture/internal/oggopus"
)

type config struct {
	duration     time.Duration
	videoPath    string
	audioPath    string
	monitor      uint
	fps          uint
	videoBitrate uint
	audioBitrate int
	channels     int
	statsEvery   time.Duration
	noVideo      bool
	noAudio      bool
}

func main() {
	var cfg config
	flag.DurationVar(&cfg.duration, "duration", 10*time.Second, "recording duration")
	flag.StringVar(&cfg.videoPath, "o-video", "out.h264", "raw H264 Annex-B output path")
	flag.StringVar(&cfg.audioPath, "o-audio", "out.opus", "Ogg Opus output path")
	flag.UintVar(&cfg.monitor, "monitor", 0, "monitor index")
	flag.UintVar(&cfg.fps, "fps", 30, "video frame rate cap")
	flag.UintVar(&cfg.videoBitrate, "bitrate-video", 8_000_000, "video bitrate in bits per second")
	flag.IntVar(&cfg.audioBitrate, "bitrate-audio", 96_000, "audio bitrate in bits per second")
	flag.IntVar(&cfg.channels, "channels", 2, "audio capture channels (1 or 2)")
	flag.DurationVar(&cfg.statsEvery, "stats", 5*time.Second, "stats log interval (0 = off)")
	flag.BoolVar(&cfg.noVideo, "no-video", false, "disable video capture")
	flag.BoolVar(&cfg.noAudio, "no-audio", false, "disable audio capture")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "mfav failed:", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if cfg.noVideo && cfg.noAudio {
		return errors.New("both -no-video and -no-audio were given")
	}
	if cfg.duration <= 0 {
		return fmt.Errorf("duration %v, want a positive value", cfg.duration)
	}

	epoch := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var video *mfcapture.Pipeline
	var audioPipe *mfcapture.AudioPipeline
	var vsink *videoSink
	var asink *audioSink

	if !cfg.noVideo {
		f, err := os.Create(cfg.videoPath)
		if err != nil {
			return err
		}
		defer f.Close()
		vsink = &videoSink{w: bufio.NewWriterSize(f, 1<<20)}

		video, err = mfcapture.NewPipeline(mfcapture.PipelineConfig{
			Output:     cfg.monitor,
			BitrateBps: uint32(cfg.videoBitrate),
			MaxFPS:     uint32(cfg.fps),
			LowLatency: true,
			Epoch:      epoch,
		}, vsink)
		if err != nil {
			return fmt.Errorf("NewPipeline: %w", err)
		}
		defer video.Close()
	}

	if !cfg.noAudio {
		f, err := os.Create(cfg.audioPath)
		if err != nil {
			return err
		}
		defer f.Close()
		asink = &audioSink{
			w:      bufio.NewWriterSize(f, 256<<10),
			serial: uint32(time.Now().UnixNano()),
		}

		audioPipe, err = mfcapture.NewAudioPipeline(mfcapture.AudioPipelineConfig{
			Epoch:      epoch,
			BitrateBps: cfg.audioBitrate,
			Channels:   cfg.channels,
		}, asink)
		if err != nil {
			return fmt.Errorf("NewAudioPipeline: %w", err)
		}
		defer audioPipe.Close()
	}

	// 两条管线并发启动。视频侧建硬件编码器明显更慢，音频会在视频首帧之前就
	// 产出一些包；audioSink 把这些"预卷"丢掉，让两个文件都从视频首帧的时间点
	// 开始，分开放进两个播放器也能直接对齐。
	clock := &videoClock{}
	readyChans := make([]chan struct{}, 0, 2)
	if vsink != nil {
		vsink.clock = clock
		vsink.ready = make(chan struct{}, 1)
		readyChans = append(readyChans, vsink.ready)
	}
	if asink != nil {
		if vsink != nil {
			asink.clock = clock // 只有录视频时才需要按视频首帧裁预卷
		}
		asink.ready = make(chan struct{}, 1)
		readyChans = append(readyChans, asink.ready)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := func(name string, runFn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := runFn(ctx)
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				cancel() // 一条管线硬失败时不要拖着另一条继续跑
				errCh <- fmt.Errorf("%s: %w", name, err)
				return
			}
			errCh <- nil
		}()
	}
	if video != nil {
		start("video", video.Run)
	}
	if audioPipe != nil {
		start("audio", audioPipe.Run)
	}

	// 两条管线都产出第一帧之后才开始计时：打开设备、建编码器、等首个关键帧
	// 的时间因机器而异，不该算进 -duration。
	go func() {
		for _, ch := range readyChans {
			select {
			case <-ch:
			case <-ctx.Done():
				return
			}
		}
		timer := time.NewTimer(cfg.duration)
		defer timer.Stop()
		select {
		case <-timer.C:
			cancel()
		case <-ctx.Done():
		}
	}()

	statsDone := make(chan struct{})
	if cfg.statsEvery > 0 {
		go func() {
			defer close(statsDone)
			ticker := time.NewTicker(cfg.statsEvery)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					printStats(video, audioPipe)
				}
			}
		}()
	} else {
		close(statsDone)
	}

	wg.Wait()
	<-statsDone
	close(errCh)

	var runErr error
	for err := range errCh {
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			runErr = errors.Join(runErr, err)
		}
	}

	var closeErr error
	if vsink != nil {
		closeErr = errors.Join(closeErr, vsink.Close())
	}
	if asink != nil {
		closeErr = errors.Join(closeErr, asink.Close())
	}
	if runErr != nil || closeErr != nil {
		return errors.Join(runErr, closeErr)
	}

	printSummary(cfg, video, audioPipe, vsink, asink)
	return nil
}

func printStats(video *mfcapture.Pipeline, audioPipe *mfcapture.AudioPipeline) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	if video != nil {
		s := video.Stats()
		fmt.Fprintf(os.Stderr, "mfav: video %d frames (%d key), %.1f MB, %d us/frame, hw=%v\n",
			s.Frames, s.KeyFrames, float64(s.Bytes)/(1<<20), s.EncodeUs, s.IsHardware)
	}
	if audioPipe != nil {
		s := audioPipe.Stats()
		fmt.Fprintf(os.Stderr, "mfav: audio %d frames, %.1f kB, %.1f kbps, dropped %d, silence %d, %d us/frame\n",
			s.Frames, float64(s.Bytes)/1000, float64(s.ActualBps)/1000,
			s.Dropped, s.SilentFrames, s.EncodeUs)
	}
	fmt.Fprintf(os.Stderr, "mfav: heap %.1f MB, goroutines %d\n",
		float64(mem.HeapAlloc)/(1<<20), runtime.NumGoroutine())
}

func printSummary(cfg config, video *mfcapture.Pipeline, audioPipe *mfcapture.AudioPipeline,
	vs *videoSink, as *audioSink) {
	fmt.Printf("epoch: both pipelines used the same origin\n")
	if video != nil {
		s := video.Stats()
		fmt.Printf("video: %d frames, %d bytes, first PTS %v, last PTS %v, hw=%v -> %s\n",
			s.Frames, s.Bytes, vs.firstPTS, vs.lastPTS, s.IsHardware, cfg.videoPath)
	}
	if audioPipe != nil {
		s := audioPipe.Stats()
		fmt.Printf("audio: %d frames, %d bytes, first PTS %v, last PTS %v, dropped %d, silence %d, pre-roll %d -> %s\n",
			s.Frames, s.Bytes, as.firstPTS, as.lastPTS, s.Dropped, s.SilentFrames, as.preRoll, cfg.audioPath)
	}
	if video != nil && audioPipe != nil {
		// 音频在视频首帧之前的包已经丢弃，两个文件从同一时间点开始；首帧差
		// 应落在一个音频帧（20 ms）加一个视频帧之内。
		fmt.Printf("startup skew: video-audio first PTS = %v (audio pre-roll trimmed)\n",
			vs.firstPTS-as.firstPTS)
		if vs.frames > 0 && as.frames > 0 {
			fmt.Printf("tail skew: video-audio last PTS = %v\n", vs.lastPTS-as.lastPTS)
		}
	}
}

// videoClock 把视频首帧的 PTS 发布给音频 sink，两条管线在各自的 goroutine
// 上通过原子变量交接。
type videoClock struct {
	hasFirst atomic.Bool
	firstPTS atomic.Int64
}

func (c *videoClock) mark(ts time.Duration) {
	c.firstPTS.Store(int64(ts))
	c.hasFirst.Store(true)
}

func (c *videoClock) start() (time.Duration, bool) {
	if !c.hasFirst.Load() {
		return 0, false
	}
	return time.Duration(c.firstPTS.Load()), true
}

// videoSink 把 Annex-B 视频帧写文件，并记录首/末 PTS。回调都在视频管线
// 所在线程，不需要加锁。
type videoSink struct {
	w     *bufio.Writer
	err   error
	ready chan struct{}
	clock *videoClock

	frames   int
	firstPTS time.Duration
	lastPTS  time.Duration
	wrotePTS bool
}

func (s *videoSink) OnFrame(f *encode.EncodedFrame) {
	if s.err != nil {
		return
	}
	if _, err := s.w.Write(f.Data); err != nil {
		s.err = err
		return
	}
	s.frames++
	if !s.wrotePTS {
		s.firstPTS = f.Timestamp
		s.wrotePTS = true
		if s.clock != nil {
			s.clock.mark(f.Timestamp)
		}
		notifyReady(s.ready)
	}
	s.lastPTS = f.Timestamp
}

func (s *videoSink) OnCursor(*capture.CursorInfo) {}

func (s *videoSink) OnEvent(e mfcapture.Event) {
	switch e.Kind {
	case mfcapture.EventDescReady:
		fmt.Fprintf(os.Stderr, "mfav: video %dx%d fps=%d/%d\n", e.Desc.Width, e.Desc.Height,
			e.Desc.FPSNum, e.Desc.FPSDen)
	case mfcapture.EventEncoderDegraded:
		fmt.Fprintln(os.Stderr, "mfav: warning: using software H264 encoder")
	case mfcapture.EventCaptureError:
		fmt.Fprintln(os.Stderr, "mfav: video capture error:", e.Err)
	}
}

func (s *videoSink) Close() error {
	if s.err != nil {
		return s.err
	}
	return s.w.Flush()
}

// audioSink 把编码帧写进 Ogg Opus，并记录首/末 PTS。视频首帧之前产生的包
// 会被丢掉，保证两个文件从同一个时间点开始。
type audioSink struct {
	w      *bufio.Writer
	serial uint32
	ogg    *oggopus.Writer
	desc   mfcapture.AudioDesc
	ready  chan struct{}
	clock  *videoClock

	err      error
	frames   int
	preRoll  int // 视频首帧之前被丢弃的音频包
	started  bool
	firstPTS time.Duration
	lastPTS  time.Duration
	wrotePTS bool
}

func (s *audioSink) OnAudioFrame(f *audio.EncodedFrame) {
	if s.err != nil || s.ogg == nil {
		return
	}
	if !s.started {
		if s.clock != nil {
			start, ok := s.clock.start()
			if !ok || f.Timestamp < start {
				s.preRoll++
				return
			}
		}
		s.started = true
	}
	samples := int(f.Duration * audio.OpusSampleRate / time.Second)
	if err := s.ogg.WritePacket(f.Data, samples); err != nil {
		s.err = err
		return
	}
	s.frames++
	if !s.wrotePTS {
		s.firstPTS = f.Timestamp
		s.wrotePTS = true
		notifyReady(s.ready)
	}
	s.lastPTS = f.Timestamp
}

func (s *audioSink) OnAudioEvent(e mfcapture.AudioEvent) {
	switch e.Kind {
	case mfcapture.AudioDescReady:
		s.start(e.Desc)
	case mfcapture.AudioDeviceChanged:
		if e.Err != nil {
			fmt.Fprintln(os.Stderr, "mfav: reopening audio device:", e.Err)
			return
		}
		if s.ogg == nil {
			s.start(e.Desc)
			return
		}
		if e.Desc.Channels != s.desc.Channels {
			fmt.Fprintf(os.Stderr,
				"mfav: audio device now reports %d channels, but the Ogg header says %d\n",
				e.Desc.Channels, s.desc.Channels)
		}
	case mfcapture.AudioCaptureError:
		fmt.Fprintln(os.Stderr, "mfav: audio capture error:", e.Err)
	case mfcapture.AudioSilenceDegraded:
		fmt.Fprintln(os.Stderr, "mfav: audio has been silent for a while (check the endpoint)")
	}
}

func (s *audioSink) start(d mfcapture.AudioDesc) {
	if s.ogg != nil || s.err != nil {
		return
	}
	ogg, err := oggopus.NewWriter(s.w, d.Channels, d.PreSkip, s.serial)
	if err != nil {
		s.err = err
		return
	}
	s.ogg = ogg
	s.desc = d
	fmt.Fprintf(os.Stderr, "mfav: audio %d Hz, %d ch, pre-skip %d\n",
		d.SampleRate, d.Channels, d.PreSkip)
}

func (s *audioSink) Close() error {
	if s.err != nil {
		return s.err
	}
	if s.ogg != nil {
		if err := s.ogg.Close(); err != nil {
			return err
		}
	}
	return s.w.Flush()
}

func notifyReady(ch chan<- struct{}) {
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default: // 同一路重复上报 DescReady 时不要阻塞采集线程
	}
}
