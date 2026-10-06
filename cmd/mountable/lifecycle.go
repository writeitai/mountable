package main

// What ends a mount, and how the mount ends for each.

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// shutdownTimeout bounds an ordinary shutdown as a whole: the clean unmount,
// serving stopping, and pending writes.
const shutdownTimeout = 30 * time.Second

type event int

const (
	// eventUnmounted: the mount went away underneath (mountable unmount,
	// umount); serving has stopped.
	eventUnmounted event = iota + 1
	// eventSignal: SIGINT or SIGTERM.
	eventSignal
	// eventRevoked: the API refused the session (a refused renewal, or a
	// gateway rejection the API confirmed).
	eventRevoked
	// eventExpired: the certificate ran out without a renewal; the session
	// has ended.
	eventExpired
)

type ending int

const (
	// endFinish: wait (bounded) for pending writes, drop caches, exit.
	endFinish ending = iota + 1
	// endUnmount: unmount cleanly, then finish; abort if that fails or runs
	// out of time.
	endUnmount
	// endAbort: abort the connection at once, without flushing: the gateway
	// no longer accepts the writes, and cached data must stop being served.
	endAbort
)

func endingFor(e event) ending {
	switch e {
	case eventUnmounted:
		return endFinish
	case eventSignal:
		return endUnmount
	default:
		return endAbort
	}
}

// mountEngine is what the lifecycle needs from a running mount.
type mountEngine interface {
	mountPoint() string
	// served is closed once the FUSE server stops serving.
	served() <-chan struct{}
	// unmount unmounts cleanly and waits for serving to stop.
	unmount() error
	// flush waits for pending writes and drops the local caches.
	flush()
	// abort ends the mount at once; an error means it was not fully done.
	abort() error
}

// lifecycle decides how a running mount ends. Nothing it waits on can be
// held up by the network: renewal and the gateway check run in their own
// worker and only report here.
type lifecycle struct {
	e        mountEngine
	signals  <-chan os.Signal
	revoked  <-chan struct{}
	renewed  <-chan struct{}
	notAfter func() time.Time
	timeout  time.Duration // for the ordinary shutdown
}

func (l *lifecycle) run() error {
	ev := l.wait()
	switch endingFor(ev) {
	case endFinish:
		return l.shutdown(false)
	case endUnmount:
		return l.shutdown(true)
	default:
		return l.abort(reason(ev))
	}
}

// wait returns the first event that ends the mount.
func (l *lifecycle) wait() event {
	expiry := time.NewTimer(time.Until(l.notAfter()))
	defer expiry.Stop()
	for {
		select {
		case <-l.e.served():
			return eventUnmounted
		case <-l.signals:
			return eventSignal
		case <-l.revoked:
			return eventRevoked
		case <-expiry.C:
			return eventExpired
		case <-l.renewed:
			expiry.Reset(time.Until(l.notAfter()))
		}
	}
}

// shutdown ends the mount the ordinary way within l.timeout: unmount
// cleanly (unless the mount is already gone), wait for serving to stop and
// for pending writes. Failure, the deadline, a second signal, revocation or
// expiry turn it into an abort.
func (l *lifecycle) shutdown(unmount bool) error {
	done := make(chan error, 1)
	go func() {
		if unmount {
			if err := l.e.unmount(); err != nil {
				done <- err
				return
			}
		}
		l.e.flush()
		done <- nil
	}()
	deadline := time.NewTimer(l.timeout)
	defer deadline.Stop()
	expiry := time.NewTimer(time.Until(l.notAfter()))
	defer expiry.Stop()
	for {
		select {
		case err := <-done:
			if err == nil {
				return nil
			}
			return l.abort(fmt.Sprintf("%s could not be unmounted cleanly (%s)", l.e.mountPoint(), oneLine(err)))
		case <-deadline.C:
			return l.abort(fmt.Sprintf("the mount did not shut down within %s", l.timeout))
		case <-l.signals:
			return l.abort("a second signal arrived during shutdown")
		case <-l.revoked:
			return l.abort(reason(eventRevoked))
		case <-expiry.C:
			return l.abort(reason(eventExpired))
		case <-l.renewed:
			expiry.Reset(time.Until(l.notAfter()))
		}
	}
}

// abort ends the mount at once, without flushing, and returns the error the
// command exits with.
func (l *lifecycle) abort(why string) error {
	select {
	case <-l.e.served():
		// Serving has stopped: the mount is already gone.
		return fmt.Errorf("%s; writes not yet committed may be lost", why)
	default:
	}
	fmt.Fprintf(os.Stderr, "mountable: %s; aborting the mount\n", why)
	if err := l.e.abort(); err != nil {
		return fmt.Errorf("%s, and the mount could not be fully aborted (%s): it ends when this process exits; writes not yet committed may be lost", why, oneLine(err))
	}
	return fmt.Errorf("%s; the mount was aborted and writes not yet committed may be lost", why)
}

func reason(ev event) string {
	if ev == eventExpired {
		return "the mount session expired"
	}
	return "the mount session was revoked"
}

// abortMount aborts the FUSE connection behind dir, then detaches dir. An
// error means the connection was not aborted (a lazy detach alone leaves
// open files working) or dir was not detached.
func abortMount(dir string) error {
	var errs []error
	if err := abortConnection(dir); err != nil {
		errs = append(errs, fmt.Errorf("aborting the FUSE connection: %w", err))
	}
	if err := detach(dir); err != nil {
		errs = append(errs, fmt.Errorf("detaching: %w", err))
	}
	return errors.Join(errs...)
}
