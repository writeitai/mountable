package main

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"golang.org/x/sys/windows"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// memFiler holds a few entries in memory: enough for the mount engine to
// look up, list, read inline content and report usage.
type memFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu      sync.Mutex
	entries map[string]*filer_pb.Entry // by directory + "/" + name
}

func (f *memFiler) LookupDirectoryEntry(_ context.Context, r *filer_pb.LookupDirectoryEntryRequest) (*filer_pb.LookupDirectoryEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if e, ok := f.entries[strings.TrimSuffix(r.Directory, "/")+"/"+r.Name]; ok {
		return &filer_pb.LookupDirectoryEntryResponse{Entry: e}, nil
	}
	return nil, status.Error(codes.NotFound, filer_pb.ErrNotFound.Error())
}

func (f *memFiler) ListEntries(r *filer_pb.ListEntriesRequest, stream grpc.ServerStreamingServer[filer_pb.ListEntriesResponse]) error {
	f.mu.Lock()
	var list []*filer_pb.Entry
	for key, e := range f.entries {
		if key[:strings.LastIndex(key, "/")] == strings.TrimSuffix(r.Directory, "/") &&
			(e.Name > r.StartFromFileName || e.Name == r.StartFromFileName && r.InclusiveStartFrom) {
			list = append(list, e)
		}
	}
	f.mu.Unlock()
	for _, e := range list {
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: e}); err != nil {
			return err
		}
	}
	return nil
}

func (f *memFiler) Statistics(context.Context, *filer_pb.StatisticsRequest) (*filer_pb.StatisticsResponse, error) {
	return &filer_pb.StatisticsResponse{TotalSize: 1 << 30, UsedSize: 1 << 20, FileCount: 1}, nil
}

// testEngine is the mount engine against an in-memory filer holding
// /hello.txt.
func testEngine(t *testing.T) *weedmount.WFS {
	t.Helper()
	now := time.Now()
	filer := &memFiler{entries: map[string]*filer_pb.Entry{
		"/c/fs1/hello.txt": {
			Name:    "hello.txt",
			Content: []byte("hello from the mount"),
			Attributes: &filer_pb.FuseAttributes{
				FileSize: 20, Mtime: now.Unix(), Crtime: now.Unix(),
				FileMode: 0o644, Uid: ownerID, Gid: ownerID, Inode: 4242,
			},
		},
	}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	filer_pb.RegisterSeaweedFilerServer(server, filer)
	go server.Serve(l)
	t.Cleanup(server.Stop)
	mapper, err := meta_cache.NewUidGidMapper("1000:1000", "1000:1000")
	if err != nil {
		t.Fatal(err)
	}
	cache := engineCacheDir(t)
	return weedmount.NewSeaweedFileSystem(&weedmount.Option{
		MountDirectory:     cache,
		FilerAddresses:     []pb.ServerAddress{pb.NewServerAddress("127.0.0.1", 1, l.Addr().(*net.TCPAddr).Port)},
		GrpcDialOption:     grpc.WithTransportCredentials(insecure.NewCredentials()),
		FilerMountRootPath: "/c/fs1",
		ChunkSizeLimit:     2 << 20,
		ConcurrentWriters:  8,
		ConcurrentReaders:  8,
		CacheDirForRead:    cache,
		CacheDirForWrite:   cache,
		CacheMetaTTlSec:    60,
		CacheDirMaxEntries: 1000,
		MountUid:           ownerID,
		MountGid:           ownerID,
		MountMode:          os.ModeDir | 0o755,
		MountCtime:         now,
		MountMtime:         now,
		Umask:              0o022,
		VolumeServerAccess: "filerProxy",
		UidGidMapper:       mapper,
		DefaultPermissions: true,
		EagerFilerCreate:   true,
	})
}

// requireWinFsp skips without WinFsp, unless CI (which installs it) asks
// for the test to run.
func requireWinFsp(t *testing.T) {
	if _, err := winfspInstallDir(); err != nil && os.Getenv("MOUNTABLE_TEST_WINFSP") == "" {
		t.Skip("WinFsp is not installed:", err)
	}
}

// freeDriveLetter is the last drive letter not in use.
func freeDriveLetter(t *testing.T) string {
	drives, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	for c := 'Z'; c >= 'G'; c-- {
		if drives&(1<<(c-'A')) == 0 {
			return string(c) + ":"
		}
	}
	t.Skip("no free drive letter")
	return ""
}

// A read-only session through WinFsp on a directory and on a drive
// letter: the root is readable once attach returns, files read back,
// writes are refused, canonicalDir leaves the mounted directory as it is,
// and unmounting removes it.
func TestWinFspMount(t *testing.T) {
	requireWinFsp(t)
	for name, dir := range map[string]string{
		"directory": filepath.Join(t.TempDir(), "mnt"),
		"drive":     freeDriveLetter(t),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			e, err := attach(ctx, testEngine(t), mountConfig{dir: dir, readOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			root := mountRoot(dir)
			got, err := os.ReadFile(filepath.Join(root, "hello.txt"))
			if err != nil || string(got) != "hello from the mount" {
				t.Errorf("reading hello.txt: %q, %v", got, err)
			}
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "hello.txt" {
				t.Errorf("listing the root: %v, %v", entries, err)
			}
			if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("x"), 0o644); err == nil {
				t.Error("a read-only mount accepted a new file")
			}
			if canonical, err := canonicalDir(dir); err != nil || !strings.EqualFold(canonical, dir) {
				t.Errorf("canonicalDir(%s) = %s, %v", dir, canonical, err)
			}
			if err := e.unmount(); err != nil {
				t.Fatalf("unmount: %v", err)
			}
			select {
			case <-e.served():
			case <-time.After(10 * time.Second):
				t.Fatal("serving did not stop")
			}
			if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) && !notThereYet(err) {
				t.Errorf("after unmounting, %s: %v", root, err)
			}
		})
	}
}

// Once the session has ended, nothing is served, and the abort removes the
// volume.
func TestWinFspAbort(t *testing.T) {
	requireWinFsp(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e, err := attach(ctx, testEngine(t), mountConfig{dir: dir, readOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	select {
	case <-e.served():
	default:
		t.Fatal("the volume is still served after the abort")
	}
	if err := e.abort(); err != nil {
		t.Errorf("a second abort: %v", err)
	}
}
