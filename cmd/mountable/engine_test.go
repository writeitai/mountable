package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// stallingFiler is a gateway that answers the first startup calls and then
// never answers the read of the cell's configuration, ignoring the caller's
// context as the engine's own call does.
type stallingFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	stalled chan struct{} // receives once the configuration read is stuck
	release chan struct{}
}

func (f *stallingFiler) GetFilerConfiguration(context.Context, *filer_pb.GetFilerConfigurationRequest) (*filer_pb.GetFilerConfigurationResponse, error) {
	return &filer_pb.GetFilerConfigurationResponse{}, nil
}

func (f *stallingFiler) CreateEntry(context.Context, *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	return &filer_pb.CreateEntryResponse{}, nil
}

func (f *stallingFiler) LookupDirectoryEntry(_ context.Context, r *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	if r.Directory == "/etc/seaweedfs" {
		f.stalled <- struct{}{}
		<-f.release
	}
	return nil, status.Error(codes.NotFound, "no entry")
}

// A gateway that stalls a startup call the engine makes without a deadline
// must not keep a signal from ending the startup, and nothing is mounted.
func TestSignalEndsAStalledStartup(t *testing.T) {
	ca := newTestCA(t)
	source := &certSource{}
	source.set(ca.sessionCert(t, "session", newKey(t), time.Now().Add(time.Hour)))
	config, err := source.tlsConfig(ca.pem)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if port <= 10000 {
		t.Skip("the gateway address needs a gRPC port above 10000")
	}
	filer := &stallingFiler{stalled: make(chan struct{}, 1), release: make(chan struct{})}
	server := grpc.NewServer(grpc.Creds(grpccredentials.NewTLS(ca.serverTLS(t))))
	filer_pb.RegisterSeaweedFilerServer(server, filer)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(func() { close(filer.release) })

	dir := t.TempDir()
	signals := make(chan os.Signal, 1)
	result := make(chan error, 1)
	go func() {
		_, err := untilSignal(signals, func(ctx context.Context) error {
			_, err := startEngine(ctx, mountConfig{
				gateway:  fmt.Sprintf("localhost:%d", port-10000), // gRPC on port
				root:     "/c/fs1",
				dir:      dir,
				tls:      config,
				cacheDir: t.TempDir(),
			})
			return err
		})
		result <- err
	}()
	select {
	case <-filer.stalled:
	case <-time.After(30 * time.Second):
		t.Fatal("startup never reached the configuration read")
	}
	signals <- syscall.SIGINT
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("startup ended with %v, want cancellation before mounting", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled startup ignored the signal")
	}
}

// With --verbose the engine's own log, which it writes to os.Stderr, reaches
// stderr with secrets removed: here its parse error for a gateway address
// holding a token. Without it, that log line is not shown.
func TestEngineLogIsRedacted(t *testing.T) {
	for _, verbose := range []bool{true, false} {
		buf := captureDiagnostics(t)
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		_, err := startEngine(ctx, mountConfig{gateway: testJWT + ":abc", root: "/c/fs1", dir: t.TempDir(), cacheDir: t.TempDir(), verbose: verbose})
		cancel()
		flushEngineLog()
		logged := buf.String()
		shown := strings.Contains(logged, "server address mtblat_[redacted]:abc parse error")
		if err == nil || shown != verbose || strings.Contains(logged, "eyJ") {
			t.Fatalf("verbose %v: err %v, engine log %q", verbose, err, logged)
		}
	}
}

func TestEngineLineShowsOnlyFatalErrorsUnlessVerbose(t *testing.T) {
	for _, c := range []struct {
		line    string
		verbose bool
		want    string
	}{
		{"I1101 11:00:07.259767 weedfs.go:307 distributed lock manager enabled for mount\n", false, ""},
		{"I1101 11:13:54.138792 retry.go:164 retry filer grpc: err: rpc error: code = Unavailable\n", false, ""},
		{"W1101 11:13:54.138792 weedfs.go:495 meta events follow retry from 2026-11-01: EOF\n", false, ""},
		{"E1101 11:13:54.138792 weedfs_dir_read.go:359 dir ReadDirAll /x: rpc error\n", false, ""},
		{"F1101 11:13:54.138792 filehandle.go:156 setting file handle entry to nil\n", false, "mountable: fatal error: setting file handle entry to nil\n"},
		{"2026/11/01 11:00:07 writer: Write/Writev failed\n", false, ""},
		{"I1101 11:00:07.259767 weedfs.go:307 distributed lock manager enabled for mount\n", true, "I1101 11:00:07.259767 weedfs.go:307 distributed lock manager enabled for mount\n"},
		{"2026/11/01 11:00:07 writer: Write/Writev failed\n", true, "2026/11/01 11:00:07 writer: Write/Writev failed\n"},
	} {
		if got := engineLine(c.line, c.verbose); got != c.want {
			t.Errorf("engineLine(%q, %v) = %q, want %q", c.line, c.verbose, got, c.want)
		}
	}
}

func TestUnlessCancelledStopsWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	never := make(chan struct{})
	defer close(never)
	if err := unlessCancelled(ctx, func() error { <-never; return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := unlessCancelled(ctx, func() error { <-never; return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if err := unlessCancelled(context.Background(), func() error { return errors.New("x") }); err == nil || err.Error() != "x" {
		t.Fatalf("got %v", err)
	}
}
