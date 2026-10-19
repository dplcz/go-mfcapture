# 在 mfcapture 中加入 Opus 音频编码

本文给出选型结论、性能数据和落地设计。性能数据来自
[`bench/opusbench`](bench/opusbench/README.md)，可复现。

## 1. 结论

1. **编码器选 `github.com/tphakala/go-opus`**（v1.2.1，纯 Go、无 cgo）。
   在三个候选里最快：比 `pion/opus` 快 25%~60%，比 WASM 版 libopus
   （`meokit/opus`）快 50%~90%，且稳态零分配。pion 的 Encoder 目前只存在
   于 main 分支、未随 tag 发布。
2. **采集用 WASAPI loopback**（系统声音），请求 48 kHz / stereo / float32。
   Opus 的编码采样率固定 48 kHz，三个候选库的公开编码入口都以此为主。
3. **固定 20 ms 一包**（960 采样/声道），码率 96~128 kbps、Constrained VBR、
   complexity 5 作为默认；复杂度 10 作为"高质量"选项。
4. **新增独立的 `AudioPipeline`，不要塞进现有视频 `Pipeline`**。音频的
   COM 线程、节拍、丢帧策略和视频完全不同；两者只在应用层用同一个 epoch
   对齐时间戳。
5. **依赖代价**：go-opus v1.2.x 要求 Go 1.27（主模块需从 1.26.4 升级）。
   若暂时不能升级，锁 **v1.1.0**——它是最后一个兼容 Go 1.26 的版本，公开
   API 与 v1.2.1 相同；实测 stereo 128k c5/c10 约 200/281 µs，仍比 pion
   快约 25%/22%，只是比 v1.2.1 慢 10%~15%。
6. **`meokit/opus` 作为备选**：它是唯一提供完整 libopus 编码能力的候选
   （SILK/hybrid、in-band FEC、运行时 bitrate），但 WASM 边界每帧 12 次
   分配，且所有实例共享一个无锁的 WASM 运行时。只有出现"低码率语音 /
   FEC 刚需、并发路数少"时再切换。

## 2. 当前项目现状

现有链路只处理视频：

```
DXGI Desktop Duplication -> D3D11 texture -> IMFSample
  -> H264 MFT -> EncodedFrame -> Sink.OnFrame
```

关键约定（新音频代码应保持一致）：

- 平台相关代码放在子包（`capture/`、`encode/`），编排放在根包
  （`pipeline.go`），上层通过接口注入。
- `Pipeline.Run` 用 `runtime.LockOSThread()` 绑定线程，COM 初始化、使用、
  释放全在同一线程内完成。
- 下层不做队列、不丢帧；`Sink.OnFrame` 同步调用，阻塞策略由上层决定。
- 控制命令（关键帧、码率）通过命令队列投递到 Run 线程，不跨线程碰 COM。
- 纯 Go、`CGO_ENABLED=0`；COM 通过手写 vtable + `syscall.SyscallN` 绑定。

## 3. 选型数据

测试条件：48 kHz / 20 ms，多音 + 噪声素材；i7-9700；Go 1.27；CGO 关闭。
单帧实时预算是 20 000 µs。

| 配置 | go-opus c5 | pion c5 | meokit c5 | go-opus c10 | pion c10 | meokit c10 |
|------|-----------:|--------:|----------:|------------:|---------:|-----------:|
| mono 24k CBR | **98.7 µs** | 130.4 µs | 163.1 µs | **97.5 µs** | 134.4 µs | 223.3 µs |
| mono 64k CBR | **118.2 µs** | 171.8 µs | 194.7 µs | **125.6 µs** | 173.0 µs | 252.8 µs |
| stereo 64k CBR | **158.3 µs** | 209.6 µs | 245.9 µs | **207.2 µs** | 258.3 µs | 367.4 µs |
| stereo 128k CBR | **173.3 µs** | 273.9 µs | 261.3 µs | **264.0 µs** | 364.0 µs | 439.1 µs |
| stereo 128k CVBR | **174.1 µs** | 259.6 µs | 263.7 µs | **252.5 µs** | 372.1 µs | 435.4 µs |
| stereo 128k float32 c10 | — | — | — | **293.4 µs** | 357.0 µs | 501.3 µs |

- go-opus 与 pion 稳态都是 **0 allocs/op**；meokit 每帧 12 次分配、
  约 6.3 KB（WASM 边界拷贝）。
- go-opus 新建编码器 **3.9 µs / 14.9 KB**；pion **32.6 µs / 156 KB**；
  meokit 首次构造包含 WASM 编译（毫秒级），之后复用全局实例。
- 最重的常用配置（stereo 128k c5）go-opus 占单核约 **0.9%**，meokit
  约 1.3%；即使 meokit c10 也只有 2.2%。编码不需要独立线程，但绝不能
  阻塞 WASAPI 的采集/事件线程。
- 完整方法、局限和复现命令见 `bench/opusbench/README.md`。

### 3.1 真实 WAV 素材复核

用 ffmpeg 生成 48 kHz / 5 s / PCM16 的四份素材（单音、合成语音、音乐混合、
去相关噪声），编码 + 解码完整跑一遍（CVBR，c5；语音 24k，其余 mono 64k /
stereo 128k）：

| 素材 | 编码 go/pion/meokit | 解码 go/pion/meokit | 往返 SNR go/pion/meokit |
|------|--------------------:|--------------------:|------------------------:|
| tone 64k | 78.7 / 162.8 / 190.1 µs | 38.2 / 43.4 / 62.6 µs | 43.8 / 41.2 / 40.4 dB |
| speech 24k | 98.3 / 131.7 / 167.8 µs | 34.9 / 35.4 / 58.8 µs | 18.3 / 18.2 / 18.6 dB |
| music 128k | 165.9 / 250.1 / 262.9 µs | 74.3 / 78.2 / 113.9 µs | 27.5 / 28.1 / 27.9 dB |
| noise 128k | 172.3 / 248.0 / 265.3 µs | 73.6 / 77.1 / 111.5 µs | 1.0 / 1.0 / 1.0 dB |

- 编码结论不变：真实内容下 go-opus 比 pion 快 1.3~2.1 倍，比 meokit 快
  1.5~1.7 倍（24k 语音 c10 例外，meokit 604 µs）。解码快 2%~13% / 50%~65%。
- **3×3 交叉解码全部成功**，同一码流用不同解码器得到的 SNR 一致，三个
  实现的 Opus 码流可以互操作。
- 24k 语音上 meokit（完整 libopus）只比纯 CELT 实现高 0.3~0.4 dB；
  波形 SNR 不是语音质量指标，SILK 的收益更多在主观听感和 FEC。
- 噪声素材的 1 dB 是正常现象（128 kbps 只有 0.67 bit/采样），它用于压
  性能上界，不作为音质门槛。
- **pre-skip 因实现而异**：go-opus / meokit 为 312 采样，pion 实测 120 采样。

## 4. 三个库的 API 差异

| 维度 | tphakala/go-opus | pion/opus | meokit/opus |
|------|------------------|-----------|-------------|
| 实现 | libopus 1.6.1 的逐行 Go 转写，依赖 `tphakala/simd` | 纯 Go 实现 | libopus 编译为 WASM，wazero 执行 |
| 版本状态 | v1.2.1（tagged） | Encoder 仅在 main，需 pin commit | v1.0.1（仓库 meokit/opus，模块路径 `github.com/jj11hh/opus`） |
| 编码模式 | CELT-only | CELT-only | **完整 libopus**（SILK/CELT/hybrid） |
| 采样率 | 8/12/16/24/48 kHz | 48 kHz | 8/12/16/24/48 kHz |
| 帧长 | 2.5~120 ms | 固定 20 ms | 2.5~60 ms |
| 输入 | `Encode([]int16, []byte)` | `Encode`（s16le `[]byte`）/ `EncodeFloat32` | `Encode([]int16, []byte)` / `EncodeFloat32` |
| 码率控制 | CBR / VBR / Constrained VBR、complexity 0~10、DTX | CBR/VBR、complexity、loss rate | bitrate/complexity/VBR/CVBR/DTX/FEC/loss%/bandwidth |
| 运行时改码率 | **无**（只能重建 encoder） | `SetBitrate` | `SetBitrate` |
| lookahead / pre-skip | 312 采样（6.5 ms） | 120 采样（2.5 ms，实测） | 312 采样（6.5 ms） |
| 编码吞吐（c5） | **最快（基准）** | 慢 25%~60% | 慢 50%~90% |
| 解码吞吐 | **最快（基准）** | 慢 3%~12% | 慢约 50%，且 11 allocs/包 |
| 稳态分配 | **0 allocs/op** | **0 allocs/op** | 12 allocs/编码帧、11 allocs/解码包 |
| 并发 | 每 goroutine 一个 encoder 即可 | 同左 | 共享 WASM 实例且无内部锁，需应用层串行化 |
| Go 要求 | v1.2.x: 1.27；v1.1.0: 1.26 | 1.24 | 1.22 |
| 许可 | BSD-3-Clause（libopus 派生） | MIT | MIT wrapper + libopus BSD |

编码调用形态：

```go
// go-opus
enc, _ := opus.NewEncoder(opus.EncoderConfig{
    SampleRate: 48000, Channels: 2, Bitrate: 128000,
    Complexity: 5, ConstrainedVBR: true,
})
n, err := enc.Encode(pcmInt16, outBuf) // 20 ms = 960*ch 个 int16

// pion/opus
enc, _ := pionopus.NewEncoder(
    pionopus.WithSampleRate(48000),
    pionopus.WithChannels(2),
    pionopus.WithBitrate(128000),
    pionopus.WithComplexity(5),
    pionopus.WithVBR(true),
    pionopus.WithConstrainedVBR(true),
)
n, err := enc.Encode(pcmS16LE, outBuf)

// meokit/opus（模块路径 github.com/jj11hh/opus）
enc, _ := opus.NewEncoder(48000, 2, opus.AppAudio)
enc.SetBitrate(128000)
enc.SetComplexity(5)
enc.SetVBR(true)
enc.SetVBRConstraint(true)
n, err := enc.Encode(pcmInt16, outBuf)
```

**选 go-opus 的代价**：没有运行时 `SetBitrate`。但重建只要 ~4 µs，实现上
可以"投递新码率 → 重建 encoder → 丢一帧"，实际听感是一次 20 ms 的状态
重置，可接受。若产品要求码率每 100 ms 平滑调整且不能有任何状态重置，
再考虑 pion 或 meokit。

**meokit/opus 的取舍**：它是三家里唯一提供完整 libopus 编码能力的
（SILK/hybrid、in-band FEC、DTX、运行时 bitrate）。代价是 WASM 边界拷贝
（每帧 12 次分配）、共享 WASM 实例无内部锁（多路必须串行）、首次调用要
编译 WASM。适合"需要低码率语音/FEC、但并发路数不多"的场景；当前项目
48 kHz 立体声桌面音频、每进程一两路，go-opus 更合适。

## 5. 目标结构

所有音频逻辑都在 `audio/` 包内，不往 `encode/`、`decode/` 里塞——那是视频的包。
包内沿用项目的文件约定：`capturer.go`/`encoder.go`/`decoder.go` 放接口与帧类型，
`duplication.go`/`h264_mft.go` 那样的实现文件按技术命名。

```
audio/
  opus.go              包级常量：48 kHz / 20 ms / 帧长 / 包上限 / pre-skip 兜底
  encoder.go           Encoder 接口 + EncodedFrame
  decoder.go           Decoder 接口 + DecodedFrame
  opus_encoder.go      OpusEncoderConfig + OpusEncoder（go-opus 适配）
  opus_decoder.go      OpusDecoderConfig + OpusDecoder（PLC + FEC + PreSkip）
  format.go            Format / Frame / PCM→float32 转换      ← P2
  capturer.go          Capturer 接口 + ErrTimeout             ← P2
  ring.go              Ring：固定容量 PCM 字节 FIFO            ← P2
  framer.go            Framer：20 ms 分帧 / 静音 / 积压丢弃     ← P2
  wasapi.go            WASAPI loopback 实现（纯 Go COM 绑定）  ← P3
  wasapi_types.go      WAVEFORMATEX(EXTENSIBLE)、GUID、vtable ← P3
  resample.go          非 48 kHz 时的兜底重采样（可后置）
audio_pipeline.go      根包：AudioPipeline（采集→分帧→编码→Sink）
audio_decode_session.go 根包：AudioDecodeSession（接收端：Opus 包→PCM→Sink）
cmd/mfaudio/           CLI：录 loopback 到 Ogg Opus / 原始包，人工验收
```

分层与视频侧完全对称（发送端 + 接收端）：

| 视频 | 音频（发送端） | 音频（接收端） |
|------|--------------|--------------|
| `capture.Capturer` | `audio.Capturer` | — |
| `encode.H264MFTEncoder` | `audio.OpusEncoder` | `audio.OpusDecoder` |
| `Pipeline`（根包） | `AudioPipeline`（根包） | `AudioDecodeSession`（根包） |
| `Sink.OnFrame/OnCursor/OnEvent` | `AudioSink.OnAudioFrame/OnAudioEvent` | `AudioDecodeSink.OnPCMFrame/OnAudioEvent` |

命名不重复包名：包已经是 `audio`，所以类型是 `audio.Encoder` / `audio.Decoder` /
`audio.EncodedFrame` / `audio.DecodedFrame`，不写 `audio.AudioEncoder` 这种结巴
形式；实现类型保留技术前缀，与 `encode.H264MFTEncoder` 一致。

不把音频塞进现有 `PipelineConfig` 的原因：

- 视频 `Run` 的线程被 DXGI/MFT 占用；WASAPI 需要自己的 COM 线程。
- 音频是恒定 50 pps，视频是变化帧率；两者的丢帧/限流策略不同。
- 应用层本来就需要把两路流分别复用进传输层。

## 6. 核心类型

```go
// audio/format.go
type Format struct {
    SampleRate int  // 48000
    Channels   int  // 1 或 2
    Float32    bool // true: F32LE；false: S16LE
}
func (f Format) Validate() error
func (f Format) BytesPerSample() int
func (f Format) BlockAlign() int                       // 一个采样帧的字节数
func (f Format) Samples(data []byte) int               // data 包含的采样帧数
func (f Format) FrameBytes(sampleFrames int) int
func (f Format) ToFloat32(dst []float32, data []byte) (int, error)

type Frame struct {
    Data      []byte
    Timestamp time.Duration // Capturer 已换算成相对 epoch 的 PTS
    Duration  time.Duration
    Silent    bool          // 采集源补的静音；Data 仍是正确长度的全零 PCM
}

// audio/capturer.go
var ErrTimeout = errors.New("audio: capture timed out")

type Capturer interface {
    Format() Format
    // ReadFrame 最多等待 timeout；无数据时返回 (nil, ErrTimeout)。
    // 返回的 Frame.Data 只在下一次 ReadFrame 之前有效。
    ReadFrame(timeout time.Duration) (*Frame, error)
    Release()
}
```

**P2 决策**：`Frame` 保留设计里的 `[]byte`（F32LE/S16LE 由 `Format.Float32`
区分），采集层可以零拷贝交出 WASAPI 缓冲；编码前用 `Format.ToFloat32`
一次性转成 `[]float32`。时间戳由 Capturer 负责换算成 epoch 相对 PTS——
WASAPI 实现内部把首个包的 `qpcPosition` 锚定到 epoch，上层不接触 QPC。

### 6.1 Ring 与 Framer（P2）

```go
// audio/ring.go —— 只做字节 FIFO，不解释时间戳
func NewRing(format Format, maxLatency time.Duration) (*Ring, error)
func (r *Ring) Append(data []byte) (droppedSamples int, err error)
func (r *Ring) Available() int                       // 采样帧数
func (r *Ring) Take(sampleFrames int) ([]byte, error) // 别名内部缓冲
func (r *Ring) Drop(sampleFrames int) int
func (r *Ring) Cap() int
func (r *Ring) DroppedSamples() int
func (r *Ring) Reset()

// audio/framer.go —— 分帧、PTS、静音补齐、积压丢弃
type FramerConfig struct {
    Format     Format
    MaxLatency time.Duration // 0 = 200 ms
}
func NewFramer(cfg FramerConfig) (*Framer, error)
func (f *Framer) Append(fr *Frame) error
func (f *Framer) Take() (*Frame, bool)       // 攒满 960 采样才产出
func (f *Framer) Silence() *Frame            // 补一帧 20 ms 静音并推进 PTS
func (f *Framer) Available() int
func (f *Framer) NextPTS() time.Duration
func (f *Framer) DroppedFrames() int
func (f *Framer) Reset()
```

职责边界：`Ring` 只管字节和容量，溢出时丢最旧的整 20 ms 帧；`Framer`
在其上维护 PTS 时间线并产出固定 20 ms 帧。调用方（P4 的 AudioPipeline）
决定何时阻塞读采集源、何时调 `Silence`，因此 P2 的三项验收标准可以脱离
WASAPI 用假 `Capturer` 完整验证。

时间线规则：第一条非空数据锚定 PTS，之后按采样数推进；ring 排空后允许
向前重新对齐到源时钟，但不倒退（补过静音后源时间戳略早时保持单调）。
`Framer` 不要求 48 kHz 以外的格式：非 48 kHz 端点需要先重采样，构造时
直接报错而不是静默产出错误时长的帧。

```go
// audio/opus_encoder.go
type OpusEncoderConfig struct {
    SampleRate int  // 必须 48000
    Channels   int
    BitrateBps int  // 0 = 96000
    Complexity int  // 0 = 5
    CBR        bool // false = Constrained VBR
    DTX        bool // 桌面静音时省码率
}
```

```go
// audio/encoder.go
type EncodedFrame struct {
    Data          []byte        // 一个完整 Opus packet
    Timestamp     time.Duration // 包内第一个采样的 PTS
    Duration      time.Duration // 固定 20 ms
    Discontinuous bool          // DTX / 静音跳帧
}

type Encoder interface {
    // pcm 必须是恰好一个 20 ms 帧（960*Channels 个 float32）。
    Encode(pcm []float32, ts time.Duration) (*EncodedFrame, error)
    SetBitrate(bps int) error // go-opus: 内部重建，丢一帧状态
    Flush() ([]*EncodedFrame, error)
    Release()
}
```

实现要点：

- `Encode` 内部维护 `[]int16` scratch，把 float32 转成 int16 后调用
  go-opus；不每帧分配。转换成本已包含在 benchmark 的 float32 行里。
- 输出缓冲由 encoder 持有。`EncodedFrame.Data` 要不要复制取决于
  Sink 契约：建议在 `AudioPipeline` 里复制一份（20 ms × 128 kbps ≈ 320 B）
  后交给上层，保证"回调返回后数据仍有效"。
- `AudioDesc` 暴露给对端：

```go
type AudioDesc struct {
    Codec      string // "opus"
    SampleRate int    // 48000
    Channels   int
    FrameMs    int // 20
    PreSkip    int // go-opus 48 kHz 下 312；pion 实测 120；按实现取值
}
```

### 6.2 解码侧类型

```go
// audio/decoder.go
type DecodedFrame struct {
    PCM       []float32     // 交错 PCM float32，960*Channels 个采样
    Timestamp time.Duration
    Duration  time.Duration // 固定 20 ms
    PLC       bool          // 本帧由丢包隐藏生成
}

type Decoder interface {
    // Decode 解码一个 Opus 包。data 为 nil 时触发 PLC（丢包隐藏）。
    Decode(data []byte, ts time.Duration) (*DecodedFrame, error)
    // DecodeFEC 尝试从下一个包中恢复前一个丢失帧（前向纠错）。
    DecodeFEC(nextPacket []byte, ts time.Duration) (*DecodedFrame, error)
    Reset() // 流切换或 seek 后重置解码器状态
    Release()
}
```

实现要点：

- `OpusDecoder` 包装 `opus.NewDecoder` + `dec.Decode`，输出 float32 PCM。
- 内部维护 `[]float32` 和 `[]int16` scratch 缓冲，稳态零分配。
- `data == nil` 时调用 go-opus 的 PLC 模式生成补偿帧，`DecodedFrame.PLC = true`。
- **PreSkip 处理**：解码器跟踪已输出采样数，前 `PreSkip` 个采样静默丢弃，
  不交给上层。这与 OggOpus 规范一致，且解码端统一处理避免发送端/接收端各做一半。

### 6.3 AudioDecodeSession

对称于视频侧 `decode.Session`，封装 OS 线程绑定与线程安全投递：

```go
// audio_decode_session.go
type AudioDecodeSink interface {
    OnPCMFrame(f *audio.DecodedFrame)
    OnAudioEvent(e AudioEvent)
}

type AudioDecodeSessionConfig struct {
    Channels   int // 0 = 2
    BufferSize int // Submit 队列深度；0 = 64
}

func NewAudioDecodeSession(cfg AudioDecodeSessionConfig, sink AudioDecodeSink) (*AudioDecodeSession, error)
func (s *AudioDecodeSession) Run(ctx context.Context) error // 阻塞
func (s *AudioDecodeSession) Submit(data []byte, ts time.Duration) // 任意 goroutine
func (s *AudioDecodeSession) SubmitPLC(ts time.Duration)           // 显式请求丢包隐藏
func (s *AudioDecodeSession) Close() error
```

- `Run` 不需要 COM（Opus 解码是纯 Go），但仍用独立 goroutine 保证
  解码和回调在同一线程，避免上层并发问题。
- `Submit` 通过 channel 投递到 Run goroutine，与 `decode.Session` 模式一致。
- 应用层在传输层检测丢包后调用 `SubmitPLC`，Session 内部调 `Decoder.Decode(nil, ts)`。

## 7. WASAPI loopback 采集

### 7.1 接口与初始化

纯 Go 手写 COM 绑定，风格与 `mf/` 一致。需要的最小接口集：

- `IMMDeviceEnumerator::GetDefaultAudioEndpoint(eRender, eConsole)`
- `IMMDevice::Activate(IID_IAudioClient)`
- `IAudioClient`：`GetMixFormat`、`Initialize`、`SetEventHandle`、`Start/Stop`、`GetService`
- `IAudioCaptureClient`：`GetNextPacketSize`、`GetBuffer`、`ReleaseBuffer`

GUID：

| 名称 | 值 |
|------|----|
| CLSID_MMDeviceEnumerator | `BCDE0395-E52F-467C-8E3D-C4579291692E` |
| IID_IMMDeviceEnumerator | `A95664D2-9614-4F35-A746-DE8DB63617E6` |
| IID_IAudioClient | `1CB9AD4C-DBFA-4C32-B178-C2F568A703B2` |
| IID_IAudioCaptureClient | `C8ADBD64-E71E-48A0-A4DE-185C395CD317` |
| KSDATAFORMAT_SUBTYPE_IEEE_FLOAT | `00000003-0000-0010-8000-00AA00389B71` |

初始化流程：

1. `CoInitializeEx(COINIT_MULTITHREADED)`（必须在 `Run` 线程上）。
2. 取默认 **render** 端点，`Activate` 出 `IAudioClient`。
3. 构造目标格式 `WAVEFORMATEXTENSIBLE`：48 kHz、2ch、32-bit float、
   `WAVE_FORMAT_EXTENSIBLE`、`SPEAKER_FRONT_LEFT|RIGHT`。
4. `Initialize(SHARED, LOOPBACK | EVENTCALLBACK | AUTOCONVERTPCM |
   SRC_DEFAULT_QUALITY, 100ms, 0, targetFormat, nil)`。
   - `AUTOCONVERTPCM` 让 Windows 把端点混音格式重采样/下混到 48 kHz
     立体声；端点本来是 44.1 kHz 或 5.1 时不需要自己处理。
   - 若 `Initialize` 拒绝目标格式（老系统/驱动），退回 `GetMixFormat`，
     用 `resample.go` 做兜底转换，并上报 degraded 事件。
5. `CreateEvent` + `SetEventHandle`，`Start`。
6. 退出时 `Stop`、释放所有接口、`CoTaskMemFree(mixFormat)`、关闭事件。

缓冲区给 100~200 ms。WASAPI 引擎大约每 10 ms 唤醒一次，采集包大小通常
是 10 ms 的整数倍；**不要拿到一个包就编一包**，要先累积到 960 采样。

### 7.2 读取循环

```go
for {
    wait := WaitForSingleObject(event, 20 /*ms*/)
    for {
        n, _ := capture.GetNextPacketSize()
        if n == 0 { break }
        data, frames, flags, _, qpc := capture.GetBuffer()
        // flags&AUDCLNT_BUFFERFLAGS_SILENT != 0 时 data 不可读，按 0 填充；
        // PTS 由 qpc 锚定到 epoch（见 §7.3），换算后再交给 Framer。
        pts := epochAnchor + (qpc - firstQPC)
        frame := &Frame{Data: pcm, Timestamp: pts, Duration: durationOf(frames)}
        framer.Append(frame)
        capture.ReleaseBuffer(frames)
    }
    for {
        pcm, ok := framer.Take() // 只在攒满 960 采样时产出
        if !ok { break }
        emit(pcm)
    }
    if wait == timeout && framer.Available() == 0 && silenceMode {
        emit(framer.Silence()) // 保持 50 pps，PTS 继续按 20 ms 推进
    }
}
```

需要明确的策略：

- **静音**：loopback 在没有进程出声时**不会产生任何包**。如果不补静音，
  接收端音频时间线会停住，A/V 同步和抖动缓冲都会出问题。默认开启
  "20 ms 无数据就补一帧静音"，同时推进 PTS。
- **积压**：ring 超过 `MaxLatencyMs`（默认 200 ms）时丢弃最旧的完整帧，
  `Stats.Dropped` 自增。只丢**整 20 ms 帧**，不要切在半帧上。
- **DTX**：开启后 go-opus 对连续数字静音编码 1 字节包。补静音与 DTX
  可以同时用：包率不变，静音期码率接近 0。
- **设备变化**：默认端点被拔掉/切换时，采集会报错。捕获后销毁并重建
  `IAudioClient`，期间持续补静音；不要像视频那样直接把整条管线返回。

### 7.3 时间戳与 A/V 同步

视频侧现在用 `time.Since(start)`（100 ns 单位，QPC 单调时钟）。音频需要
和它共享同一个 epoch：

1. 给 `PipelineConfig` 增加 `Epoch time.Time`（零值 = `time.Now()`），
   `pipeline.go` 里 `start` 改用它。这是向后兼容的小改动。
2. `AudioPipeline` 用同一个 `Epoch`。WASAPI 的 `GetBuffer` 会返回
   `qpcPosition`（首采样的 QPC 时间，100 ns）。把首个包的 qpc 与 epoch
   锚定后，后续帧：

   ```
   pts = firstPTS + (qpc - firstQPC) + (samplesSinceFirst / 48000)
   ```

   优先信 QPC（它和视频时钟同源），用累计采样数补插值；每 10 s 比较一次
   两者，偏差 > 20 ms 时以 QPC 为准修正。
3. 若补了静音帧，PTS 按 `上一帧 PTS + 20 ms` 推进。
4. **PreSkip**：go-opus 在 48 kHz 下是 312 采样（6.5 ms），pion 实测是
   120 采样（2.5 ms，它没有实现 192 采样的 delay-compensation ring）。
   raw packet 传输时 `AudioDesc.PreSkip` 必须由编码器实现给出，**不能写死
   312**。接收端丢弃前 `PreSkip` 个采样，或发送端把音频 PTS 整体减去对应
   时长；二选一，不要两边都做。6.5 ms 对唇音同步通常可以忽略，但切换编码器
   时必须同步更新描述。

## 8. AudioPipeline

```go
type AudioPipelineConfig struct {
    Epoch        time.Time
    BitrateBps   int  // 0 = 96000
    Channels     int  // 0 = 2
    Complexity   int  // 0 = 5
    CBR          bool
    DTX          bool
    Silence      bool // 无数据时补静音，默认 true
    MaxLatencyMs int  // 0 = 200
}

type AudioSink interface {
    OnAudioFrame(f *audio.EncodedFrame)
    OnAudioEvent(e AudioEvent)
}

func NewAudioPipeline(cfg AudioPipelineConfig, sink AudioSink) (*AudioPipeline, error)
func (p *AudioPipeline) Run(ctx context.Context) error // 阻塞；绑定 OS 线程
func (p *AudioPipeline) Desc() AudioDesc
func (p *AudioPipeline) Stats() AudioStats
func (p *AudioPipeline) SetBitrate(bps int) // 命令队列，Run 线程执行
func (p *AudioPipeline) Close() error       // Run 返回后调用
```

- `Run`：`LockOSThread` → `CoInitializeEx` → 打开 loopback → 建 encoder →
  循环 → 清理，全程同线程，和 `Pipeline.Run` 一致。
- 编码在采集线程内联完成。单帧最多约 0.3 ms，相对 20 ms 预算有 60 倍余量；
  如果以后开多路采集，再把编码拆到独立 goroutine，接口不变。
- `Sink.OnAudioFrame` 同步调用；阻塞会让采集 ring 积压并触发丢帧，
  文档里要和视频 Sink 一样写清楚。
- 事件：`AudioDescReady`、`AudioCaptureError`、`AudioDeviceChanged`、
  `AudioSilenceDegraded`（长时间只有静音）。
- 统计：`Frames`、`Bytes`、`EncodeUs`、`Dropped`、`SilentFrames`、
  `ActualBps`（滑动窗口）。`ActualBps` 用于和视频码率一起做拥塞控制。

应用层集成：

```go
epoch := time.Now()
video, _ := mfcapture.NewPipeline(mfcapture.PipelineConfig{Epoch: epoch, ...}, vSink)
audio, _ := mfcapture.NewAudioPipeline(mfcapture.AudioPipelineConfig{Epoch: epoch, ...}, aSink)
go video.Run(ctx)
go audio.Run(ctx)
```

传输层把 `EncodedFrame` 当作"一个 20 ms Opus 包 + PTS"发送即可；
如果走 WebRTC/RTP，直接映射到 Opus/48000/2、`ptime=20`（payload type 111）。

## 9. 文件与容器

- **流式传输**：不需要容器。协议里带 `AudioDesc` + 每包 PTS/duration。
- **本地文件**：用 go-opus 的 `oggopus` 包写标准 `.opus`（含 OpusHead、
  pre-skip、granule position），VLC/ffmpeg 可直接播放。
- 若以后要封装进 MP4/WebM，需要 `dOps`/CodecPrivate；在当前阶段不做。

## 10. 测试与落地步骤

### 10.1 测试清单

1. **已完成的选型证据**：`bench/opusbench`。其中
   `BenchmarkWAVEncode` / `BenchmarkWAVDecode` 用 `gen_testdata.sh` 生成的
   ffmpeg 素材测真实内容吞吐，`TestWAVRoundTripQuality` 做编码→解码→
   对齐→SNR 以及三个实现的交叉解码。
2. **编码适配器单测**（无 Windows 依赖）：
   - 非 20 ms 输入报错；
   - 单/双声道输出包长符合码率；
   - 连续 50 帧后 `Stats` 与 PTS 推进正确；
   - float32→int16 转换边界（±1.0、NaN）。
3. **解码适配器单测**（无 Windows 依赖）：
   - 正常解码 + PLC（data=nil）输出帧长正确；
   - PreSkip 采样被静默丢弃，不出现在回调中；
   - 编码→解码往返 SNR 与 `bench/opusbench` 一致。
4. **分帧/节拍单测**：用假 `Capturer` 注入 0/10/30/200 ms 的包，验证
   50 pps 输出、静音补齐、积压丢帧；另测 Ring 的环绕搬移、非对齐输入、
   单个超容量包与 `Format.ToFloat32` 的两种采样编码。
5. **WASAPI 冒烟**：`cmd/mfaudio -duration 5s -o out.opus`，播放已知
   440 Hz 音源，检查包率 50 pps、实测码率、VLC 可播放。用 build tag
   `//go:build windows && integration` 隔离，避免无音频端点的 CI 失败。
6. **A/V 同步**：同时录视频与音频，用拍手/秒表画面验证音画偏差 < 40 ms。
7. **长稳**：连续 1 小时，观察内存、`ActualBps`、PTS 漂移与设备切换恢复。

### 10.2 实施阶段

按以下阶段提交，每个阶段独立成 PR，可通过测试后合入主干。

#### P1：Opus 编解码适配器 ✅ 已完成

| 文件 | 内容 |
|------|------|
| `audio/opus.go` | 包级常量：48 kHz / 20 ms / 帧长 / 包上限 / pre-skip 兜底 |
| `audio/encoder.go` | `Encoder` 接口 + `EncodedFrame` |
| `audio/decoder.go` | `Decoder` 接口 + `DecodedFrame` |
| `audio/opus_encoder.go` | `OpusEncoderConfig` + `OpusEncoder`：go-opus 适配、float32→int16、scratch 复用、`SetBitrate` 重建 |
| `audio/opus_decoder.go` | `OpusDecoderConfig` + `OpusDecoder`：PLC、FEC、PreSkip 裁剪 |
| `audio/opus_encoder_test.go` | 编码单测（测试清单第 2 条） |
| `audio/opus_decoder_test.go` | 解码单测 + 编解码往返（测试清单第 3 条） |
| `go.mod` | 新增 `github.com/tphakala/go-opus` 依赖（v1.1.0 或 v1.2.x） |

**决策**：采用 **v1.2.1**，`go.mod` 的 go 指令随之升到 `go 1.27`。

**实测**（i7-9700，Go 1.27.2，48 kHz / stereo / 128 kbps / complexity 5）：

| 指标 | 实测 | 说明 |
|------|------|------|
| 编码 | 159 µs/帧 | 单帧预算 20 ms 的 0.8%；与 §3 的 174 µs 基准同一量级（素材不同） |
| 解码 | 89 µs/包 | 预算的 0.45% |
| 分配 | 1 次 / 51 B（编码）、1 次 / 48 B（解码） | 全部是返回的帧结构体。go-opus 内部仍是 0 allocs |
| 往返相关度 | 0.9999 @ lag 0 | 440 Hz 正弦，50 帧；pre-skip 裁剪后时间线正好对齐 |

**实现中确认的几点**（与前文假设有出入，以代码为准）：

- `OpusEncoderConfig.Complexity` 零值取 **5**，不是 go-opus 的库默认 10——10 的 CPU
  占用翻倍而音质差异听不出来。
- `AudioDesc.PreSkip` 不写死 312，直接取 `OpusEncoder.PreSkip()`；解码侧的
  兜底常量 `defaultPreSkip` 由 `TestOpusPreSkipMatchesLibrary` 断言与库
  一致，换实现时该测试先失败。
- go-opus 的解码器**不**丢 pre-skip（那是容器的活），本项目没有容器，所以
  裁剪放在 `OpusDecoder` 里，由 `OpusDecoderConfig.PreSkip` 驱动。首帧因此
  只有 648 采样（960 − 312），PTS 同步前推 6.5 ms。
- `EncodedFrame.Data` / `DecodedFrame.PCM` 指向内部缓冲，只在下
  一次 Encode/Decode 之前有效；P4 的 `AudioPipeline` 负责复制后交给 Sink。

验收标准：`go test ./audio/...` 全绿，benchmark 数据与 `bench/opusbench` 吻合。

#### P2：音频格式与分帧逻辑 ✅ 已完成

| 文件 | 内容 |
|------|------|
| `audio/format.go` | `Format`（F32LE/S16LE）、`Frame`、`Format.ToFloat32` 转换 |
| `audio/capturer.go` | `Capturer` 接口 + `ErrTimeout` |
| `audio/ring.go` | `Ring`：固定容量 PCM 字节 FIFO，`Append`/`Take`/`Available`/`Drop`，按 960 采样对齐丢整帧 |
| `audio/framer.go` | `Framer`：20 ms 分帧、PTS 时间线、静音补齐、积压统计 |
| `audio/fake_capturer_test.go` | 假 `Capturer`：虚拟时钟回放任意时长/间隔的 PCM 包 |
| `audio/format_test.go` | 格式校验、布局换算、两种采样编码的 float32 转换 |
| `audio/ring_test.go` | 环绕搬移、非对齐输入、溢出丢最旧整帧、超大包只留最新 |
| `audio/framer_test.go` | 10/30 ms 包节奏、静音补齐、积压丢弃、PTS 单调（测试清单第 4 条） |

**P2 决策**（开工前确认）：

- `Frame` 保留设计里的 `[]byte` + `Format.Float32`，采集层可零拷贝交出 WASAPI
  缓冲；编码前由 `Format.ToFloat32` 统一转成 `[]float32`。
- 节拍逻辑拆成 `Ring`（纯字节 FIFO）+ `Framer`（分帧/PTS/静音），而不是全部
  塞进 ring：两者都能脱离 WASAPI 独立测试，P4 的 `AudioPipeline` 只做编排。
- `Frame.Timestamp` 由 Capturer 换算成相对 epoch 的 PTS（WASAPI 内部锚定
  `qpcPosition`），上层不接触 QPC。

**实现细节**：

- `Ring` 的容量按 `maxLatency` 换算成整 20 ms 帧，溢出时只丢整帧；单个包比
  整个环还大时只保留最新的一段。`Take` 返回内部缓冲别名，`Framer` 复制后再
  交给调用方，避免暴露环形布局。
- `Framer` 的时间线由第一条非空数据锚定，之后按采样数推进；ring 排空后允许
  向前重对齐到源时钟，但不倒退——补过静音后源时间戳略早时，单调性对接收端
  的抖动缓冲更重要。
- `Framer` 构造时拒绝非 48 kHz 格式：重采样尚未实现，静默按 44.1 kHz 采样数
  产帧会让每帧时长变成 21.8 ms。

验收结果：`go test ./audio/` 全绿（含 P1）；10 ms/30 ms 包节奏均长期 50 pps，
100 ms 积压上限下 400 ms 巨型包保留最新 5 帧并统计丢弃 15 帧，静音补齐后
PTS 连续且不倒退。

#### P3：WASAPI loopback COM 绑定（可与 P1/P2 并行）

| 文件 | 内容 |
|------|------|
| `audio/wasapi_types.go` | `WAVEFORMATEX(EXTENSIBLE)`、COM GUID、vtable 结构体 |
| `audio/wasapi.go` | `WASAPICapturer`：实现 `audio.Capturer` 接口，事件驱动读取循环 |

这是工作量最大的一块（约 400~600 行 COM 绑定），风格与 `mf/` 包一致：
手写 vtable + `syscall.SyscallN`，不引入 CGo。

验收标准：`//go:build windows && integration` 下可采集到系统音频。

#### P4：AudioPipeline + CLI（依赖 P1 + P2 + P3）

| 文件 | 内容 |
|------|------|
| `audio_pipeline.go` | `AudioPipeline`：采集→ring→编码→Sink 主循环，命令队列，统计 |
| `pipeline.go` | `PipelineConfig` 新增 `Epoch time.Time` 字段，`Run` 中 `start` 改为使用 Epoch |
| `cmd/mfaudio/main.go` | CLI 冒烟工具：`-duration 5s -o out.opus`，输出 Ogg Opus 文件 |

验收标准：测试清单第 5 条（WASAPI 冒烟），VLC 可播放输出文件。

#### P5：AudioDecodeSession（依赖 P1，可与 P3/P4 并行）

| 文件 | 内容 |
|------|------|
| `audio_decode_session.go` | `AudioDecodeSession`：`Submit`→channel→Run goroutine→解码→`OnPCMFrame` |
| `audio_decode_session_test.go` | 编码→提交→解码往返测试，PLC 测试 |

验收标准：对端发来的 Opus 包可正确解码为 PCM，丢包时 PLC 生成补偿帧。

#### P6：应用层集成与验证（依赖 P4 + P5）

| 内容 | 说明 |
|------|------|
| `PipelineConfig.Epoch` 对齐 | 视频与音频共享 Epoch，验证 QPC 时间戳同源 |
| A/V 同步验证 | 测试清单第 6 条：拍手/秒表，偏差 < 40 ms |
| 长稳测试 | 测试清单第 7 条：1 小时连续运行 |
| 集成示例 | 更新 `cmd/mfaudio` 或新增示例，展示音视频双 Pipeline 并行启动 |

### 10.3 阶段依赖关系

```
P1（编解码适配器）──→ P2（格式/分帧）──→ P4（AudioPipeline + CLI）──→ P6（集成验证）
        │                                       ↑
        ├──→ P5（AudioDecodeSession）────────────┘
        │                                       ↑
P3（WASAPI COM 绑定）───────────────────────────┘
```

P1 和 P3 可以并行开发（P3 不依赖 P1），P5 可以与 P3/P4 并行。
关键路径为 P1 → P2 → P4 → P6，其中 P3（WASAPI）是工期最长的单项。

## 11. 风险与备选

| 风险 | 影响 | 应对 |
|------|------|------|
| go-opus 要求 Go 1.27 | CI/构建机需升级 | 接受升级；或锁 v1.1.0（Go 1.26，API 相同） |
| 无运行时 `SetBitrate` | 自适应码率需重建 | 重建仅 ~5 µs，丢一帧；或改用 pion |
| 仅 CELT、无 SILK | 低码率语音效率一般 | 桌面音频以音乐/混合内容为主，64~128 kbps 足够；纯语音场景再评估 libopus |
| 无编码端 FEC | 丢包只能靠 PLC | 传输层做 FEC/重传；`DecodeFEC` 已支持解码侧 |
| loopback 无数据 | 时间线停住 | 默认补静音；结合 DTX 控制码率 |
| WASAPI 绑定工作量大 | 工期 | 先用假 Capturer 打通编码与节拍；WASAPI 单独成 PR |
| pion Encoder 未发版 | API/行为可能变 | 不作为首选；如用则 pin commit 并在 CI 锁版本 |
| 许可 | 合规审查 | go-opus BSD-3-Clause、simd MIT；保留 libopus 版权声明 |
