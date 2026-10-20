package nfs

import (
	"bytes"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sampleEngine() *fakeEngine {
	f := newFakeEngine()
	f.add("a.txt", 1001, syscall.S_IFREG|0o644, "hello world")
	f.add("dir", 1002, syscall.S_IFDIR|0o755, "")
	f.add("dir/b", 1003, syscall.S_IFREG|0o600, "bee")
	f.add("link", 1004, syscall.S_IFLNK|0o777, "a.txt")
	f.add("secret", 1005, syscall.S_IFREG|0o000, "s3cret")
	f.add("dir/sub", 1006, syscall.S_IFDIR|0o700, "")
	return f
}

func TestLookupAndGetAttr(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	fh, a := c.lookup(root, "a.txt")
	if len(fh) != 48 || a.ftype != typeReg || a.mode != 0o644 || a.size != 11 || a.fileid != 1001 || a.uid != 501 {
		t.Fatalf("attributes %+v", a)
	}
	if a.mtimeSec != 1700000000 || a.mtimeNsec != 123456789 {
		t.Fatalf("times lost their nanoseconds: %+v", a)
	}
	r := c.call(procGetAttr, wire{}.opaque(fh))
	if s := r.u32(); s != 0 {
		t.Fatalf("GETATTR status %d", s)
	}
	if got := r.fattr(); got != a {
		t.Fatalf("GETATTR %+v, LOOKUP %+v", got, a)
	}
	// LOOKUP of a missing name answers NOENT with the directory's attributes.
	r = c.call(procLookup, wire{}.opaque(root).str("missing"))
	if s := r.u32(); s != uint32(nfsErrNoEnt) {
		t.Fatalf("status %d", s)
	}
	if _, ok := r.postOpAttr(); !ok {
		t.Fatal("no directory attributes")
	}
	// A file is no directory.
	if s := c.status(procLookup, wire{}.opaque(fh).str("x")); s != uint32(nfsErrNotDir) {
		t.Fatalf("LOOKUP in a file: %d", s)
	}
}

func TestLookupNames(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	dir, dirAttr := c.lookup(root, "dir")
	sub, _ := c.lookup(dir, "sub")
	if fh, _ := c.lookup(dir, "."); !bytes.Equal(fh, dir) {
		t.Fatal(`"." is not the directory`)
	}
	if fh, a := c.lookup(sub, ".."); !bytes.Equal(fh, dir) || a.fileid != dirAttr.fileid {
		t.Fatal(`".." is not the parent`)
	}
	if fh, _ := c.lookup(dir, ".."); !bytes.Equal(fh, root) {
		t.Fatal(`".." of a top directory is not the root`)
	}
	if fh, _ := c.lookup(root, ".."); !bytes.Equal(fh, root) {
		t.Fatal(`the root's ".." is not the root`)
	}
	for name, want := range map[string]nfsStatus{
		strings.Repeat("x", 256): nfsErrNameLong,
		"":                       nfsErrNoEnt,
		"a/b":                    nfsErrNoEnt,
		"bad\xff":                nfsErrNoEnt,
	} {
		if s := c.status(procLookup, wire{}.opaque(root).str(name)); s != uint32(want) {
			t.Errorf("LOOKUP %q: %d, want %d", name, s, want)
		}
	}
}

// Every issued handle holds exactly one lookup reference on its inode,
// however often it is looked up.
func TestLookupReferences(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	for range 5 {
		c.lookup(root, "a.txt")
	}
	if n := f.refs("a.txt"); n != 1 {
		t.Fatalf("%d references", n)
	}
}

func TestHandlesSurviveRename(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	fh, _ := c.lookup(c.root(), "a.txt")
	f.rename("a.txt", "dir/moved.txt")
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != 0 {
		t.Fatalf("GETATTR after rename: %d", s)
	}
}

// A deleted object's handle is stale, and stays stale when something new
// takes its name: WFS gives the newcomer the same inode, but not the same
// identity.
func TestReplacedObjectsGoStale(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	old, oldAttr := c.lookup(root, "a.txt")
	f.remove("a.txt")
	if s := c.status(procGetAttr, wire{}.opaque(old)); s != uint32(nfsErrStale) {
		t.Fatalf("deleted: %d", s)
	}
	f.add("a.txt", 2001, syscall.S_IFREG|0o644, "new")
	if s := c.status(procGetAttr, wire{}.opaque(old)); s != uint32(nfsErrStale) {
		t.Fatalf("replaced: %d", s)
	}
	fresh, a := c.lookup(root, "a.txt")
	if bytes.Equal(fresh, old) || a.size != 3 || a.fileid != oldAttr.fileid {
		t.Fatalf("new object: same handle %v, %+v", bytes.Equal(fresh, old), a)
	}
	if s := c.status(procGetAttr, wire{}.opaque(old)); s != uint32(nfsErrStale) {
		t.Fatalf("the old handle came back: %d", s)
	}
	// Replaced while nobody looked: the old handle never reads the newcomer.
	b, _ := c.lookup(root, "secret")
	f.remove("secret")
	f.add("secret", 3001, syscall.S_IFREG|0o644, "other")
	if s := c.status(procRead, wire{}.opaque(b).u64(0).u32(10)); s != uint32(nfsErrStale) {
		t.Fatalf("READ through a replaced object's handle: %d", s)
	}
	if n := f.refs("a.txt"); n != 1 {
		t.Fatalf("%d references after replacement", n)
	}
}

func TestUIDGate(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	for _, proc := range []uint32{procGetAttr, procAccess, procFSInfo} {
		args := wire{}.opaque(root)
		if proc == procAccess {
			args = args.u32(0x3f)
		}
		if s := c.callAs(502, proc, args).u32(); s != uint32(nfsErrAccess) {
			t.Errorf("proc %d by another user: %d", proc, s)
		}
		if s := c.callAs(0, proc, args).u32(); s != 0 {
			t.Errorf("proc %d by root: %d", proc, s)
		}
	}
}

func TestAccess(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	access := func(uid uint32, fh []byte) uint32 {
		r := c.callAs(uid, procAccess, wire{}.opaque(fh).u32(0x3f))
		if s := r.u32(); s != 0 {
			t.Fatalf("ACCESS status %d", s)
		}
		if _, ok := r.postOpAttr(); !ok {
			t.Fatal("ACCESS without attributes")
		}
		return r.u32()
	}
	file, _ := c.lookup(root, "a.txt")
	dir, _ := c.lookup(root, "dir")
	secret, _ := c.lookup(root, "secret")
	// Read-only: nothing that modifies, extends or deletes is granted.
	cases := []struct {
		name string
		uid  uint32
		fh   []byte
		want uint32
	}{
		{"0644 file", 501, file, accessRead},
		{"0755 directory", 501, dir, accessRead | accessLookup},
		{"0000 file", 501, secret, 0},
		{"0000 file as root", 0, secret, accessRead},
		{"0755 directory as root", 0, dir, accessRead | accessLookup},
	}
	for _, tc := range cases {
		if got := access(tc.uid, tc.fh); got != tc.want {
			t.Errorf("%s: %#x, want %#x", tc.name, got, tc.want)
		}
	}
	// Only the bits asked for are answered.
	r := c.call(procAccess, wire{}.opaque(dir).u32(accessLookup|accessModify))
	r.u32()
	r.postOpAttr()
	if got := r.u32(); got != accessLookup {
		t.Errorf("asked LOOKUP|MODIFY: %#x", got)
	}
}

func TestRead(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	file, _ := c.lookup(root, "a.txt")
	read := func(fh []byte, offset uint64, count uint32) (uint32, []byte, bool) {
		r := c.call(procRead, wire{}.opaque(fh).u64(offset).u32(count))
		status := r.u32()
		if _, ok := r.postOpAttr(); !ok {
			t.Fatal("READ without attributes")
		}
		if status != 0 {
			return status, nil, false
		}
		n := r.u32()
		eof := r.u32() == 1
		data := r.opaque()
		if int(n) != len(data) {
			t.Fatalf("count %d, %d bytes", n, len(data))
		}
		return 0, data, eof
	}
	if _, data, eof := read(file, 0, 5); string(data) != "hello" || eof {
		t.Fatalf("%q eof %v", data, eof)
	}
	if _, data, eof := read(file, 6, 100); string(data) != "world" || !eof {
		t.Fatalf("%q eof %v", data, eof)
	}
	if _, data, eof := read(file, 50, 10); len(data) != 0 || !eof {
		t.Fatalf("past the end: %q eof %v", data, eof)
	}
	// The owner reads a 0000 file: RFC 1813 §4.4's owner exception.
	secret, _ := c.lookup(root, "secret")
	if _, data, _ := read(secret, 0, 100); string(data) != "s3cret" {
		t.Fatalf("owner exception: %q", data)
	}
	dir, _ := c.lookup(root, "dir")
	if s, _, _ := read(dir, 0, 10); s != uint32(nfsErrIsDir) {
		t.Fatalf("READ of a directory: %d", s)
	}
	link, _ := c.lookup(root, "link")
	if s, _, _ := read(link, 0, 10); s != uint32(nfsErrInval) {
		t.Fatalf("READ of a symlink: %d", s)
	}
}

func TestReadlink(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	link, _ := c.lookup(root, "link")
	r := c.call(procReadlink, wire{}.opaque(link))
	if s := r.u32(); s != 0 {
		t.Fatalf("status %d", s)
	}
	if a, _ := r.postOpAttr(); a.ftype != typeLnk {
		t.Fatalf("attributes %+v", a)
	}
	if target := r.opaque(); string(target) != "a.txt" {
		t.Fatalf("target %q", target)
	}
	file, _ := c.lookup(root, "a.txt")
	if s := c.status(procReadlink, wire{}.opaque(file)); s != uint32(nfsErrInval) {
		t.Fatalf("READLINK of a file: %d", s)
	}
}

func TestFilesystemInformation(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	r := c.call(procFSStat, wire{}.opaque(root))
	if s := r.u32(); s != 0 {
		t.Fatalf("FSSTAT %d", s)
	}
	r.postOpAttr()
	if tbytes, fbytes, abytes := r.u64(), r.u64(), r.u64(); tbytes != 2048*512 || fbytes != 1024*512 || abytes != 1024*512 {
		t.Fatalf("FSSTAT bytes %d %d %d", tbytes, fbytes, abytes)
	}
	if tfiles, ffiles := r.u64(), r.u64(); tfiles != 1000 || ffiles != 990 {
		t.Fatalf("FSSTAT files %d %d", tfiles, ffiles)
	}

	r = c.call(procFSInfo, wire{}.opaque(root))
	if s := r.u32(); s != 0 {
		t.Fatalf("FSINFO %d", s)
	}
	r.postOpAttr()
	got := []uint32{r.u32(), r.u32(), r.u32(), r.u32(), r.u32(), r.u32(), r.u32()}
	want := []uint32{1 << 20, 1 << 20, 4096, 1 << 20, 1 << 20, 4096, 64 << 10}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FSINFO sizes %v, want %v", got, want)
		}
	}
	if max := r.u64(); max != 9999*2<<20 {
		t.Fatalf("maxfilesize %d", max)
	}
	if sec, nsec := r.u32(), r.u32(); sec != 0 || nsec != 1 {
		t.Fatalf("time_delta %d.%09d", sec, nsec)
	}
	if props := r.u32(); props != 0x1b {
		t.Fatalf("properties %#x", props)
	}

	r = c.call(procPathConf, wire{}.opaque(root))
	if s := r.u32(); s != 0 {
		t.Fatalf("PATHCONF %d", s)
	}
	r.postOpAttr()
	if linkMax, nameMax := r.u32(), r.u32(); linkMax < 2 || nameMax != 255 {
		t.Fatalf("linkmax %d, name_max %d", linkMax, nameMax)
	}
	if noTrunc, chown, insensitive, preserving := r.u32(), r.u32(), r.u32(), r.u32(); noTrunc != 1 || chown != 1 || insensitive != 0 || preserving != 1 {
		t.Fatal("PATHCONF names: want no_trunc, chown_restricted, case-sensitive and case-preserving")
	}
}

// Until the write path exists, every procedure that would change
// something answers ROFS with its own failure layout.
func TestMutationsAreReadOnly(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	for proc, words := range map[uint32]int{
		procSetAttr: 2, procWrite: 2, procCreate: 2, procMkdir: 2, procSymlink: 2, procMknod: 2,
		procRemove: 2, procRmdir: 2, procRename: 4, procLink: 3, procCommit: 2,
	} {
		r := c.call(proc, wire{}.opaque(root))
		if s := r.u32(); s != uint32(nfsErrROFS) {
			t.Errorf("proc %d: status %d", proc, s)
		}
		if len(r.b) != 4*words || !bytes.Equal(r.b, make([]byte, 4*words)) {
			t.Errorf("proc %d: failure body % x, want %d absent fields", proc, r.b, words)
		}
	}
	if got := c.raw(programNFS, 3, 22, 501, nil); !bytes.HasSuffix(got, wire{}.u32(3)) {
		t.Errorf("procedure 22: % x, want PROC_UNAVAIL", got)
	}
}

func TestEndedExportFailsEveryCall(t *testing.T) {
	srv, c := startServer(t, sampleEngine())
	root := c.root()
	srv.End()
	if s := c.status(procGetAttr, wire{}.opaque(root)); s != uint32(nfsErrIO) {
		t.Fatalf("GETATTR after End: %d", s)
	}
	r := c.accepted(c.raw(programMount, 3, mountProcMnt, 501, wire{}.str("/")))
	if s := r.u32(); s != mountErrServFail {
		t.Fatalf("MNT after End: %d", s)
	}
}

func TestGarbageArguments(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	got := c.raw(programNFS, 3, procGetAttr, 501, wire{}.u32(1<<20))
	want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(4) // GARBAGE_ARGS
	if !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
}

// A call the engine does not finish in time is answered "retry later"
// before the client's 30-second timeout, and that answer is not cached.
func TestStalledCallsAnswerRetryLater(t *testing.T) {
	short(t, 200*time.Millisecond, 100*time.Millisecond)
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	f.block = make(chan struct{})
	defer close(f.block)
	start := time.Now()
	r := c.call(procGetAttr, wire{}.opaque(root))
	if s := r.u32(); s != uint32(nfsErrJukebox) {
		t.Fatalf("status %d", s)
	}
	if waited := time.Since(start); waited < 150*time.Millisecond || waited > 2*time.Second {
		t.Fatalf("answered after %s", waited)
	}
}

func TestBoundedSteps(t *testing.T) {
	short(t, 300*time.Millisecond, 100*time.Millisecond)
	s := &service{workers: make(chan struct{}, 1), ops: map[*op]struct{}{}, endCh: make(chan struct{})}
	c := callFor(1, procRemove, nil)
	release := make(chan struct{})
	defer close(release)

	// Still preparing at the budget: retry later, and the procedure is told.
	c.received = time.Now()
	stopped := make(chan bool, 1)
	r := s.run(c, false, func(o *op) reply {
		<-o.cancel
		stopped <- o.beginMutation()
		return reply{}
	})
	if !r.retry || !bytes.Equal(r.body, append(be32(uint32(nfsErrJukebox)), make([]byte, 8)...)) {
		t.Fatalf("got % x", r.body)
	}
	if <-stopped {
		t.Fatal("a mutation began after the call was answered")
	}

	// Already mutating at the deadline: the outcome is unknown, so IO.
	c.received = time.Now()
	r = s.run(c, false, func(o *op) reply {
		o.beginMutation()
		<-release
		return reply{}
	})
	if r.retry || !bytes.HasPrefix(r.body, be32(uint32(nfsErrIO))) {
		t.Fatalf("got % x", r.body)
	}

	// No worker free within the budget: retry later. The one worker is
	// still held by the stuck mutation above.
	c.received = time.Now()
	if r = s.run(c, true, func(*op) reply { return reply{} }); !r.retry {
		t.Fatal("ran without a worker")
	}
}

// short sets the call deadlines for one test.
func short(t *testing.T, deadline, prepare time.Duration) {
	oldDeadline, oldPrepare := rpcDeadline, prepareBudget
	rpcDeadline, prepareBudget = deadline, prepare
	t.Cleanup(func() { rpcDeadline, prepareBudget = oldDeadline, oldPrepare })
}

// An old handle never reads the object that replaced its file, whether the
// replacement lands before the read opens the file or while it reads.
func TestReadNeverReturnsTheReplacement(t *testing.T) {
	for name, hook := range map[string]func(f *fakeEngine, swap func()){
		"before open": func(f *fakeEngine, swap func()) { f.beforeOpen = swap },
		"before read": func(f *fakeEngine, swap func()) { f.beforeRead = swap },
	} {
		t.Run(name, func(t *testing.T) {
			f := sampleEngine()
			_, c := startServer(t, f)
			old, _ := c.lookup(c.root(), "a.txt")
			hook(f, func() {
				f.remove("a.txt")
				f.add("a.txt", 2001, syscall.S_IFREG|0o644, "successor")
			})
			r := c.call(procRead, wire{}.opaque(old).u64(0).u32(100))
			if s := r.u32(); s != uint32(nfsErrStale) {
				t.Fatalf("status %d, rest % x", s, r.b)
			}
			if bytes.Contains(r.b, []byte("successor")) {
				t.Fatal("the successor's bytes went out")
			}
		})
	}
}

// An object seen gone during a lookup is retired, so a successor with the
// same stored inode (recreated within the same second) gets new handles
// and the old ones stay stale.
func TestLookupRetiresAVanishedObject(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	old, _ := c.lookup(root, "a.txt")
	f.afterLookup = func() { f.remove("a.txt") }
	if s := c.status(procLookup, wire{}.opaque(root).str("a.txt")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("lookup racing the delete: %d", s)
	}
	f.afterLookup = nil
	f.add("a.txt", 1001, syscall.S_IFREG|0o644, "same inode, other object")
	fresh, _ := c.lookup(root, "a.txt")
	if bytes.Equal(fresh, old) {
		t.Fatal("the successor got the old handle")
	}
	if s := c.status(procGetAttr, wire{}.opaque(old)); s != uint32(nfsErrStale) {
		t.Fatalf("old handle: %d", s)
	}
}

// Nothing read where data was asked for is the end of the file, even when
// the size said more a moment ago.
func TestReadOfATruncatedFileEnds(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	fh, _ := c.lookup(c.root(), "a.txt")
	f.beforeRead = func() { f.truncate("a.txt") }
	r := c.call(procRead, wire{}.opaque(fh).u64(0).u32(5))
	if s := r.u32(); s != 0 {
		t.Fatalf("status %d", s)
	}
	r.postOpAttr()
	if n, eof := r.u32(), r.u32(); n != 0 || eof != 1 {
		t.Fatalf("count %d, eof %d", n, eof)
	}
}

// End answers calls in progress at once, and Close waits for their work.
func TestEndCancelsCallsInProgress(t *testing.T) {
	short(t, 2*time.Second, time.Second)
	f := sampleEngine()
	srv, c := startServer(t, f)
	root := c.root()
	f.block = make(chan struct{})
	c.xid++
	c.send(callMessage(c.xid, programNFS, 3, procGetAttr, 501, wire{}.opaque(root)))
	time.Sleep(50 * time.Millisecond) // the call is with the engine
	srv.End()
	start := time.Now()
	if s := c.accepted(c.receive()).u32(); s != uint32(nfsErrIO) || time.Since(start) > time.Second {
		t.Fatalf("status %d after %s", s, time.Since(start))
	}
	closed := make(chan struct{})
	go func() { srv.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("Close did not wait for the work still running")
	case <-time.After(200 * time.Millisecond):
	}
	close(f.block) // a closed block lets everything through
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the work ended")
	}
}

// A LOOKUP that no longer finds a name checks the object it last named: a
// deleted one is retired, so a successor with the same stored inode gets
// new handles; a renamed one keeps its handle.
func TestLookupMissRetiresDeletedObjects(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	old, _ := c.lookup(root, "a.txt")
	f.remove("a.txt")
	if s := c.status(procLookup, wire{}.opaque(root).str("a.txt")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("lookup after the delete: %d", s)
	}
	f.add("a.txt", 1001, syscall.S_IFREG|0o644, "same stored inode")
	if fresh, _ := c.lookup(root, "a.txt"); bytes.Equal(fresh, old) {
		t.Fatal("the successor got the old handle")
	}
	if s := c.status(procGetAttr, wire{}.opaque(old)); s != uint32(nfsErrStale) {
		t.Fatalf("old handle: %d", s)
	}

	moved, _ := c.lookup(root, "secret")
	f.rename("secret", "dir/secret")
	if s := c.status(procLookup, wire{}.opaque(root).str("secret")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("lookup after the rename: %d", s)
	}
	if s := c.status(procGetAttr, wire{}.opaque(moved)); s != 0 {
		t.Fatalf("the renamed object's handle: %d", s)
	}
}

// replaceDir puts another directory, with another stored inode, at a path.
func replaceDir(f *fakeEngine, rel string, ino uint64) {
	f.remove(rel)
	f.add(rel, ino, syscall.S_IFDIR|0o755, "")
}

// Every procedure that reads by path after checking identity checks it
// again before answering: a replacement in between answers STALE, and
// nothing of the successor's goes out.
func TestReplacementRaces(t *testing.T) {
	t.Run("READLINK", func(t *testing.T) {
		f := sampleEngine()
		_, c := startServer(t, f)
		link, _ := c.lookup(c.root(), "link")
		f.beforeReadlink = func() {
			f.remove("link")
			f.add("link", 2004, syscall.S_IFLNK|0o777, "successor-target")
		}
		r := c.call(procReadlink, wire{}.opaque(link))
		if s := r.u32(); s != uint32(nfsErrStale) || bytes.Contains(r.b, []byte("successor")) {
			t.Fatalf("status %d, rest %q", s, r.b)
		}
	})
	t.Run("LOOKUP", func(t *testing.T) {
		f := sampleEngine()
		_, c := startServer(t, f)
		dir, _ := c.lookup(c.root(), "dir")
		f.afterLookup = func() {
			f.afterLookup = nil
			replaceDir(f, "dir", 2002)
			f.add("dir/b", 2003, syscall.S_IFREG|0o600, "successor")
		}
		if s := c.status(procLookup, wire{}.opaque(dir).str("b")); s != uint32(nfsErrStale) {
			t.Fatalf("status %d", s)
		}
	})
	t.Run("READDIRPLUS", func(t *testing.T) {
		f := sampleEngine()
		_, c := startServer(t, f)
		dir, _ := c.lookup(c.root(), "dir")
		f.beforeList = func() {
			f.beforeList = nil
			replaceDir(f, "dir", 2002)
			f.add("dir/successor", 2005, syscall.S_IFREG|0o600, "")
		}
		if s, _ := c.readDirPlus(dir, 0, 0, 4096, 8192); s != uint32(nfsErrStale) {
			t.Fatalf("status %d", s)
		}
		if n := f.refs("dir/successor"); n != 0 {
			t.Fatalf("the successor's child kept %d references", n)
		}
	})
}

// Close ends calls still waiting for a worker without starting them, and
// waits for the work running, at most rpcDeadline.
func TestCloseQuiescesQueuedCalls(t *testing.T) {
	short(t, 500*time.Millisecond, 5*time.Second)
	f := sampleEngine()
	srv, c := startServer(t, f)
	root := c.root()
	f.block = make(chan struct{})
	defer close(f.block)
	const queued = 10
	for range maxWorkers + queued {
		c.xid++
		c.send(callMessage(c.xid, programNFS, 3, procGetAttr, 501, wire{}.opaque(root)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		started := f.getAttrs
		f.mu.Unlock()
		if started == maxWorkers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d calls reached the engine", started)
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	srv.Close()
	if took := time.Since(start); took < 400*time.Millisecond || took > 3*time.Second {
		t.Fatalf("Close took %s with the engine stuck, want about rpcDeadline", took)
	}
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getAttrs != maxWorkers {
		t.Fatalf("%d calls reached the engine, want only the %d running", f.getAttrs, maxWorkers)
	}
}

// End abandons a call still preparing: it can no longer begin changing
// things once it sees the cancellation.
func TestEndAbandonsPreparation(t *testing.T) {
	s := &service{workers: make(chan struct{}, 1), ops: map[*op]struct{}{}, endCh: make(chan struct{})}
	c := callFor(1, procRemove, nil)
	c.received = time.Now()
	began := make(chan bool, 1)
	entered := make(chan struct{})
	result := make(chan reply, 1)
	go func() {
		result <- s.run(c, false, func(o *op) reply {
			close(entered)
			<-o.cancel
			began <- o.beginMutation()
			return reply{}
		})
	}()
	<-entered
	s.end()
	if <-began {
		t.Fatal("a mutation began after End")
	}
	if r := <-result; !bytes.HasPrefix(r.body, be32(uint32(nfsErrIO))) {
		t.Fatalf("answered % x", r.body)
	}
}

// A miss whose check of the object is inconclusive (an error) keeps the
// name, and the next miss checks again: a recreate with the same stored
// inode then does not revive the old handle.
func TestInconclusiveMissIsCheckedAgain(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	root := c.root()
	old, _ := c.lookup(root, "a.txt")
	f.remove("a.txt")
	f.mu.Lock()
	f.namespaceFails = true
	f.mu.Unlock()
	if s := c.status(procLookup, wire{}.opaque(root).str("a.txt")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("first miss: %d", s)
	}
	f.mu.Lock()
	f.namespaceFails = false
	f.mu.Unlock()
	if s := c.status(procLookup, wire{}.opaque(root).str("a.txt")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("second miss: %d", s)
	}
	f.add("a.txt", 1001, syscall.S_IFREG|0o644, "same stored inode")
	if s := c.status(procRead, wire{}.opaque(old).u64(0).u32(100)); s != uint32(nfsErrStale) {
		t.Fatalf("READ through the old handle: %d", s)
	}
}

// A link that vanishes between being resolved and being read is retired
// and STALE: recreated with the same stored inode, the old handle never
// reads the new target.
func TestReadlinkOfAVanishedLink(t *testing.T) {
	f := sampleEngine()
	_, c := startServer(t, f)
	link, _ := c.lookup(c.root(), "link")
	f.beforeReadlink = func() {
		f.beforeReadlink = nil
		f.remove("link")
	}
	if s := c.status(procReadlink, wire{}.opaque(link)); s != uint32(nfsErrStale) {
		t.Fatalf("READLINK of the vanished link: %d", s)
	}
	f.add("link", 1004, syscall.S_IFLNK|0o777, "replacement-target")
	r := c.call(procReadlink, wire{}.opaque(link))
	if s := r.u32(); s != uint32(nfsErrStale) || bytes.Contains(r.b, []byte("replacement")) {
		t.Fatalf("READLINK after the recreate: status %d", s)
	}
}
