package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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

func TestEngineLineShowsOnlyWhatNeedsAttentionUnlessVerbose(t *testing.T) {
	quiet := &engineOutput{root: "/c/fs1", dir: "/home/me/work"}
	verbose := &engineOutput{verbose: true, root: "/c/fs1", dir: "/home/me/work"}
	for _, c := range []struct {
		line string
		out  *engineOutput
		want string
	}{
		{"I1101 11:00:07.259767 weedfs.go:307 distributed lock manager enabled for mount\n", quiet, ""},
		{"I1101 11:13:54.138792 retry.go:164 retry filer grpc: err: rpc error: code = Unavailable desc = error reading from server: read: connection reset by peer\n", quiet, ""},
		{"W1101 11:13:54.138792 weedfs.go:495 meta events follow retry from 2026-11-01: EOF\n", quiet, ""},
		{"W1101 11:13:54.138792 weedfs_file_sync.go:214 /c/fs1/a fh 3 flush: retrying metadata flush (attempt 1/3) after 1s: EOF\n", quiet, ""},
		{"E1101 11:13:54.138792 weedfs_dir_read.go:359 dir ReadDirAll /c/fs1/x: rpc error\n", quiet, ""},
		{"2026/11/01 11:00:07 writer: Write/Writev failed\n", quiet, ""},
		{"E1101 11:13:54.138792 weedfs_file_sync.go:182 /c/fs1/notes/a b.txt doFlush: upload data: rpc error: code = ResourceExhausted desc = entry quota exceeded\n", quiet,
			"mountable: could not save /home/me/work/notes/a b.txt: entry quota exceeded\n"},
		{"E1101 11:13:54.138792 weedfs_file_sync.go:217 /c/fs1/a.txt fh 7 flush: rpc error: code = ResourceExhausted desc = filesystem_spending_cap_reached: the filesystem's monthly spending cap is reached\n", quiet,
			"mountable: could not save /home/me/work/a.txt: the filesystem's monthly spending cap is reached\n"},
		// Logged within a flush that may still retry and succeed.
		{"E1101 11:13:54.138792 weedfs_file_sync.go:342 fh flush create /c/fs1/b.txt: disk quota exceeded\n", quiet, ""},
		{"W1101 11:13:54.138792 weedfs_file_sync.go:467 flush /c/fs1/c.txt fh 9: the path no longer leads to the open file, not writing it\n", quiet,
			"mountable: could not save /home/me/work/c.txt: it was replaced or removed elsewhere while open\n"},
		{"W1101 11:13:54.138792 weedfs_file_sync.go:473 flush /c/fs1/c.txt fh 9: the handle holds another object than its inode's, not writing it\n", quiet,
			"mountable: could not save /home/me/work/c.txt: it was replaced or removed elsewhere while open\n"},
		{"W1101 11:13:54.138792 weedfs_file_sync.go:520 flush /c/fs1/c.txt fh 9: the filer has another object there, not writing it\n", quiet,
			"mountable: could not save /home/me/work/c.txt: it was replaced or removed elsewhere while open\n"},
		{"E1101 11:13:54.138792 weedfs_file_sync.go:182 /c/fs10/a doFlush: EOF\n", quiet,
			"mountable: could not save /c/fs10/a: EOF\n"},
		{"W1101 11:13:54.138792 weedfs_file_io.go:153 release fh 4 inode 99: fallback flush failed: 5=input/output error\n", quiet, ""},
		{"I1101 11:13:54.138792 metadata_rate_wait.go:88 create /c/fs1/node_modules/a.js: waiting at the filesystem's metadata rate\n", quiet,
			"mountable: the filesystem's metadata rate is reached; changes wait until it allows them\n"},
		{"I1101 11:13:54.138792 metadata_rate_wait.go:101 create /c/fs1/node_modules/a.js: sent after 12s at the metadata rate\n", quiet, ""},
		{"F1101 11:13:54.138792 filehandle.go:156 setting file handle entry to nil\n", quiet,
			"mountable: the mount stopped on an internal error; run it with --verbose for details\n"},
		{"I1101 11:00:07.259767 weedfs.go:307 distributed lock manager enabled for mount\n", verbose, "I1101 11:00:07.259767 weedfs.go:307 distributed lock manager enabled for mount\n"},
		{"2026/11/01 11:00:07 writer: Write/Writev failed\n", verbose, "2026/11/01 11:00:07 writer: Write/Writev failed\n"},
	} {
		if got := engineLine(c.line, c.out); got != c.want {
			t.Errorf("engineLine(%q, %+v) = %q, want %q", c.line, c.out, got, c.want)
		}
	}
}

// The engine messages engineLine translates are the engine's own: an engine
// upgrade that rewords one fails here instead of silently hiding it.
func TestEngineMessagesStillExist(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/seaweedfs/seaweedfs").Output()
	if err != nil {
		t.Skipf("the engine's source is not available: %v", err)
	}
	mount := filepath.Join(strings.TrimSpace(string(out)), "weed", "mount")
	for file, formats := range map[string][]string{
		// A failed flush at release goes through doFlush, which logs it
		// with the path before Release's own warning.
		"weedfs_file_sync.go": {
			`"%v doFlush: %v", fileFullPath`, `"%v fh %d flush: %v", fileFullPath`,
			`"flush %s fh %d: the path no longer leads to the open file, not writing it", path`,
			`"flush %s fh %d: the handle holds another object than its inode's, not writing it", path`,
			`"flush %s fh %d: the filer has another object there, not writing it", path`,
		},
		"weedfs_file_io.go":     {`wfs.doFlush(waitingAtMetadataRate(context.Background(), nil), fh, in.Uid, in.Gid, allowAsync)`},
		"metadata_rate_wait.go": {`"%s: waiting at the filesystem's metadata rate", what`},
	} {
		source, err := os.ReadFile(filepath.Join(mount, file))
		if err != nil {
			t.Fatal(err)
		}
		for _, format := range formats {
			if !strings.Contains(string(source), format) {
				t.Errorf("%s no longer logs %s", file, format)
			}
		}
	}
}

// A line repeating the one before it stays quiet while it keeps coming
// within repeatQuiet.
func TestRepeatsAreQuiet(t *testing.T) {
	var q quieter
	now := time.Now()
	for i, c := range []struct {
		line  string
		after time.Duration
		fresh bool
	}{
		{"a", 0, true}, {"a", 5 * time.Second, false}, {"a", 14 * time.Second, false},
		{"b", 15 * time.Second, true}, {"a", 16 * time.Second, true}, {"a", 30 * time.Second, true},
	} {
		if got := q.fresh(c.line, now.Add(c.after)); got != c.fresh {
			t.Errorf("%d: fresh(%q) = %v, want %v", i, c.line, got, c.fresh)
		}
	}
}

func TestGist(t *testing.T) {
	for message, want := range map[string]string{
		"rpc error: code = ResourceExhausted desc = entry quota exceeded":                                                "entry quota exceeded",
		`connection error: desc = "transport: Error while dialing: dial tcp 1.2.3.4:18889: connect: connection refused"`: "connection refused",
		"error reading from server: read tcp 10.0.0.1:5->10.0.0.2:18889: read: connection reset by peer":                 "connection reset by peer",
		"disk quota exceeded": "disk quota exceeded",
	} {
		if got := gist(message); got != want {
			t.Errorf("gist(%q) = %q, want %q", message, got, want)
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
