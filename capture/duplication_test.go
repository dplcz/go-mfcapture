package capture

import (
	"errors"
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

type testCOMObject struct {
	_    structs.HostLayout
	vtbl unsafe.Pointer
}

// COM callbacks exercise the actual AcquireFrame/deferred ReleaseFrame and
// reinitialization paths without requiring a desktop, driver, or encoder.
type duplicationFixture struct {
	d Duplicator

	oldAcquires, oldFrameReleases, oldReleases int
	newAcquires                                int
	createAttempts, deniedAttempts             int
	objects                                    []*testCOMObject
}

func newDuplicationFixture(t *testing.T, releaseHR d3d.HRESULT) *duplicationFixture {
	t.Helper()
	captureDiagnosticLogs(t, slog.LevelError)
	f := &duplicationFixture{}
	object := func(vtbl unsafe.Pointer) *testCOMObject {
		o := &testCOMObject{vtbl: vtbl}
		f.objects = append(f.objects, o)
		return o
	}
	release := syscall.NewCallback(func(uintptr) uintptr { return 1 })
	resourceVtbl := &dxgi.IDXGIResourceVtbl{}
	resourceVtbl.Release = release
	resource := object(unsafe.Pointer(resourceVtbl))
	acquire := func(counter *int) uintptr {
		return syscall.NewCallback(func(_ uintptr, _ uintptr, info, desktop uintptr) uintptr {
			*counter++
			// A pointer-only frame still needs ReleaseFrame, but avoids texture
			// copying and allows the regression to isolate duplication ownership.
			*(*dxgi.DXGI_OUTDUPL_FRAME_INFO)(unsafe.Pointer(info)) = dxgi.DXGI_OUTDUPL_FRAME_INFO{}
			*(**dxgi.IDXGIResource)(unsafe.Pointer(desktop)) = (*dxgi.IDXGIResource)(unsafe.Pointer(resource))
			return 0
		})
	}
	oldVtbl := &dxgi.IDXGIOutputDuplicationVtbl{}
	oldVtbl.AcquireNextFrame = acquire(&f.oldAcquires)
	oldVtbl.ReleaseFrame = syscall.NewCallback(func(uintptr) uintptr {
		f.oldFrameReleases++
		return uintptr(releaseHR)
	})
	oldVtbl.Release = syscall.NewCallback(func(uintptr) uintptr {
		f.oldReleases++
		return 0
	})
	oldDup := object(unsafe.Pointer(oldVtbl))
	newVtbl := &dxgi.IDXGIOutputDuplicationVtbl{}
	newVtbl.AcquireNextFrame = acquire(&f.newAcquires)
	newVtbl.ReleaseFrame = syscall.NewCallback(func(uintptr) uintptr { return 0 })
	newVtbl.Release = release
	newDup := object(unsafe.Pointer(newVtbl))
	create := func(result uintptr) uintptr {
		f.createAttempts++
		if f.createAttempts <= f.deniedAttempts {
			return uintptr(errAccessDenied)
		}
		*(**dxgi.IDXGIOutputDuplication)(unsafe.Pointer(result)) = (*dxgi.IDXGIOutputDuplication)(unsafe.Pointer(newDup))
		return 0
	}
	out5Vtbl := &dxgi.IDXGIOutput5Vtbl{}
	out5Vtbl.Release = release
	out5Vtbl.DuplicateOutput1 = syscall.NewCallback(func(_ uintptr, _ uintptr, _ uintptr, _ uintptr, _ uintptr, result uintptr) uintptr {
		return create(result)
	})
	out5Vtbl.GetDesc = syscall.NewCallback(func(_ uintptr, result uintptr) uintptr {
		*(*dxgi.DXGI_OUTPUT_DESC)(unsafe.Pointer(result)) = dxgi.DXGI_OUTPUT_DESC{
			DesktopCoordinates: dxgi.RECT{Right: 1920, Bottom: 1080},
		}
		return 0
	})
	out5 := object(unsafe.Pointer(out5Vtbl))
	out1Vtbl := &dxgi.IDXGIOutput1Vtbl{}
	out1Vtbl.Release = release
	out1Vtbl.DuplicateOutput = syscall.NewCallback(func(_ uintptr, _ uintptr, result uintptr) uintptr { return create(result) })
	out1 := object(unsafe.Pointer(out1Vtbl))
	outVtbl := &dxgi.IDXGIOutputVtbl{}
	outVtbl.Release = release
	qiCalls := 0
	outVtbl.QueryInterface = syscall.NewCallback(func(_ uintptr, _ uintptr, result uintptr) uintptr {
		qiCalls++
		if qiCalls%2 == 1 {
			*(*unsafe.Pointer)(unsafe.Pointer(result)) = unsafe.Pointer(out5)
		} else {
			*(*unsafe.Pointer)(unsafe.Pointer(result)) = unsafe.Pointer(out1)
		}
		return 0
	})
	out := object(unsafe.Pointer(outVtbl))
	adapterVtbl := &dxgi.IDXGIAdapter1Vtbl{}
	adapterVtbl.Release = release
	adapterVtbl.EnumOutputs = syscall.NewCallback(func(_ uintptr, _ uintptr, result uintptr) uintptr {
		*(*unsafe.Pointer)(unsafe.Pointer(result)) = unsafe.Pointer(out)
		// Each initialization first queries Output5, then Output1 on failure.
		qiCalls = 0
		return 0
	})
	adapter := object(unsafe.Pointer(adapterVtbl))
	dxgiDeviceVtbl := &dxgi.IDXGIDevice1Vtbl{}
	dxgiDeviceVtbl.Release = release
	dxgiDeviceVtbl.GetParent = syscall.NewCallback(func(_ uintptr, _ uintptr, result uintptr) uintptr {
		*(*unsafe.Pointer)(unsafe.Pointer(result)) = unsafe.Pointer(adapter)
		return 0
	})
	dxgiDevice := object(unsafe.Pointer(dxgiDeviceVtbl))
	deviceVtbl := &d3d11.ID3D11DeviceVtbl{}
	deviceVtbl.QueryInterface = syscall.NewCallback(func(_ uintptr, _ uintptr, result uintptr) uintptr {
		*(*unsafe.Pointer)(unsafe.Pointer(result)) = unsafe.Pointer(dxgiDevice)
		return 0
	})
	device := object(unsafe.Pointer(deviceVtbl))
	f.d = Duplicator{
		device:            (*d3d11.ID3D11Device)(unsafe.Pointer(device)),
		outputDuplication: (*dxgi.IDXGIOutputDuplication)(unsafe.Pointer(oldDup)),
		dxgiOutput:        (*dxgi.IDXGIOutput5)(unsafe.Pointer(out5)),
		// Matching texture dimensions prevent unrelated texture recreation.
		privTex: (*d3d11.ID3D11Texture2D)(unsafe.Pointer(resource)),
		width:   1920, height: 1080, privW: 1920, privH: 1080,
		diag: duplicationDiagnostics{id: 1, generation: 1},
	}
	t.Cleanup(func() { runtime.KeepAlive(f) })
	return f
}

func TestReleaseAccessLostRebuildsBeforeNextAcquire(t *testing.T) {
	f := newDuplicationFixture(t, d3d.DXGI_ERROR_ACCESS_LOST)
	if _, err := f.d.AcquireFrame(0); !errors.Is(err, ErrTimeout) {
		t.Fatalf("pointer-only frame: %v", err)
	}
	if f.d.frameReleaseHR != d3d.DXGI_ERROR_ACCESS_LOST {
		t.Fatal("deferred release lost its ACCESS_LOST result")
	}
	// DuplicateOutput1 and its fallback both fail on two attempts, just as
	// Windows can deny duplication throughout the secure desktop transition.
	f.deniedAttempts = 4
	for range 2 {
		if _, err := f.d.AcquireFrame(0); !errors.Is(err, ErrCaptureUnavailable) || !errors.Is(err, errAccessDenied) {
			t.Fatalf("locked desktop: %v", err)
		}
		if f.oldAcquires != 1 || f.oldFrameReleases != 1 || f.oldReleases != 1 || f.d.outputDuplication != nil {
			t.Fatal("lost duplication reused or released more than once")
		}
	}
	if _, err := f.d.AcquireFrame(0); !errors.Is(err, ErrAccessLostRecovered) {
		t.Fatalf("desktop recovery: %v", err)
	}
	if f.d.diag.generation != 2 || f.d.frameReleaseHR != d3d.S_OK || f.newAcquires != 0 {
		t.Fatal("recovery did not reset release state or signal the caller before acquiring")
	}
	if _, err := f.d.AcquireFrame(0); !errors.Is(err, ErrTimeout) {
		t.Fatalf("first frame after recovery: %v", err)
	}
	if f.newAcquires != 1 || f.oldAcquires != 1 {
		t.Fatal("acquisition did not resume exclusively on the new duplication")
	}
}

func TestReleaseSuccessKeepsDuplication(t *testing.T) {
	f := newDuplicationFixture(t, d3d.S_OK)
	for range 2 {
		if _, err := f.d.AcquireFrame(0); !errors.Is(err, ErrTimeout) {
			t.Fatal(err)
		}
	}
	if f.oldAcquires != 2 || f.oldFrameReleases != 2 || f.oldReleases != 0 || f.createAttempts != 0 {
		t.Fatal("successful release caused an unnecessary rebuild")
	}
}

func TestUnexpectedReleaseErrorStopsBeforeAcquire(t *testing.T) {
	f := newDuplicationFixture(t, d3d.DXGI_ERROR_INVALID_CALL)
	_, _ = f.d.AcquireFrame(0)
	if _, err := f.d.AcquireFrame(0); !errors.Is(err, d3d.DXGI_ERROR_INVALID_CALL) {
		t.Fatalf("unexpected release error: %v", err)
	}
	if f.oldAcquires != 1 || f.oldFrameReleases != 1 || f.createAttempts != 0 {
		t.Fatal("unexpected error was retried or masked by a rebuild")
	}
}
