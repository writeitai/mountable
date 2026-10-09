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

// The engine's own log, which it writes to os.Stderr, reaches stderr with
// secrets removed: here its parse error for a gateway address holding a
// token.
func TestEngineLogIsRedacted(t *testing.T) {
	buf := captureDiagnostics(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := startEngine(ctx, mountConfig{gateway: testJWT + ":abc", root: "/c/fs1", dir: t.TempDir(), cacheDir: t.TempDir()})
	flushEngineLog()
	logged := buf.String()
	if err == nil || !strings.Contains(logged, "server address mtblat_[redacted]:abc parse error") || strings.Contains(logged, "eyJ") {
		t.Fatalf("err %v, engine log %q", err, logged)
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
