package nfs

import (
	"context"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// memFiler is a filer holding a few entries in memory, enough for the real
// mount engine to look up, list, read inline content and report usage.
type memFiler struct {
	filer_pb.UnimplementedSeaweedFilerServer
	mu      sync.Mutex
	entries map[string]*filer_pb.Entry // by directory + "/" + name
}

func (f *memFiler) remove(dir, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, dir+"/"+name)
}

func (f *memFiler) put(dir string, e *filer_pb.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[dir+"/"+e.Name] = e
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
	var names []string
	byName := map[string]*filer_pb.Entry{}
	for key, e := range f.entries {
		if dir := key[:strings.LastIndex(key, "/")]; dir == strings.TrimSuffix(r.Directory, "/") && strings.HasPrefix(e.Name, r.Prefix) {
			names = append(names, e.Name)
			byName[e.Name] = e
		}
	}
	f.mu.Unlock()
	sort.Strings(names)
	sent := uint32(0)
	for _, name := range names {
		if name < r.StartFromFileName || name == r.StartFromFileName && !r.InclusiveStartFrom {
			continue
		}
		if r.Limit > 0 && sent == r.Limit {
			break
		}
		if err := stream.Send(&filer_pb.ListEntriesResponse{Entry: byName[name]}); err != nil {
			return err
		}
		sent++
	}
	return nil
}

func (f *memFiler) Statistics(context.Context, *filer_pb.StatisticsRequest) (*filer_pb.StatisticsResponse, error) {
	return &filer_pb.StatisticsResponse{TotalSize: 1 << 30, UsedSize: 1 << 20, FileCount: 3}, nil
}

func entry(name string, mode os.FileMode, inode uint64, content string) *filer_pb.Entry {
	now := time.Now()
	return &filer_pb.Entry{
		Name:        name,
		IsDirectory: mode.IsDir(),
		Content:     []byte(content),
		Attributes: &filer_pb.FuseAttributes{
			FileSize: uint64(len(content)),
			Mtime:    now.Unix(), MtimeNs: int32(now.Nanosecond()),
			Crtime:   now.Unix(),
			FileMode: uint32(mode),
			Uid:      1000, Gid: 1000,
			Inode: inode,
		},
	}
}

// realEngine is the SeaweedFS mount engine itself, configured as the CLI
// configures it, against an in-memory filer.
func realEngine(t *testing.T, filer *memFiler) *weedmount.WFS {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	filer_pb.RegisterSeaweedFilerServer(server, filer)
	go server.Serve(l)
	t.Cleanup(server.Stop)
	mapper, err := meta_cache.NewUidGidMapper("501:1000", "20:1000")
	if err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	return weedmount.NewSeaweedFileSystem(&weedmount.Option{
		MountDirectory:     t.TempDir(),
		FilerAddresses:     []pb.ServerAddress{pb.NewServerAddress("127.0.0.1", 1, l.Addr().(*net.TCPAddr).Port)},
		GrpcDialOption:     grpc.WithTransportCredentials(insecure.NewCredentials()),
		FilerMountRootPath: "/c/fs1",
		ChunkSizeLimit:     2 << 20,
		ConcurrentWriters:  8,
		ConcurrentReaders:  8,
		CacheDirForRead:    cache,
		CacheDirForWrite:   cache,
		CacheMetaTTlSec:    60,
		CacheDirMaxEntries: 100000,
		MountUid:           501,
		MountGid:           20,
		MountMode:          os.ModeDir | 0o755,
		MountCtime:         time.Now(),
		MountMtime:         time.Now(),
		Umask:              0o022,
		VolumeServerAccess: "filerProxy",
		UidGidMapper:       mapper,
		DefaultPermissions: true,
	})
}

// The read path against the real engine: lookups, listing with handles,
// attributes and content.
func TestRealEngineReadPath(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{}}
	filer.put("/c/fs1", entry("hello.txt", 0o644, 777, "hello from the engine"))
	filer.put("/c/fs1", entry("sub", os.ModeDir|0o755, 778, ""))
	filer.put("/c/fs1/sub", entry("deep.txt", 0o600, 779, "deep"))
	_, c := startServer(t, realEngine(t, filer))
	root := c.root()

	fh, a := c.lookup(root, "hello.txt")
	if a.ftype != typeReg || a.size != 21 || a.uid != 501 || a.fileid != 777 || a.mode != 0o644 {
		t.Fatalf("attributes %+v", a)
	}
	r := c.call(procRead, wire{}.opaque(fh).u64(6).u32(100))
	if s := r.u32(); s != 0 {
		t.Fatalf("READ %d", s)
	}
	r.postOpAttr()
	n, eof, data := r.u32(), r.u32(), r.opaque()
	if string(data) != "from the engine" || n != 15 || eof != 1 {
		t.Fatalf("read %q (%d), eof %d", data, n, eof)
	}

	status, p := c.readDirPlus(root, 0, 0, 8192, 65536)
	if status != 0 || !p.eof {
		t.Fatalf("READDIRPLUS %d, eof %v", status, p.eof)
	}
	var names []string
	for _, e := range p.entries {
		names = append(names, e.name)
		if e.attr == nil || len(e.fh) != handleSize {
			t.Fatalf("%s without attributes or handle", e.name)
		}
	}
	if strings.Join(names, " ") != ". .. hello.txt sub" {
		t.Fatalf("listed %v", names)
	}
	sub := p.entries[3].fh
	deep, _ := c.lookup(sub, "deep.txt")
	if s := c.status(procGetAttr, wire{}.opaque(deep)); s != 0 {
		t.Fatalf("GETATTR %d", s)
	}
	if up, _ := c.lookup(sub, ".."); string(up) != string(root) {
		t.Fatal(`".." of sub is not the root`)
	}
	// The handle the listing issued and the one LOOKUP issues are the same.
	if again, _ := c.lookup(root, "sub"); string(again) != string(sub) {
		t.Fatal("LOOKUP and READDIRPLUS issued different handles")
	}
	if s := c.status(procLookup, wire{}.opaque(root).str("missing")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("missing name: %d", s)
	}
	r = c.call(procFSStat, wire{}.opaque(root))
	if s := r.u32(); s != 0 {
		t.Fatalf("FSSTAT %d", s)
	}
	r.postOpAttr()
	if tbytes := r.u64(); tbytes != 1<<30 {
		t.Fatalf("FSSTAT total %d", tbytes)
	}
}

// The real engine, codex's reproduction: listing A reads "a", "a" is
// deleted, listing B starts over, and A resumes without skipping "b".
func TestRealEngineRestartKeepsCookies(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{}}
	filer.put("/c/fs1", entry("d", os.ModeDir|0o755, 800, ""))
	for i, name := range []string{"a", "b", "c"} {
		filer.put("/c/fs1/d", entry(name, 0o644, uint64(801+i), name))
	}
	_, c := startServer(t, realEngine(t, filer))
	dir, _ := c.lookup(c.root(), "d")
	_, a := c.readDirPlus(dir, 0, 0, 1024, pageOverhead+3*184)
	if names := strings.Join(a.names(), " "); names != ". .. a" {
		t.Fatalf("first page %s", names)
	}
	filer.remove("/c/fs1/d", "a")
	if names := strings.Join(c.readAll(dir, 0, 0, 4096), " "); names != ". .. b c" {
		t.Fatalf("the restarted listing: %s", names)
	}
	if names := strings.Join(c.readAll(dir, a.last(), a.verf, 4096), " "); names != "b c" {
		t.Fatalf("the paused listing resumed with %s", names)
	}
}
