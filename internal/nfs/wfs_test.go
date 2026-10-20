package nfs

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
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
	calls   int                        // lookups and listings served
}

func (f *memFiler) served() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
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
	f.calls++
	if e, ok := f.entries[strings.TrimSuffix(r.Directory, "/")+"/"+r.Name]; ok {
		return &filer_pb.LookupDirectoryEntryResponse{Entry: e}, nil
	}
	return nil, status.Error(codes.NotFound, filer_pb.ErrNotFound.Error())
}

func (f *memFiler) ListEntries(r *filer_pb.ListEntriesRequest, stream grpc.ServerStreamingServer[filer_pb.ListEntriesResponse]) error {
	f.mu.Lock()
	f.calls++
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
// configures it, against an in-memory filer. Directories of more than
// maxCached entries are not cached but read through to the filer, which
// sends no change events: there, the engine sees changes at once.
func realEngine(t *testing.T, filer *memFiler, maxCached int) *weedmount.WFS {
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
		CacheDirMaxEntries: maxCached,
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
	_, c := startServer(t, realEngine(t, filer, 100000))
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
	_, c := startServer(t, realEngine(t, filer, 2))
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

// A directory too large for the engine's cache is listed in batches: a
// READDIRPLUS of all of it, with attributes and handles, costs a few filer
// calls, not one per child; and names added or removed between pages show
// at once, with handles that work.
func TestRealEngineReadThroughDirectory(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{}}
	filer.put("/c/fs1", entry("big", os.ModeDir|0o755, 900, ""))
	const n = 1500
	for i := range n {
		filer.put("/c/fs1/big", entry(fmt.Sprintf("f%04d", i), 0o644, uint64(10000+i), "x"))
	}
	_, c := startServer(t, realEngine(t, filer, 100))
	dir, _ := c.lookup(c.root(), "big")
	before := filer.served()
	var got []string
	cookie, verf := uint64(0), uint64(0)
	pages := 0
	for ; ; pages++ {
		_, p := c.readDirPlus(dir, cookie, verf, 8192, 1<<20)
		for _, e := range p.entries {
			if e.attr == nil || len(e.fh) != handleSize {
				t.Fatalf("%s without attributes or handle", e.name)
			}
			got = append(got, e.name)
		}
		if p.eof {
			break
		}
		cookie, verf = p.last(), p.verf
		if pages == 0 {
			filer.remove("/c/fs1/big", "f1400")
			filer.put("/c/fs1/big", entry("f1499a", 0o644, 20000, "new"))
		}
	}
	calls := filer.served() - before
	t.Logf("%d entries in %d pages, %d filer calls", n, pages+1, calls)
	if calls > 20 || pages < 2 {
		t.Fatalf("%d filer calls for %d entries in %d pages", calls, n, pages+1)
	}
	if slices.Contains(got, "f1400") || !slices.Contains(got, "f1499a") || len(got) != n+2 {
		t.Fatalf("listed %d names", len(got))
	}
	fh, a := c.lookup(dir, "f1499a")
	if a.size != 3 {
		t.Fatalf("the added name: %+v", a)
	}
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != 0 {
		t.Fatalf("its handle: %d", s)
	}
}

// Two links to one file; the first is removed elsewhere. LOOKUP of the
// survivor, and the shared handle, still work through it.
func TestRealEngineHardLinkSurvivor(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{}}
	filer.put("/c/fs1", entry("d", os.ModeDir|0o755, 700, ""))
	for _, name := range []string{"one", "two"} {
		e := entry(name, 0o644, 4242, "shared")
		e.HardLinkId, e.HardLinkCounter = []byte("link-4242"), 2
		filer.put("/c/fs1/d", e)
	}
	filer.put("/c/fs1/d", entry("other", 0o644, 701, "")) // too many to cache
	_, c := startServer(t, realEngine(t, filer, 2))
	dir, _ := c.lookup(c.root(), "d")
	one, a := c.lookup(dir, "one")
	if two, b := c.lookup(dir, "two"); !bytes.Equal(one, two) || a.fileid != b.fileid {
		t.Fatal("the links got different handles")
	}
	filer.remove("/c/fs1/d", "one")
	if s := c.status(procLookup, wire{}.opaque(dir).str("one")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("the removed link: %d", s)
	}
	if two, _ := c.lookup(dir, "two"); !bytes.Equal(two, one) {
		t.Fatal("the survivor got another handle")
	}
	if s := c.status(procGetAttr, wire{}.opaque(one)); s != 0 {
		t.Fatalf("the shared handle: %d", s)
	}
	r := c.call(procRead, wire{}.opaque(one).u64(0).u32(100))
	if s := r.u32(); s != 0 {
		t.Fatalf("READ through the survivor: %d", s)
	}
	r.postOpAttr()
	r.u32()
	r.u32()
	if data := r.opaque(); string(data) != "shared" {
		t.Fatalf("read %q", data)
	}
}

// Two links to one file; the first is replaced by a rename over it. The
// shared handle and the survivor keep the file; the name that was
// overwritten is another file now. Many GETATTRs at once agree.
func TestRealEngineRenameOverAHardLink(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{}}
	filer.put("/c/fs1", entry("d", os.ModeDir|0o755, 700, ""))
	for _, name := range []string{"one", "two"} {
		e := entry(name, 0o644, 4242, "shared")
		e.HardLinkId, e.HardLinkCounter = []byte("link-4242"), 2
		filer.put("/c/fs1/d", e)
	}
	filer.put("/c/fs1/d", entry("other", 0o644, 701, "")) // too many to cache
	_, c := startServer(t, realEngine(t, filer, 2))
	dir, _ := c.lookup(c.root(), "d")
	shared, _ := c.lookup(dir, "one")
	c.lookup(dir, "two")
	filer.put("/c/fs1/d", entry("one", 0o644, 9999, "replacement"))

	for range 40 { // pipelined: the server works on them at once
		c.xid++
		c.send(callMessage(c.xid, programNFS, 3, procGetAttr, 501, wire{}.opaque(shared)))
	}
	for range 40 {
		r := &replyReader{t: t, b: c.receive()[24:]}
		if s := r.u32(); s != 0 {
			t.Fatalf("GETATTR of the shared handle: %d", s)
		}
	}
	read := func(fh []byte) string {
		r := c.call(procRead, wire{}.opaque(fh).u64(0).u32(100))
		if s := r.u32(); s != 0 {
			t.Fatalf("READ: %d", s)
		}
		r.postOpAttr()
		r.u32()
		r.u32()
		return string(r.opaque())
	}
	if got := read(shared); got != "shared" {
		t.Fatalf("the shared handle read %q", got)
	}
	if two, _ := c.lookup(dir, "two"); !bytes.Equal(two, shared) || read(two) != "shared" {
		t.Fatal("the survivor lost its file")
	}
	one, a := c.lookup(dir, "one")
	if bytes.Equal(one, shared) || a.size != 11 || read(one) != "replacement" {
		t.Fatal("the overwritten name is not the replacement")
	}
}

// Codex's reproduction on the real engine: a handle holds the file open
// while it is deleted; LOOKUP sees the name gone; the file is recreated
// with the same stored inode. The open handle does not count as proof the
// old object survived: the old NFS handle is STALE, never the newcomer.
func TestRealEngineOpenHandleIsNoProofOfLife(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{}}
	filer.put("/c/fs1", entry("d", os.ModeDir|0o755, 700, ""))
	for i, name := range []string{"f", "x", "y"} { // too many to cache
		filer.put("/c/fs1/d", entry(name, 0o644, uint64(710+i), "original"))
	}
	engine := realEngine(t, filer, 2)
	_, c := startServer(t, engine)
	dir, _ := c.lookup(c.root(), "d")
	old, a := c.lookup(dir, "f")
	var opened fuse.OpenOut
	h := fuse.InHeader{NodeId: a.fileid}
	if st := engine.Open(nil, &fuse.OpenIn{InHeader: h, Flags: syscall.O_RDONLY}, &opened); st != fuse.OK {
		t.Fatalf("open: %v", st)
	}
	filer.remove("/c/fs1/d", "f")
	if s := c.status(procLookup, wire{}.opaque(dir).str("f")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("lookup of the deleted name: %d", s)
	}
	filer.put("/c/fs1/d", entry("f", 0o644, 710, "successor"))
	engine.Release(nil, &fuse.ReleaseIn{InHeader: h, Fh: opened.Fh, Flags: syscall.O_RDONLY})
	r := c.call(procRead, wire{}.opaque(old).u64(0).u32(100))
	if s := r.u32(); s != uint32(nfsErrStale) || bytes.Contains(r.b, []byte("successor")) {
		t.Fatalf("READ through the old handle: status %d", s)
	}
}
