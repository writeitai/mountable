package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestEndingFor(t *testing.T) {
	cases := map[event]ending{
		eventUnmounted: endFinish,
		eventSignal:    endUnmount,
		eventRevoked:   endAbort,
		eventExpired:   endAbort,
	}
	for ev, want := range cases {
		if got := endingFor(ev); got != want {
			t.Errorf("endingFor(%d) = %d, want %d", ev, got, want)
		}
	}
}

// fakeEngine records what the lifecycle did to it.
type fakeEngine struct {
	done        chan struct{}
	unmountFn   func() error
	abortErr    error
	aborted     atomic.Bool
	flushed     atomic.Bool
	unmountCall atomic.Bool
}

func newFakeEngine() *fakeEngine { return &fakeEngine{done: make(chan struct{})} }

func (f *fakeEngine) mountPoint() string      { return "/mnt/f" }
func (f *fakeEngine) served() <-chan struct{} { return f.done }
func (f *fakeEngine) flush()                  { f.flushed.Store(true) }
func (f *fakeEngine) abort() error            { f.aborted.Store(true); return f.abortErr }
func (f *fakeEngine) unmount() error {
	f.unmountCall.Store(true)
	if f.unmountFn != nil {
		return f.unmountFn()
	}
	close(f.done)
	return nil
}

type harness struct {
	l        *lifecycle
	e        *fakeEngine
	signals  chan os.Signal
	revoked  chan struct{}
	renewed  chan struct{}
	notAfter atomic.Pointer[time.Time]
}

func newHarness(validFor time.Duration) *harness {
	h := &harness{
		e:       newFakeEngine(),
		signals: make(chan os.Signal, 2),
		revoked: make(chan struct{}, 1),
		renewed: make(chan struct{}, 1),
	}
	h.setNotAfter(time.Now().Add(validFor))
	h.l = &lifecycle{
		e: h.e, signals: h.signals, revoked: h.revoked, renewed: h.renewed,
		notAfter: func() time.Time { return *h.notAfter.Load() },
		timeout:  200 * time.Millisecond,
	}
	return h
}

func (h *harness) setNotAfter(t time.Time) { h.notAfter.Store(&t) }

// runAsync runs the lifecycle and returns its result channel.
func (h *harness) runAsync() <-chan error {
	result := make(chan error, 1)
	go func() { result <- h.l.run() }()
	return result
}

func await(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the lifecycle did not end")
		return nil
	}
}

func TestSignalUnmountsCleanly(t *testing.T) {
	h := newHarness(time.Hour)
	h.signals <- syscall.SIGTERM
	if err := await(t, h.runAsync()); err != nil {
		t.Fatal(err)
	}
	if !h.e.unmountCall.Load() || !h.e.flushed.Load() || h.e.aborted.Load() {
		t.Fatal("expected unmount and flush without abort")
	}
}

func TestExternalUnmountOnlyFlushes(t *testing.T) {
	h := newHarness(time.Hour)
	close(h.e.done)
	if err := await(t, h.runAsync()); err != nil {
		t.Fatal(err)
	}
	if h.e.unmountCall.Load() || !h.e.flushed.Load() || h.e.aborted.Load() {
		t.Fatal("expected only a flush")
	}
}

func TestRevocationAborts(t *testing.T) {
	h := newHarness(time.Hour)
	h.revoked <- struct{}{}
	err := await(t, h.runAsync())
	if err == nil || !strings.Contains(err.Error(), "revoked") || !h.e.aborted.Load() || h.e.flushed.Load() {
		t.Fatalf("expected an abort without flush, got %v", err)
	}
}

func TestExpiryAbortsOnItsOwnTimer(t *testing.T) {
	h := newHarness(50 * time.Millisecond)
	err := await(t, h.runAsync())
	if err == nil || !strings.Contains(err.Error(), "expired") || !h.e.aborted.Load() {
		t.Fatalf("expected an abort on expiry, got %v", err)
	}
}

func TestRenewalRearmsExpiry(t *testing.T) {
	h := newHarness(150 * time.Millisecond)
	result := h.runAsync()
	time.Sleep(20 * time.Millisecond)
	h.setNotAfter(time.Now().Add(time.Hour))
	h.renewed <- struct{}{}
	time.Sleep(300 * time.Millisecond)
	if h.e.aborted.Load() {
		t.Fatal("expired despite the renewal")
	}
	h.signals <- syscall.SIGTERM
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
}

// The worker is stuck (nothing ever arrives on revoked); signals still end
// the mount at once.
func TestSignalsDoNotWaitForTheNetwork(t *testing.T) {
	h := newHarness(time.Hour)
	result := h.runAsync()
	start := time.Now()
	h.signals <- syscall.SIGINT
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the signal waited")
	}
}

func TestStuckUnmountAbortsAtTheDeadline(t *testing.T) {
	h := newHarness(time.Hour)
	h.e.unmountFn = func() error { select {} } // a request the gateway never answers
	h.signals <- syscall.SIGTERM
	err := await(t, h.runAsync())
	if err == nil || !strings.Contains(err.Error(), "did not shut down") || !h.e.aborted.Load() {
		t.Fatalf("expected an abort at the deadline, got %v", err)
	}
}

func TestEventsDuringShutdownAbort(t *testing.T) {
	cases := map[string]func(h *harness){
		"second signal": func(h *harness) { h.signals <- syscall.SIGTERM },
		"revoked":       func(h *harness) { h.revoked <- struct{}{} },
		"expired":       func(h *harness) { h.setNotAfter(time.Now()); h.renewed <- struct{}{} },
	}
	for name, during := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(time.Hour)
			h.l.timeout = time.Hour
			blocked := make(chan struct{})
			h.e.unmountFn = func() error { close(blocked); select {} }
			h.signals <- syscall.SIGTERM
			result := h.runAsync()
			<-blocked
			during(h)
			if err := await(t, result); err == nil || !h.e.aborted.Load() {
				t.Fatalf("expected an abort, got %v", err)
			}
		})
	}
}

func TestFailedCleanUnmountAborts(t *testing.T) {
	h := newHarness(time.Hour)
	h.e.unmountFn = func() error { return errors.New("device or\nresource busy") }
	h.signals <- syscall.SIGTERM
	err := await(t, h.runAsync())
	if err == nil || !strings.Contains(err.Error(), "could not be unmounted cleanly (device or resource busy)") || !h.e.aborted.Load() {
		t.Fatalf("expected an abort, got %v", err)
	}
}

// Diagnostics written outside a command's output are redacted.
func TestDiagnosticsAreRedacted(t *testing.T) {
	var buf syncBuffer
	saved := diagnostics
	diagnostics = &buf
	defer func() { diagnostics = saved }()
	h := newHarness(time.Hour)
	h.e.unmountFn = func() error { return errors.New("busy with " + testJWT) }
	h.signals <- syscall.SIGTERM
	err := await(t, h.runAsync())
	if err == nil || !strings.Contains(buf.String(), "(busy with mtblat_[redacted]); aborting the mount") || strings.Contains(buf.String(), "eyJ") {
		t.Fatalf("diagnostics %q, err %v", buf.String(), err)
	}
}

func TestAFailedAbortIsReported(t *testing.T) {
	h := newHarness(time.Hour)
	h.e.abortErr = errors.New("aborting the FUSE connection: no abort file")
	h.revoked <- struct{}{}
	err := await(t, h.runAsync())
	if err == nil || !strings.Contains(err.Error(), "could not be fully aborted (aborting the FUSE connection: no abort file)") {
		t.Fatalf("the failed abort was not reported: %v", err)
	}
}

func TestNoAbortOnceServingStopped(t *testing.T) {
	h := newHarness(time.Hour)
	h.e.unmountFn = func() error { close(h.e.done); select {} } // unmounted, flush stuck
	h.signals <- syscall.SIGTERM
	if err := await(t, h.runAsync()); err == nil || h.e.aborted.Load() {
		t.Fatalf("expected an error without an abort, got %v", err)
	}
}

func TestUntilSignalCancelsStartup(t *testing.T) {
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGINT
	interrupted, err := untilSignal(signals, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !interrupted || !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, %v", interrupted, err)
	}
	interrupted, err = untilSignal(make(chan os.Signal), func(context.Context) error { return nil })
	if interrupted || err != nil {
		t.Fatalf("got %v, %v", interrupted, err)
	}
}

func TestEngineLogsToStderr(t *testing.T) {
	if err := logToStderr(); err != nil {
		t.Fatal(err)
	}
}
