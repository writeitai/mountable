package main

import (
	"bytes"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/writeitai/mountable"
)

func TestVersionPrintsOnlyTheCLIVersion(t *testing.T) {
	saved := version
	defer func() { version = saved }()
	version = "1.2.3"
	var out bytes.Buffer
	printVersion(&out)
	if out.String() != "mountable 1.2.3\n" {
		t.Fatalf("version printed %q", out.String())
	}
}

func TestLicensesPrintsTheEmbeddedNotices(t *testing.T) {
	var out bytes.Buffer
	printLicenses(&out)
	if out.String() != mountable.ThirdPartyNotices {
		t.Fatal("licenses did not print the embedded notices")
	}
	for _, want := range []string{"The Go standard library and runtime", "Apache License"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("notices lack %q", want)
		}
	}
}

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
