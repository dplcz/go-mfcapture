# mfcapture

PoC: zero-copy screen capture and hardware H264 encoding on Windows, built in
pure Go (no CGO) on top of [go-d3d](https://github.com/kirides/go-d3d)'s COM
binding conventions.

Pipeline:

```
DXGI Desktop Duplication (GPU texture)
  -> GPU->GPU CopyResource into a private D3D11 texture
  -> MFCreateDXGISurfaceBuffer (wraps the texture as an IMFMediaBuffer, no CPU copy)
  -> IMFSample -> IMFSinkWriter.WriteSample (hardware H264 encoder MFT)
  -> MP4 file
```

The captured frame data never touches CPU memory between duplication and the
encoder; the only CPU-side work per frame is a few COM calls and PODs
(timestamps, GUIDs, small structs).

## Package layout

- `mf/` - Media Foundation COM bindings (vtable structs + wrapper methods,
  following go-d3d's pattern: `syscall.SyscallN` through `uintptr` vtable
  fields, no CGO). Covers just the subset needed here: `IMFAttributes`,
  `IMFMediaType`, `IMFSample`, `IMFMediaBuffer`, `IMFSinkWriter`,
  `IMFDXGIDeviceManager`, plus the free functions (`MFStartup`,
  `MFCreateSinkWriterFromURL`, etc.) and GUIDs.
- `capture/` - `NewVideoCapableD3D11Device` (D3D11 device with the
  `VIDEO_SUPPORT`/`BGRA_SUPPORT` flags MF needs, which go-d3d's own device
  constructor doesn't set), `EnableMultithreadProtection` (`ID3D10Multithread`,
  not bound by go-d3d), and `RawDuplicator` (a minimal desktop-duplication
  wrapper that hands back a GPU texture instead of go-d3d's CPU-mapped one).
- `pipeline.go` - `Pipeline`: wires the above into a capture→encode loop and
  hands encoded H264 frames + cursor updates to a caller-supplied `Sink`. This
  is the module's only public entry point for streaming.
- `decode/` - `H264MFTDecoder`: wraps a Media Foundation H264 **decoder** MFT
  (hardware first, software fallback). Turns Annex-B NAL data back into pixels
  (NV12 by default). Used by the viewing client.
- `record.go` - `Record(ctx, Config) error`: legacy path writing an MP4 via
  `IMFSinkWriter`. Kept for reference; new code should use `Pipeline`.
- `cmd/mfrecord/` - CLI wrapper for the legacy MP4 path.
- `cmd/mfencode/` - CLI wrapper for `Pipeline` (raw H264 to file).
- `record_test.go` - unit tests plus manual integration tests (see Testing).
- `docs/app-layer-design.md` - design of the application layer (device
  discovery, signalling, auth, input, clipboard, file transfer) that sits on
  top of this module as a separate project.

This module does not modify go-d3d; it only reuses its exported `d3d11`/`dxgi`
types and GUIDs, and depends on it via a local `replace` directive (see
`go.mod`) pointing at a sibling `go-d3d-review` checkout.

## Requirements

- Windows 10/11, Go 1.26+ (`go.mod` says 1.26.4).
- **A Media Foundation hardware H264 encoder MFT** (Intel QSV / NVIDIA NVENC /
  AMD AMF). This is the important one — see the troubleshooting section below.
- An unlocked, local interactive (console) session. DXGI Desktop Duplication
  returns `E_ACCESSDENIED` when the session is locked, and generally doesn't
  work over RDP.

## Troubleshooting: `SetInputType: 0xC00D36B4`

`0xC00D36B4` is `MF_E_INVALIDMEDIATYPE` — the encoder rejected the input media
type. In practice this means **there is no hardware H264 encoder on the machine**
(typical in VMs and on driverless installs), so the code fell back to Microsoft's
**software** H264 encoder MFT, and that one does not accept `ARGB32` input.

The software encoder's accepted input subtypes are `IYUV / YV12 / NV12 / YUY2`.
Hardware encoders (Intel/NVIDIA/AMD) additionally accept `ARGB32`, which is what
DXGI Desktop Duplication produces and what this module feeds in.

Confirm on the affected machine:

```sh
go run ./cmd/mfdiag            # uses the primary monitor's resolution
go run ./cmd/mfdiag -w 1920 -h 1080
```

Example output on a machine with hardware encoders:

```
[硬件 (MFT_ENUM_FLAG_HARDWARE)] 找到 3 个
  #0 D3D11-aware=1  接受的输入类型: [NV12 2560x1440 ARGB32 2560x1440]
[软件/同步 (MFT_ENUM_FLAG_SYNCMFT)] 找到 1 个
  #0 D3D11-aware=unknown  接受的输入类型: [IYUV 2560x1440 YV12 2560x1440 NV12 2560x1440 YUY2 2560x1440]
```

If the hardware group is empty, the only way forward is to convert the captured
BGRA texture to NV12 before encoding (tracked as a work item — not implemented
yet). Note the software encoder is not D3D-aware, so it would also need CPU-side
samples (`MFCreateMemoryBuffer`) rather than DXGI surface buffers.

## Build & run

```sh
go build ./...
go run ./cmd/mfencode -list                     # list captureable displays
go run ./cmd/mfencode -duration 5s -o out.h264  # raw H264 Annex-B to file
go run ./cmd/mfrecord -duration 5s -o out.mp4   # legacy MP4 path
```

`cmd/mfencode` flags:

| Flag         | Default    | Meaning                                                     |
|--------------|------------|--------------------------------------------------------------|
| `-duration`  | `5s`       | recording duration                                          |
| `-o`         | `out.h264` | output raw H264 path                                        |
| `-bitrate`   | `8000000`  | target bitrate, bits/sec                                    |
| `-monitor`   | `0`        | which display to capture (`-list` shows valid indices)      |
| `-maxfps`    | `30`       | cap on frames per second (`0` = unlimited, see warning)     |
| `-gop`       | `30`       | keyframe interval in frames                                 |
| `-list`      | `false`    | list captureable displays and exit                          |

**Bitrate warning.** The encoder allocates its bit budget per frame using the
frame rate declared in `FPSNum`/`FPSDen`. If `MaxFPS` is left at `0`, the real
frame rate is whatever the screen change rate happens to be, and the actual
bitrate drifts to roughly `BitrateBps × (actualFPS / FPSNum)`. Measured here:
declared 30 fps, configured 8 Mbps, unlimited — ~100 fps and ~20 Mbps on a busy
screen. Keep `MaxFPS` equal to `FPSNum` (or watch `Stats().Frames`) if you care
about hitting the target bitrate.

## Using this module from an application layer

```go
p, _ := mfcapture.NewPipeline(mfcapture.PipelineConfig{Output: 0, MaxFPS: 30}, mySink)
go func() { _ = p.Run(ctx) }()   // Run binds the calling goroutine to an OS thread
p.RequestKeyFrame()              // thread-safe, queued to the capture thread
p.SetBitrate(4_000_000)
```

- `Run` owns the OS thread and does all COM/MF init and teardown internally.
  `NewPipeline` touches no COM at all.
- `Sink.OnFrame` is called synchronously on the capture thread. **The module
  does no queueing and no frame dropping** — buffering and drop policy are the
  caller's decision. If `OnFrame` blocks, you are choosing to drop capture
  frames.
- Output is Annex-B H264 with SPS/PPS inlined before every IDR, so a decoder
  can start at any keyframe; `VideoDesc.Extradata` is always `nil`.
- `EventDescReady` always precedes the first `OnFrame`.

## Decoding (client side)

```go
dec, _ := decode.NewH264MFTDecoder(decode.H264DecoderConfig{
    Width: 2560, Height: 1440,   // 来自 VideoDesc
})
frames, err := dec.Decode(accessUnit, pts, dur)   // 一次可能出 0 帧或多帧
subtype, w, h, stride := dec.OutputFormat()       // 通常是 NV12 2560x1440
```

- Feed one **access unit** per call (in practice one `EncodedFrame` from the
  host — the encoder emits whole AUs).
- Output is uncompressed pixels, NV12 by default (`w*h*3/2` bytes per frame).
- `MF_E_TRANSFORM_STREAM_CHANGE` (被控端改分辨率) is handled internally: the
  output type is renegotiated and decoding continues.
- Resolution must be passed in (`-w/-h` in the demo). Without it some decoder
  MFTs refuse input (`MF_E_NOTACCEPTING`); the client always knows the size
  from `VideoDesc`, so this is not a problem in practice.

### 硬件加速（DXVA）

默认开启。一个容易踩的误解先说清楚：

**Windows 的 H264 解码器 MFT 不是用 `MFT_ENUM_FLAG_HARDWARE` 枚举出来的那种"硬件 MFT"。**
实测 `MFT_ENUM_FLAG_HARDWARE` 枚举视频解码器结果是 **0 个**；能枚举到的是一个**同步**、
但 `D3D11-aware=1` 的 MFT。它走不走硬件，取决于你有没有把 `IMFDXGIDeviceManager`
交给它（`MFT_MESSAGE_SET_D3D_MANAGER`）——不给就内部退化成软件解码。

所以本包的做法是：默认创建 D3D11 设备 + 设备管理器交给解码器（即默认 DXVA），
**并且带自动回退**——如果 DXVA 路径解码出错，就整个重建为软件解码器继续跑，
只回退一次，不会每帧都重建。

实测（2560×1440，3 秒 48 个访问单元）：

| 配置 | 结果 |
|---|---|
| DXVA（默认） | 协商成功，但 `ProcessOutput` 持续报 `0xC00D6D61`，回退后正常解码 |
| `-nohw` 纯软件 | 48 帧全部解出，**5.5 ms/帧（约 180 fps）** |

在这台机器上 DXVA 反而更慢/不稳，原因可能是 `D3D11-aware` 但内部 DXVA 路径未被驱动
正确支持，或者需要解码到 D3D 纹理（而不是回读内存）。**先跑软件版本**：180 fps 远超
30 fps 的需求；等客户端接上 D3D 渲染、需要零拷贝纹理时再做 DXVA 直出纹理那条路。

### NV12 像素怎么给播放器用

解码输出 `DecodedFrame{Subtype, Width, Height, Stride, Data}`，默认 `NV12`。三点注意：

1. **NV12 是解码器与渲染器之间最好的交换格式**：硬件解码器原生输出它，
   D3D11/ffmpeg/ffplay 都能直接吃（`ffplay -f rawvideo -pix_fmt nv12 -s WxH`），
   每帧只有 `w*h*3/2` 字节，是 BGRA 的一半。
2. **但它不能直接喂给 UI 工具箱**（那些要 RGB/A）。需要 RGB 时用 `decode.ToBGRA`：

   ```go
   bgra, err := decode.ToBGRA(frame, decode.ColorMatrixAuto) // 自动向选 BT.709(≥1280)/BT.601
   ```

   注意这是纯 Go 逐像素转换，2560×1440 实测约 **12 ms/帧**——真要上 GPU 渲染时
   应该让 D3D11 的 shader/video processor 直接吃 NV12，别在 CPU 上转。
3. **务必使用 `Stride`**：MF 输出的行宽常常大于 `Width`（对齐），直接按 `Width` 步进
   读会得到错位的画面。`ToBGRA` 已经处理了。

Verification round trip:

```sh
go run ./cmd/mfencode -duration 4s -o t.h264
go run ./cmd/mfdecode -i t.h264 -o t.nv12 -w 2560 -h 1440
ffmpeg -f rawvideo -pix_fmt nv12 -video_size 2560x1440 -i t.nv12 -frames:v 1 frame.png
```

## Testing

`go test ./...` runs the normal (fast, no real capture) test suite.

Fast tests (no real capture): `TestEnumMonitors`, `TestParseSPSProfileLevel`.

`record_test.go` also has manual integration tests that actually capture the
real screen. They are skipped unless explicitly opted into:

```sh
MFCAPTURE_MANUAL_TEST=1 go test -run 'TestRecordZeroCopy|TestPipelineRun' -v ./...
```

They require an unlocked interactive session (see Requirements).

## Scope / known limitations

This is a PoC validating the zero-copy pipeline, not a production capture
layer. Deliberately out of scope:

- **Fixed encoding parameters.** Native screen resolution, fixed bitrate, no
  adaptive bitrate/resolution/QoS.
- **Variable frame rate, no resubmission.** A sample is written only when
  `AcquireNextFrame` actually returns a new frame; timestamps use real
  elapsed wall-clock time. On timeout (no screen change) the loop just waits
  again - the previous frame is never resubmitted to simulate a fixed rate.
  `-maxfps` only adds an upper bound on top of this; it does not force a
  minimum rate. (For a remote-desktop client this is fine: it keeps showing
  the last frame. Use `RequestKeyFrame()` when a new consumer joins.)
- **Cursor is delivered out-of-band, not composited.** The captured texture
  is the desktop surface only. Cursor position/visibility/shape come through
  `Sink.OnCursor` as separate updates (cached by `HCURSOR`, bitmap sent once
  per shape), which is what a "server-side cursor" remote-desktop client
  wants. Nothing composites it into the video.
- **Single output per pipeline, no audio.**
- **No decoder.** This module encodes only. A viewing client needs its own
  H264 decoder (e.g. an MF decoder MFT, or ffmpeg).

`DXGI_ERROR_ACCESS_LOST` (lock/unlock, display mode change, GPU reset) is
recovered automatically: the duplicator is recreated and, if the resolution
changed, the encoder is rebuilt to match and an `EventResolutionChanged` is
raised.
