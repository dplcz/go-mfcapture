// Package audio 是远程桌面的音频采集与编解码层，与视频侧的 capture/、
// encode/、decode/ 对应。
//
// 分层与视频侧对称：Capturer 是采集源，Encoder/Decoder 是编解码器，
// 两者都由根包的 Pipeline 编排。平台相关的 COM 绑定（WASAPI）也放在本包内，
// 风格与 mf/ 一致：手写 vtable + syscall，不引入 cgo。
package audio

// 整条音频链路固定跑在 48 kHz / 20 ms：WASAPI loopback 以 48 kHz 采集，Opus 的
// 编码采样率也固定 48 kHz，因此不需要重采样，容器时间戳也统一按 48 kHz 计。
const (
	// OpusSampleRate 是本包唯一使用的采样率。
	OpusSampleRate = 48000
	// OpusFrameMs 是每包的帧长。20 ms 是 Opus 的标准帧长，也是 RTP 的 ptime=20。
	OpusFrameMs = 20
	// OpusFrameSamples 是每声道每帧的采样数：48000 * 20 / 1000 = 960，
	// 也就是 Opus 的 20 ms 帧长。上层按它分配 float32 编码缓冲。
	OpusFrameSamples = OpusSampleRate * OpusFrameMs / 1000
	// frameSamples 是包内使用的短名，与 OpusFrameSamples 保持同一个值。
	frameSamples = OpusFrameSamples

	// maxPacketSize 是单帧包的最大字节数。go-opus 文档：1276 字节可容纳任意
	// 2.5~20 ms 的 VBR 包；缓冲不足时 libopus 会主动降码率而不是报错，所以这里
	// 给满，避免静默降质。
	maxPacketSize = 1276

	// defaultComplexity 是 OpusEncoderConfig.Complexity 的零值替代。go-opus 的
	// 库默认是 10，本项目按设计取 5：桌面音频以音乐/混合内容为主，5 与 10 的
	// 音质差异听不出来，但 CPU 占用差一倍。
	defaultComplexity = 5

	// defaultPreSkip 是 go-opus 在 48 kHz 下的 pre-skip 采样数（6.5 ms），用作
	// 没有带外描述时的兜底。真实值应该用对端 AudioDesc 里的 PreSkip 覆盖：
	// opus_encoder_test.go 断言它与 OpusEncoder.PreSkip() 一致，换实现时这条
	// 测试会先失败。
	defaultPreSkip = 312
)
