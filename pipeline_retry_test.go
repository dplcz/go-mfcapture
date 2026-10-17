package mfcapture

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestCaptureRetryWaitsBeforeNextAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if err := waitCaptureRetry(context.Background()); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != captureRetryDelay {
			t.Fatalf("retry delay = %v, want %v", elapsed, captureRetryDelay)
		}
	})
}

func TestCaptureRetryCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- waitCaptureRetry(ctx) }()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("retry error = %v, want context.Canceled", err)
			}
		default:
			t.Fatal("retry did not stop on cancellation")
		}
	})
}
