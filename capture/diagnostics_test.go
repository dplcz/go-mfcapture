package capture

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"runtime"
	"structs"
	"syscall"
	"testing"
	"unsafe"

	d3d "github.com/kirides/go-d3d"
	"github.com/kirides/go-d3d/d3d11"
	"github.com/kirides/go-d3d/dxgi"
)

type diagnosticDuplication struct {
	_    structs.HostLayout
	vtbl *dxgi.IDXGIOutputDuplicationVtbl
}

func captureDiagnosticLogs(t *testing.T, level slog.Level) *bytes.Buffer {
	t.Helper()
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: level})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &output
}

func TestReleaseDiagnosticsPreserveCallsAndState(t *testing.T) {
	for _, releaseHR := range []d3d.HRESULT{d3d.S_OK, d3d.DXGI_ERROR_ACCESS_LOST} {
		t.Run(releaseHR.Error(), func(t *testing.T) {
			output := captureDiagnosticLogs(t, slog.LevelDebug)
			releaseCalls, deviceQueries := 0, 0
			var releaseSelf, deviceSelf uintptr
			dupVtbl := dxgi.IDXGIOutputDuplicationVtbl{ReleaseFrame: syscall.NewCallback(func(self uintptr) uintptr {
				releaseCalls++
				releaseSelf = self
				return uintptr(releaseHR)
			})}
			devVtbl := d3d11.ID3D11DeviceVtbl{GetDeviceRemovedReason: syscall.NewCallback(func(self uintptr) uintptr {
				deviceQueries++
				deviceSelf = self
				return uintptr(d3d.DXGI_ERROR_DEVICE_RESET)
			})}
			dupObj := diagnosticDuplication{vtbl: &dupVtbl}
			devObj := diagnosticDevice{vtbl: &devVtbl}
			d := Duplicator{
				device:            (*d3d11.ID3D11Device)(unsafe.Pointer(&devObj)),
				outputDuplication: (*dxgi.IDXGIOutputDuplication)(unsafe.Pointer(&dupObj)),
				acquiredFrame:     true,
				diag: duplicationDiagnostics{
					id: 7, generation: 3, acquireSeq: 42,
				},
			}
			d.releaseFrame()
			d.releaseFrame()
			if releaseCalls != 1 || d.acquiredFrame {
				t.Fatalf("release calls=%d, local ownership=%v; want one call and false", releaseCalls, d.acquiredFrame)
			}
			if releaseSelf != uintptr(unsafe.Pointer(&dupObj)) {
				t.Fatal("ReleaseFrame received an incorrect COM object pointer")
			}
			wantQueries := 0
			if releaseHR.Failed() {
				wantQueries = 1
				if deviceSelf != uintptr(unsafe.Pointer(&devObj)) {
					t.Fatal("GetDeviceRemovedReason received an incorrect COM object pointer")
				}
			}
			if deviceQueries != wantQueries {
				t.Fatalf("device queries=%d, want %d", deviceQueries, wantQueries)
			}
			decoder := json.NewDecoder(output)
			var releaseLog map[string]any
			if err := decoder.Decode(&releaseLog); err != nil {
				t.Fatal(err)
			}
			if releaseLog["operation"] != "ReleaseFrame" || releaseLog["local_owns_frame"] != true || releaseLog["local_owns_frame_after"] != false {
				t.Fatalf("incorrect release diagnostic: %v", releaseLog)
			}
			if releaseHR.Failed() && (releaseLog["level"] != "INFO" || releaseLog["hr"] != "0x887a0026" || releaseLog["device_removed_hr"] != "0x887a0007") {
				t.Fatalf("missing failure HRESULTs: %v", releaseLog)
			}
			// A later acquire failure must retain the prior release's identity and
			// result, even after the local ownership flag has been cleared.
			d.diag.acquireSeq++
			d.logDXGI(slog.LevelWarn, "AcquireNextFrame", d3d.DXGI_ERROR_INVALID_CALL)
			var acquireLog map[string]any
			if err := decoder.Decode(&acquireLog); err != nil {
				t.Fatal(err)
			}
			if acquireLog["capture_id"] != float64(7) || acquireLog["generation"] != float64(3) || acquireLog["acquire_seq"] != float64(43) || acquireLog["last_release_seq"] != float64(42) || acquireLog["last_release_generation"] != float64(3) || acquireLog["last_release_hr"] != releaseLog["hr"] {
				t.Fatalf("prior release correlation lost: %v", acquireLog)
			}
			runtime.KeepAlive(&dupObj)
			runtime.KeepAlive(&devObj)
		})
	}
}

func TestDisabledDiagnosticDoesNotQueryDevice(t *testing.T) {
	output := captureDiagnosticLogs(t, slog.LevelInfo)
	// An invalid non-nil pointer would crash if a disabled diagnostic tried to
	// query the device. The logger must return before accessing the COM vtable.
	obj := diagnosticDevice{}
	d := Duplicator{device: (*d3d11.ID3D11Device)(unsafe.Pointer(&obj))}
	d.logDXGI(slog.LevelDebug, "DuplicateOutput1", d3d.DXGI_ERROR_UNSUPPORTED)
	if output.Len() != 0 {
		t.Fatalf("disabled debug diagnostic produced output: %s", output)
	}
	runtime.KeepAlive(&obj)
}
