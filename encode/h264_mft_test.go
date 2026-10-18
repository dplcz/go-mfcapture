package encode

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"structs"
	"syscall"
	"testing"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/com"
	"golang.org/x/sys/windows"
	"mfcapture/mf"
)

type encoderTestObject struct {
	_    structs.HostLayout
	vtbl unsafe.Pointer
}

// Reinterpret callback argument storage without a uintptr-to-pointer conversion
// whose lifetime could span a syscall.
func encoderCallbackPointer[T any](v uintptr) *T { return *(**T)(unsafe.Pointer(&v)) }

type outputFixture struct {
	enc                                           H264MFTEncoder
	objects                                       []*encoderTestObject
	results                                       []uint32
	streamInfo                                    mf.MFT_OUTPUT_STREAM_INFO
	streamInfoHR                                  uint32
	setTypeHR                                     uint32
	noTypes                                       bool
	inputCalls, outputCalls, typeCalls, infoCalls int
	sampleReleases, eventReleases                 int
	capacities                                    []uint32
	callbackErr                                   error
	types                                         []*mf.IMFMediaType
	candidateCount                                uintptr
	asyncFixture                                  bool
	inputCredits, outputCredits                   int
	eventsRemaining                               int
	eventStatus                                   uint32
	nativeSample                                  *mf.IMFSample
}

func newOutputFixture(t *testing.T, results ...uint32) *outputFixture {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	if hr := mf.CoInitializeEx(); d3d.HRESULT(hr).Failed() {
		t.Fatalf("CoInitializeEx: %v", d3d.HRESULT(hr))
	}
	t.Cleanup(mf.CoUninitialize)
	if hr := mf.MFStartup(); d3d.HRESULT(hr).Failed() {
		t.Fatalf("MFStartup: %v", d3d.HRESULT(hr))
	}
	t.Cleanup(func() { mf.MFShutdown() })
	f := &outputFixture{results: results, capacities: make([]uint32, 0, 8), candidateCount: 1, streamInfo: mf.MFT_OUTPUT_STREAM_INFO{DwFlags: mf.MFT_OUTPUT_STREAM_PROVIDES_SAMPLES, CbSize: 4096}}
	f.enc.width, f.enc.height = 2560, 1600
	f.enc.providesSamples = true
	f.enc.outputBufSize = 16
	for range 4 {
		mt, err := mf.NewOutputMediaType(2560, 1600, 30, 1, 5_000_000)
		if err != nil || mt == nil {
			t.Fatalf("create test output type: %p %v", mt, err)
		}
		f.types = append(f.types, mt)
	}
	t.Cleanup(func() {
		for _, mt := range f.types {
			mt.Release()
		}
	})
	object := func(vtbl unsafe.Pointer) *encoderTestObject {
		o := &encoderTestObject{vtbl: vtbl}
		f.objects = append(f.objects, o)
		return o
	}
	data := []byte{0, 0, 1, 0x65}
	// Use a native sample in the multi-call async tests. Returning fake Go COM
	// buffers through reentrant callbacks can invalidate stack output pointers.
	if hr := mf.MFCreateSample(&f.nativeSample); d3d.HRESULT(hr).Failed() {
		t.Fatal(d3d.HRESULT(hr))
	}
	t.Cleanup(func() {
		if refs := f.nativeSample.Release(); refs != 0 {
			t.Errorf("native output sample leaked %d references", refs)
		}
	})
	var nativeBuffer *mf.IMFMediaBuffer
	if hr := mf.MFCreateMemoryBuffer(uint32(len(data)), &nativeBuffer); d3d.HRESULT(hr).Failed() {
		t.Fatal(d3d.HRESULT(hr))
	}
	defer nativeBuffer.Release()
	var nativeData unsafe.Pointer
	if hr := nativeBuffer.Lock(&nativeData, nil, nil); d3d.HRESULT(hr).Failed() {
		t.Fatal(d3d.HRESULT(hr))
	}
	copy(unsafe.Slice((*byte)(nativeData), len(data)), data)
	nativeBuffer.Unlock()
	nativeBuffer.SetCurrentLength(uint32(len(data)))
	f.nativeSample.AddBuffer(nativeBuffer)
	bufferVtbl := &mf.IMFMediaBufferVtbl{}
	bufferVtbl.Release = syscall.NewCallback(func(uintptr) uintptr { return 1 })
	bufferVtbl.Lock = syscall.NewCallback(func(_ uintptr, p, max, current uintptr) uintptr {
		*encoderCallbackPointer[unsafe.Pointer](p) = unsafe.Pointer(&data[0])
		if max != 0 {
			*encoderCallbackPointer[uint32](max) = uint32(len(data))
		}
		if current != 0 {
			*encoderCallbackPointer[uint32](current) = uint32(len(data))
		}
		return 0
	})
	bufferVtbl.Unlock = syscall.NewCallback(func(uintptr) uintptr { return 0 })
	buffer := object(unsafe.Pointer(bufferVtbl))
	sampleVtbl := &mf.IMFSampleVtbl{}
	sampleVtbl.Release = syscall.NewCallback(func(uintptr) uintptr { f.sampleReleases++; return 0 })
	sampleVtbl.ConvertToContiguousBuffer = syscall.NewCallback(func(_ uintptr, pp uintptr) uintptr {
		*encoderCallbackPointer[*mf.IMFMediaBuffer](pp) = (*mf.IMFMediaBuffer)(unsafe.Pointer(buffer))
		return 0
	})
	getTime := syscall.NewCallback(func(_ uintptr, p uintptr) uintptr { *encoderCallbackPointer[int64](p) = 123; return 0 })
	sampleVtbl.GetSampleTime, sampleVtbl.GetSampleDuration = getTime, getTime
	sampleVtbl.GetUINT32 = syscall.NewCallback(func(_ uintptr, _ uintptr, p uintptr) uintptr { *encoderCallbackPointer[uint32](p) = 1; return 0 })
	sample := object(unsafe.Pointer(sampleVtbl))
	eventsVtbl := &com.IUnknownVtbl{}
	eventsVtbl.Release = syscall.NewCallback(func(uintptr) uintptr { f.eventReleases++; return 0 })
	events := object(unsafe.Pointer(eventsVtbl))
	transformVtbl := &mf.IMFTransformVtbl{}
	transformVtbl.ProcessInput = syscall.NewCallback(func(_, _, _, _ uintptr) uintptr {
		if f.asyncFixture {
			if f.inputCredits == 0 {
				f.callbackErr = fmt.Errorf("ProcessInput without NeedInput")
				return 0xC00D36B0
			}
			f.inputCredits--
		}
		f.inputCalls++
		return 0
	})
	transformVtbl.ProcessMessage = syscall.NewCallback(func(_, _, _ uintptr) uintptr { return 0 })
	transformVtbl.GetOutputAvailableType = syscall.NewCallback(func(_, _, index, pp uintptr) uintptr {
		target := encoderCallbackPointer[*mf.IMFMediaType](pp)
		if f.noTypes || index >= f.candidateCount {
			return 0xC00D36B9
		}
		if len(f.types) == 0 {
			f.callbackErr = fmt.Errorf("test output types exhausted")
			return 0x80004005
		}
		mt := f.types[0]
		f.types = f.types[1:]
		*target = mt
		return 0
	})
	transformVtbl.SetOutputType = syscall.NewCallback(func(_, _, mt, flags uintptr) uintptr {
		f.typeCalls++
		var sub windows.GUID
		if hr := encoderCallbackPointer[mf.IMFMediaType](mt).GetGUID(mf.MF_MT_SUBTYPE, &sub); d3d.HRESULT(hr).Failed() || sub != mf.MFVideoFormat_H264 || flags != 0 {
			f.callbackErr = fmt.Errorf("unexpected output type or flags")
			return 0x80004005
		}
		return uintptr(f.setTypeHR)
	})
	transformVtbl.GetOutputStreamInfo = syscall.NewCallback(func(_, _, p uintptr) uintptr {
		f.infoCalls++
		*encoderCallbackPointer[mf.MFT_OUTPUT_STREAM_INFO](p) = f.streamInfo
		return uintptr(f.streamInfoHR)
	})
	transformVtbl.ProcessOutput = syscall.NewCallback(func(_, _, count, p, _ uintptr) uintptr {
		// Model the async MFT contract: each call consumes one HaveOutput,
		// including a call that returns STREAM_CHANGE.
		if f.asyncFixture {
			if f.outputCredits == 0 {
				return 0x8000FFFF // E_UNEXPECTED
			}
			f.outputCredits--
		}
		out := encoderCallbackPointer[mf.MFT_OUTPUT_DATA_BUFFER](p)
		if count != 1 {
			f.callbackErr = fmt.Errorf("output count=%d", count)
			return 0x80004005
		}
		if out.PSample != nil {
			var buf *mf.IMFMediaBuffer
			if hr := out.PSample.GetBufferByIndex(0, &buf); d3d.HRESULT(hr).Failed() {
				f.callbackErr = fmt.Errorf("missing caller buffer")
				return 0x80004005
			}
			var memory unsafe.Pointer
			var capacity uint32
			buf.Lock(&memory, &capacity, nil)
			buf.Unlock()
			buf.Release()
			f.capacities = append(f.capacities, capacity)
		} else {
			f.capacities = append(f.capacities, 0)
		}
		out.PSample = (*mf.IMFSample)(unsafe.Pointer(sample))
		if f.asyncFixture {
			f.nativeSample.AddRef()
			out.PSample = f.nativeSample
		}
		out.PEvents = (*mf.IMFCollection)(unsafe.Pointer(events))
		i := f.outputCalls
		f.outputCalls++
		if i >= len(f.results) {
			f.callbackErr = fmt.Errorf("unexpected extra output call")
			return 0x80004005
		}
		return uintptr(f.results[i])
	})
	f.enc.transform = (*mf.IMFTransform)(unsafe.Pointer(object(unsafe.Pointer(transformVtbl))))
	t.Cleanup(func() {
		if f.callbackErr != nil {
			t.Error(f.callbackErr)
		}
		runtime.KeepAlive(f)
	})
	return f
}

func TestEncoderStreamChangeContinuesAcceptedInput(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, 0)
	frame, err := f.enc.encodeSync(nil)
	if err != nil {
		t.Fatal(err)
	}
	if frame == nil || !bytes.Equal(frame.Data, []byte{0, 0, 1, 0x65}) || !frame.IsKeyFrame {
		t.Fatalf("frame=%+v", frame)
	}
	if f.inputCalls != 1 || f.outputCalls != 2 || f.typeCalls != 1 || f.infoCalls != 1 {
		t.Fatalf("input=%d output=%d setType=%d info=%d", f.inputCalls, f.outputCalls, f.typeCalls, f.infoCalls)
	}
	if f.sampleReleases != 2 || f.eventReleases != 2 {
		t.Fatalf("sample releases=%d event releases=%d", f.sampleReleases, f.eventReleases)
	}
}

func (f *outputFixture) useAsyncEvents(eventTypes ...uint32) {
	f.asyncFixture = true
	f.eventsRemaining = len(eventTypes)
	var current uint32
	eventVtbl := &mf.IMFMediaEventVtbl{}
	eventVtbl.Release = syscall.NewCallback(func(uintptr) uintptr { return 0 })
	eventVtbl.GetType = syscall.NewCallback(func(_ uintptr, p uintptr) uintptr {
		*encoderCallbackPointer[uint32](p) = current
		return 0
	})
	eventVtbl.GetStatus = syscall.NewCallback(func(_ uintptr, p uintptr) uintptr {
		*encoderCallbackPointer[int32](p) = int32(f.eventStatus)
		return 0
	})
	event := &encoderTestObject{vtbl: unsafe.Pointer(eventVtbl)}
	f.objects = append(f.objects, event)
	genVtbl := &mf.IMFMediaEventGeneratorVtbl{}
	genVtbl.GetEvent = syscall.NewCallback(func(_, _, pp uintptr) uintptr {
		if len(eventTypes) == 0 {
			f.callbackErr = fmt.Errorf("test event queue exhausted (lost notification or unexpected wait)")
			return 0x80004005
		}
		current, eventTypes = eventTypes[0], eventTypes[1:]
		f.eventsRemaining = len(eventTypes)
		if current == mf.METransformNeedInput {
			f.inputCredits++
		}
		if current == mf.METransformHaveOutput {
			f.outputCredits++
		}
		*encoderCallbackPointer[*mf.IMFMediaEvent](pp) = (*mf.IMFMediaEvent)(unsafe.Pointer(event))
		return 0
	})
	gen := &encoderTestObject{vtbl: unsafe.Pointer(genVtbl)}
	f.objects = append(f.objects, gen)
	f.enc.eventGen = (*mf.IMFMediaEventGenerator)(unsafe.Pointer(gen))
	f.enc.isAsync = true
}

func TestAsyncEncoderStreamChangeContinuesOutput(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, 0)
	f.useAsyncEvents(mf.METransformNeedInput, mf.METransformHaveOutput, mf.METransformHaveOutput)
	frame, err := f.enc.Encode(nil)
	if err != nil || frame == nil || f.inputCalls != 1 || f.outputCalls != 2 || f.eventsRemaining != 0 {
		t.Fatalf("frame=%v err=%v inputs=%d outputs=%d remaining events=%d", frame, err, f.inputCalls, f.outputCalls, f.eventsRemaining)
	}
}

func TestAsyncEncoderPreservesNeedInput(t *testing.T) {
	for _, streamChange := range []bool{false, true} {
		t.Run(fmt.Sprint(streamChange), func(t *testing.T) {
			f := newOutputFixture(t, 0)
			events := []uint32{mf.METransformNeedInput, mf.METransformNeedInput, mf.METransformHaveOutput}
			if streamChange {
				f.results = []uint32{mf.MF_E_TRANSFORM_STREAM_CHANGE, 0}
				events = []uint32{mf.METransformNeedInput, mf.METransformHaveOutput, mf.METransformNeedInput, mf.METransformHaveOutput}
			}
			f.useAsyncEvents(events...)
			frame, err := f.enc.Encode(nil)
			if err != nil || frame != nil {
				t.Fatalf("first encode: frame=%v err=%v", frame, err)
			}
			frame, err = f.enc.Encode(nil)
			if err != nil || frame == nil || f.inputCalls != 2 || f.eventsRemaining != 0 {
				t.Fatalf("second encode: frame=%v err=%v inputs=%d outputs=%d remaining=%d releases=%d", frame, err, f.inputCalls, f.outputCalls, f.eventsRemaining, f.sampleReleases)
			}
		})
	}
}

func TestAsyncEncoderPreservesOutputBeforeNeedInput(t *testing.T) {
	f := newOutputFixture(t, 0, 0)
	f.useAsyncEvents(mf.METransformNeedInput, mf.METransformHaveOutput, mf.METransformHaveOutput, mf.METransformNeedInput)
	for i := 0; i < 2; i++ {
		frame, err := f.enc.Encode(nil)
		if err != nil || frame == nil {
			t.Fatalf("encode %d: frame=%v err=%v inputs=%d outputs=%d releases=%d", i, frame, err, f.inputCalls, f.outputCalls, f.sampleReleases)
		}
	}
	if f.inputCalls != 2 || f.outputCalls != 2 || f.eventsRemaining != 0 {
		t.Fatalf("inputs=%d outputs=%d remaining=%d", f.inputCalls, f.outputCalls, f.eventsRemaining)
	}
}

func TestAsyncEncoderChecksEventStatus(t *testing.T) {
	f := newOutputFixture(t)
	f.useAsyncEvents(mf.METransformNeedInput)
	f.eventStatus = 0x80004005
	_, err := f.enc.Encode(nil)
	if err == nil || !strings.Contains(err.Error(), "0x80004005") || f.inputCalls != 0 {
		t.Fatalf("err=%v inputs=%d", err, f.inputCalls)
	}
}

func TestAsyncEncoderStreamChangeRetryLimit(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE)
	f.useAsyncEvents(mf.METransformNeedInput, mf.METransformHaveOutput, mf.METransformHaveOutput, mf.METransformHaveOutput, mf.METransformHaveOutput)
	_, err := f.enc.Encode(nil)
	if err == nil || !strings.Contains(err.Error(), "too many consecutive") || f.outputCalls != 4 || f.typeCalls != 3 {
		t.Fatalf("err=%v outputs=%d types=%d", err, f.outputCalls, f.typeCalls)
	}
}

func TestAsyncEncoderStreamChangeLimitAcrossInputs(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE)
	f.useAsyncEvents(mf.METransformNeedInput, mf.METransformHaveOutput,
		mf.METransformNeedInput, mf.METransformHaveOutput,
		mf.METransformNeedInput, mf.METransformHaveOutput,
		mf.METransformNeedInput, mf.METransformHaveOutput)
	for i := 0; i < 3; i++ {
		if frame, err := f.enc.Encode(nil); err != nil || frame != nil {
			t.Fatalf("encode %d: frame=%v err=%v", i, frame, err)
		}
	}
	if _, err := f.enc.Encode(nil); err == nil || !strings.Contains(err.Error(), "too many consecutive") {
		t.Fatalf("err=%v", err)
	}
}

func TestAsyncEncoderFlushBufferedFrames(t *testing.T) {
	f := newOutputFixture(t, 0, 0, 0)
	f.useAsyncEvents(mf.METransformNeedInput, mf.METransformMarker, mf.METransformHaveOutput,
		mf.METransformHaveOutput, mf.METransformHaveOutput, mf.METransformNeedInput,
		mf.METransformDrainComplete)
	for i := 0; i < 2; i++ {
		frame, err := f.enc.Encode(nil)
		if err != nil || frame == nil || !bytes.Equal(frame.Data, []byte{0, 0, 1, 0x65}) {
			t.Fatalf("encode %d: frame=%v err=%v", i, frame, err)
		}
	}
	frames, err := f.enc.Flush()
	if err != nil || len(frames) != 1 || f.inputCalls != 2 || f.outputCalls != 3 || f.eventsRemaining != 0 {
		t.Fatalf("frames=%d err=%v inputs=%d outputs=%d remaining=%d", len(frames), err, f.inputCalls, f.outputCalls, f.eventsRemaining)
	}
}

func TestAsyncEncoderRejectsOutputWithoutEvent(t *testing.T) {
	f := newOutputFixture(t)
	f.useAsyncEvents()
	if _, err := f.enc.processOutput(); err == nil || f.outputCalls != 0 {
		t.Fatalf("err=%v outputCalls=%d", err, f.outputCalls)
	}
}

func TestAsyncEncoderFlushStreamChange(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, 0)
	f.useAsyncEvents(mf.METransformHaveOutput, mf.METransformNeedInput, mf.METransformHaveOutput, mf.METransformDrainComplete)
	frames, err := f.enc.Flush()
	if err != nil || len(frames) != 1 || f.inputCalls != 0 || f.outputCalls != 2 || f.eventsRemaining != 0 {
		t.Fatalf("frames=%d err=%v inputs=%d outputs=%d remaining=%d", len(frames), err, f.inputCalls, f.outputCalls, f.eventsRemaining)
	}
}

func TestEncoderStreamChangeRefreshesAllocation(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after bool
		size          uint32
	}{
		{"larger caller buffer", false, false, 8192},
		{"switch to caller allocation", true, false, 4096},
		{"switch to MFT allocation", false, true, 4096},
		{"zero size fallback", false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, 0)
			f.enc.providesSamples = tc.before
			f.streamInfo.DwFlags = 0
			if tc.after {
				f.streamInfo.DwFlags = mf.MFT_OUTPUT_STREAM_PROVIDES_SAMPLES
			}
			f.streamInfo.CbSize = tc.size
			frame, err := f.enc.processOutput()
			if err != nil || frame == nil {
				t.Fatalf("frame=%v err=%v", frame, err)
			}
			want := tc.size
			if want == 0 {
				want = 1024 * 1024
			}
			if tc.after {
				want = 0
			}
			if f.capacities[1] != want {
				t.Fatalf("retry buffer=%d want=%d", f.capacities[1], want)
			}
			if f.sampleReleases != 2 || f.eventReleases != 2 {
				t.Fatalf("sample releases=%d events=%d", f.sampleReleases, f.eventReleases)
			}
		})
	}
}

func TestEncoderStreamChangeFailures(t *testing.T) {
	for _, tc := range []struct {
		name          string
		noTypes       bool
		setHR, infoHR uint32
		want          string
	}{
		{"no output types", true, 0, 0, "GetOutputAvailableType"},
		{"rejected type", false, 0xC00D36B4, 0, "SetOutputType"},
		{"stream info failed", false, 0, 0x80004005, "GetOutputStreamInfo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE)
			f.noTypes, f.setTypeHR, f.streamInfoHR = tc.noTypes, tc.setHR, tc.infoHR
			frame, err := f.enc.processOutput()
			if frame != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("frame=%v err=%v", frame, err)
			}
			if f.outputCalls != 1 || f.sampleReleases != 1 || f.eventReleases != 1 {
				t.Fatalf("output=%d samples=%d events=%d", f.outputCalls, f.sampleReleases, f.eventReleases)
			}
		})
	}
}

func TestEncoderStreamChangeRetryLimit(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_STREAM_CHANGE)
	_, err := f.enc.processOutput()
	if err == nil || !strings.Contains(err.Error(), "too many consecutive") {
		t.Fatalf("err=%v", err)
	}
	if f.outputCalls != 4 || f.typeCalls != 3 || f.sampleReleases != 4 || f.eventReleases != 4 {
		t.Fatalf("output=%d types=%d samples=%d events=%d", f.outputCalls, f.typeCalls, f.sampleReleases, f.eventReleases)
	}
}

func TestEncoderStreamChangeSelectsCompatibleType(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, 0)
	f.candidateCount = 3
	if hr := f.types[0].SetGUID(mf.MF_MT_SUBTYPE, mf.MFVideoFormat_NV12); d3d.HRESULT(hr).Failed() {
		t.Fatal(d3d.HRESULT(hr))
	}
	if hr := f.types[1].SetUINT64(mf.MF_MT_FRAME_SIZE, uint64(1920)<<32|1080); d3d.HRESULT(hr).Failed() {
		t.Fatal(d3d.HRESULT(hr))
	}
	frame, err := f.enc.processOutput()
	if err != nil || frame == nil {
		t.Fatalf("frame=%v err=%v", frame, err)
	}
	if f.typeCalls != 1 || len(f.types) != 1 {
		t.Fatalf("set types=%d remaining types=%d", f.typeCalls, len(f.types))
	}
}

func TestEncoderStreamChangeRejectsChangedDimensions(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE)
	if hr := f.types[0].SetUINT64(mf.MF_MT_FRAME_SIZE, uint64(1920)<<32|1080); d3d.HRESULT(hr).Failed() {
		t.Fatal(d3d.HRESULT(hr))
	}
	_, err := f.enc.processOutput()
	if err == nil || !strings.Contains(err.Error(), "no compatible H264 output type") || f.typeCalls != 0 {
		t.Fatalf("set types=%d err=%v", f.typeCalls, err)
	}
}

func TestEncoderStreamChangeCanNeedMoreInput(t *testing.T) {
	f := newOutputFixture(t, mf.MF_E_TRANSFORM_STREAM_CHANGE, mf.MF_E_TRANSFORM_NEED_MORE_INPUT)
	frame, err := f.enc.encodeSync(nil)
	if frame != nil || err != nil || f.inputCalls != 1 || f.outputCalls != 2 {
		t.Fatalf("frame=%v err=%v inputs=%d outputs=%d", frame, err, f.inputCalls, f.outputCalls)
	}
}

func TestEncoderOutputOtherStatuses(t *testing.T) {
	for _, hr := range []uint32{mf.MF_E_TRANSFORM_NEED_MORE_INPUT, 0x80004005} {
		t.Run(fmt.Sprintf("%x", hr), func(t *testing.T) {
			f := newOutputFixture(t, hr)
			frame, err := f.enc.processOutput()
			if frame != nil || (err != nil) != (hr != mf.MF_E_TRANSFORM_NEED_MORE_INPUT) {
				t.Fatalf("frame=%v err=%v", frame, err)
			}
			if f.typeCalls != 0 || f.sampleReleases != 1 || f.eventReleases != 1 {
				t.Fatalf("types=%d samples=%d events=%d", f.typeCalls, f.sampleReleases, f.eventReleases)
			}
		})
	}
}
