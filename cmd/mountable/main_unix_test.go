//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// Without takeOverSignals the engine's handler would os.Exit(0) here, which
// fails the test.
func TestSignalsReachTheCLI(t *testing.T) {
	takeOverSignals()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	defer signal.Stop(signals)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not arrive")
	}
	time.Sleep(200 * time.Millisecond) // room for any other handler to act
}
