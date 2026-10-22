package nfs

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
)

// noAttrs is a sattr3 that sets nothing.
var noAttrs = wire{}.u32(0).u32(0).u32(0).u32(0).u32(timeDontChange).u32(timeDontChange)

// modeAttrs is a sattr3 that sets only the mode.
func modeAttrs(mode uint32) wire {
	return wire{}.u32(1).u32(mode).u32(0).u32(0).u32(0).u32(timeDontChange).u32(timeDontChange)
}

// wcc reads wcc_data, reporting whether both halves are present.
func (r *replyReader) wcc() bool {
	pre := r.u32() == 1
	if pre {
		r.u64() // size
		r.u64() // mtime
		r.u64() // ctime
	}
	_, post := r.postOpAttr()
	return pre && post
}

// created reads a create's result: its handle and attributes.
func (r *replyReader) created() ([]byte, fattr) {
	r.t.Helper()
	if s := r.u32(); s != uint32(nfsOK) {
		r.t.Fatalf("status %d", s)
	}
	if r.u32() != 1 {
		r.t.Fatal("no handle")
	}
	fh := r.opaque()
	a, ok := r.postOpAttr()
	if !ok {
		r.t.Fatal("no attributes")
	}
	if !r.wcc() {
		r.t.Fatal("no directory wcc_data")
	}
	return fh, a
}

func writable(t *testing.T) (*fakeEngine, *Server, *testClient, []byte) {
	f := sampleEngine()
	srv, c := startWritable(t, f)
	return f, srv, c, c.root()
}

// CREATE GUARDED makes a file and refuses an existing name; the handle it
// returns is the file's, as LOOKUP's is.
func TestCreateGuarded(t *testing.T) {
	f, _, c, root := writable(t)
	fh, a := c.call(procCreate, wire{}.opaque(root).str("new").u32(createGuarded).append(modeAttrs(0o600))).created()
	if a.ftype != typeReg || a.mode != 0o600 {
		t.Fatalf("attributes %+v", a)
	}
	if looked, _ := c.lookup(root, "new"); !bytes.Equal(looked, fh) {
		t.Fatal("LOOKUP gave another handle")
	}
	r := c.call(procCreate, wire{}.opaque(root).str("new").u32(createGuarded).append(noAttrs))
	if s := r.u32(); s != uint32(nfsErrExist) || !r.wcc() {
		t.Fatalf("again: %d", s)
	}
	if f.count("create") != 1 {
		t.Fatalf("%d creates", f.count("create"))
	}
}

// CREATE EXCLUSIVE retried with its verifier (a lost reply) finds the file
// it made, under a new XID; another verifier is refused. The SETATTR that
// follows clears the verifier, and only that one.
func TestCreateExclusiveReplay(t *testing.T) {
	f, _, c, root := writable(t)
	verifier := wire{}.u32(0xdead).u32(0xbeef)
	args := wire{}.opaque(root).str("lock").u32(createExclusive).append(verifier)
	fh, _ := c.call(procCreate, args).created()
	again, _ := c.call(procCreate, args).created() // a new XID
	if !bytes.Equal(fh, again) || f.count("create") != 1 {
		t.Fatalf("the retry: same handle %v, %d creates", bytes.Equal(fh, again), f.count("create"))
	}
	other := wire{}.opaque(root).str("lock").u32(createExclusive).append(wire{}.u32(1).u32(2))
	if s := c.status(procCreate, other); s != uint32(nfsErrExist) {
		t.Fatalf("another verifier: %d", s)
	}
	for range 2 {
		if s := c.status(procSetAttr, wire{}.opaque(fh).append(modeAttrs(0o644)).u32(0)); s != uint32(nfsOK) {
			t.Fatalf("SETATTR: %d", s)
		}
	}
	f.mu.Lock()
	cleared := f.clearedVerifiers
	f.mu.Unlock()
	if cleared != 1 {
		t.Fatalf("%d SETATTRs cleared the verifier", cleared)
	}
}

// MKDIR and SYMLINK make their objects; MKNOD is refused.
func TestMkdirSymlinkMknod(t *testing.T) {
	_, _, c, root := writable(t)
	if _, a := c.call(procMkdir, wire{}.opaque(root).str("d2").append(noAttrs)).created(); a.ftype != typeDir {
		t.Fatalf("MKDIR made %+v", a)
	}
	link, a := c.call(procSymlink, wire{}.opaque(root).str("l2").append(noAttrs).str("../target")).created()
	if a.ftype != typeLnk {
		t.Fatalf("SYMLINK made %+v", a)
	}
	r := c.call(procReadlink, wire{}.opaque(link))
	if r.u32() != uint32(nfsOK) {
		t.Fatal("READLINK")
	}
	r.postOpAttr()
	if got := string(r.opaque()); got != "../target" {
		t.Fatalf("target %q", got)
	}
	if s := c.status(procMknod, wire{}.opaque(root).str("fifo").u32(typeFifo).append(noAttrs)); s != uint32(nfsErrNotSupp) {
		t.Fatalf("MKNOD: %d", s)
	}
	if s := c.status(procMknod, wire{}.opaque(root).str("file").u32(typeReg)); s != uint32(nfsErrBadType) {
		t.Fatalf("MKNOD of a file: %d", s)
	}
}

// REMOVE drops a name, and its old handle goes stale; RMDIR refuses a
// directory that is not empty and files.
func TestRemoveAndRmdir(t *testing.T) {
	_, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	r := c.call(procRemove, wire{}.opaque(root).str("a.txt"))
	if s := r.u32(); s != uint32(nfsOK) || !r.wcc() {
		t.Fatalf("REMOVE: %d", s)
	}
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != uint32(nfsErrStale) {
		t.Fatalf("the removed file's handle: %d", s)
	}
	if s := c.status(procLookup, wire{}.opaque(root).str("a.txt")); s != uint32(nfsErrNoEnt) {
		t.Fatalf("LOOKUP after REMOVE: %d", s)
	}
	if s := c.status(procRmdir, wire{}.opaque(root).str("dir")); s != uint32(nfsErrNotEmpty) {
		t.Fatalf("RMDIR of a full directory: %d", s)
	}
	if s := c.status(procRmdir, wire{}.opaque(root).str("secret")); s != uint32(nfsErrNotDir) {
		t.Fatalf("RMDIR of a file: %d", s)
	}
	if s := c.status(procRemove, wire{}.opaque(root).str("dir")); s != uint32(nfsErrIsDir) {
		t.Fatalf("REMOVE of a directory: %d", s)
	}
}

// RENAME keeps the moved file's handle; the file it replaced goes stale.
func TestRenameKeepsTheHandle(t *testing.T) {
	_, _, c, root := writable(t)
	moved, _ := c.lookup(root, "a.txt")
	replaced, _ := c.lookup(root, "secret")
	r := c.call(procRename, wire{}.opaque(root).str("a.txt").opaque(root).str("secret"))
	if s := r.u32(); s != uint32(nfsOK) || !r.wcc() || !r.wcc() {
		t.Fatalf("RENAME: %d", s)
	}
	if now, _ := c.lookup(root, "secret"); !bytes.Equal(now, moved) {
		t.Fatal("the moved file has another handle")
	}
	if s := c.status(procGetAttr, wire{}.opaque(replaced)); s != uint32(nfsErrStale) {
		t.Fatalf("the replaced file's handle: %d", s)
	}
}

// LINK gives the new name the file's own handle and fileid.
func TestLinkSharesTheHandle(t *testing.T) {
	_, _, c, root := writable(t)
	fh, a := c.lookup(root, "a.txt")
	r := c.call(procLink, wire{}.opaque(fh).opaque(root).str("alias"))
	if s := r.u32(); s != uint32(nfsOK) {
		t.Fatalf("LINK: %d", s)
	}
	if _, ok := r.postOpAttr(); !ok || !r.wcc() {
		t.Fatal("LINK without attributes")
	}
	if alias, b := c.lookup(root, "alias"); !bytes.Equal(alias, fh) || b.fileid != a.fileid {
		t.Fatal("the alias is another object")
	}
}

// WRITE UNSTABLE is accepted and not persisted; COMMIT persists it, with
// the same verifier; a FILE_SYNC WRITE is persisted before its reply.
func TestWriteAndCommit(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	write := func(offset uint64, data string, stable uint32) (uint32, []byte) {
		r := c.call(procWrite, wire{}.opaque(fh).u64(offset).u32(uint32(len(data))).u32(stable).str(data))
		if s := r.u32(); s != uint32(nfsOK) {
			t.Fatalf("WRITE: %d", s)
		}
		r.wcc()
		if n := r.u32(); n != uint32(len(data)) {
			t.Fatalf("wrote %d", n)
		}
		committed := r.u32()
		return committed, append([]byte(nil), r.b[:8]...)
	}
	committed, verifier := write(0, "HELLO", stableUnstable)
	if committed != stableUnstable || f.fsyncsNow() != 0 {
		t.Fatalf("UNSTABLE: committed %d, %d flushes", committed, f.fsyncsNow())
	}
	r := c.call(procCommit, wire{}.opaque(fh).u64(0).u32(0))
	if s := r.u32(); s != uint32(nfsOK) {
		t.Fatalf("COMMIT: %d", s)
	}
	r.wcc()
	if !bytes.Equal(r.b, verifier) || f.fsyncsNow() != 1 {
		t.Fatalf("COMMIT verifier % x, WRITE's % x, %d flushes", r.b, verifier, f.fsyncsNow())
	}
	if committed, _ := write(5, " world", stableFileSync); committed != stableFileSync || f.fsyncsNow() != 2 {
		t.Fatalf("FILE_SYNC: committed %d, %d flushes", committed, f.fsyncsNow())
	}
	f.mu.Lock()
	data := string(f.objects[f.root+"/a.txt"].data)
	f.mu.Unlock()
	if data != "HELLO world" {
		t.Fatalf("data %q", data)
	}
}

func (f *fakeEngine) fsyncsNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fsyncs
}

// A flush that fails while idle is kept until a COMMIT reports it, once;
// the idle handle is released with its lease.
func TestIdleFlushFailureIsReportedByCommit(t *testing.T) {
	saved := writeIdle
	writeIdle = 50 * time.Millisecond
	defer func() { writeIdle = saved }()
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	f.mu.Lock()
	f.fsyncErr = fuse.Status(syscall.EDQUOT)
	f.mu.Unlock()
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	for deadline := time.Now().Add(5 * time.Second); f.releasesNow() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the idle handle was not released")
		}
	}
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsErrDQuot) {
		t.Fatalf("COMMIT after the failed flush: %d", s)
	}
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("the next COMMIT: %d", s)
	}
}

func (f *fakeEngine) releasesNow() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releases
}

// A COMMIT whose flush outlasts the call answers "retry later"; the retry
// waits for that flush and reports it.
func TestLongCommitAcrossRetries(t *testing.T) {
	short(t, 1200*time.Millisecond, 200*time.Millisecond)
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	gate := make(chan struct{})
	f.mu.Lock()
	f.fsyncGate = gate
	f.mu.Unlock()
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsErrJukebox) {
		t.Fatalf("COMMIT during the flush: %d", s)
	}
	close(gate)
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("COMMIT retried: %d", s)
	}
	if f.fsyncsNow() != 1 {
		t.Fatalf("%d flushes", f.fsyncsNow())
	}
}

// CREATE, REMOVE, SETATTR, RENAME and LINK held up by another mount's
// lock answer "retry later" within the preparation budget, changing
// nothing; retried with new XIDs once it is free, each is applied once.
func TestMutationsHeldByAnotherMount(t *testing.T) {
	short(t, 1500*time.Millisecond, 200*time.Millisecond)
	f, _, c, root := writable(t)
	a, _ := c.lookup(root, "a.txt")
	calls := map[string]func() (uint32, wire){
		"create": func() (uint32, wire) {
			return procCreate, wire{}.opaque(root).str("n").u32(createGuarded).append(noAttrs)
		},
		"remove":  func() (uint32, wire) { return procRemove, wire{}.opaque(root).str("secret") },
		"setattr": func() (uint32, wire) { return procSetAttr, wire{}.opaque(a).append(modeAttrs(0o600)).u32(0) },
		"rename":  func() (uint32, wire) { return procRename, wire{}.opaque(root).str("a.txt").opaque(root).str("b.txt") },
		"link":    func() (uint32, wire) { return procLink, wire{}.opaque(a).opaque(root).str("l") },
	}
	for _, what := range []string{"create", "remove", "setattr", "link", "rename"} {
		proc, args := calls[what]()
		f.mu.Lock()
		f.held = true
		f.mu.Unlock()
		start := time.Now()
		if s := c.status(proc, args); s != uint32(nfsErrJukebox) || f.count(what) != 0 {
			t.Fatalf("%s held: %d, %d applied", what, s, f.count(what))
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("%s held: answered after %v", what, took)
		}
		f.mu.Lock()
		f.held = false
		f.mu.Unlock()
		if s := c.status(proc, args); s != uint32(nfsOK) || f.count(what) != 1 {
			t.Fatalf("%s retried: %d, %d applied", what, s, f.count(what))
		}
	}
}

// Two identical SETATTRs, such as two touches, are both applied: only a
// retransmission (the same XID) is answered from the cache.
func TestIdenticalSetAttrsAreBothApplied(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	touch := wire{}.opaque(fh).u32(0).u32(0).u32(0).u32(0).u32(timeServer).u32(timeServer).u32(0)
	for range 2 {
		if s := c.status(procSetAttr, touch); s != uint32(nfsOK) {
			t.Fatalf("touch: %d", s)
		}
	}
	if f.count("setattr") != 2 {
		t.Fatalf("%d applied", f.count("setattr"))
	}
	c.send(callMessage(4242, programNFS, 3, procSetAttr, 501, touch))
	first := c.receive()
	c.send(callMessage(4242, programNFS, 3, procSetAttr, 501, touch))
	if again := c.receive(); !bytes.Equal(first, again) || f.count("setattr") != 3 {
		t.Fatalf("the retransmission: same reply %v, %d applied", bytes.Equal(first, again), f.count("setattr"))
	}
}

// A guarded SETATTR whose ctime is not the object's answers NOT_SYNC.
func TestGuardedSetAttr(t *testing.T) {
	_, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	if s := c.status(procSetAttr, wire{}.opaque(fh).append(modeAttrs(0o600)).u32(1).u32(1700000001).u32(6)); s != uint32(nfsErrNotSync) {
		t.Fatalf("wrong guard: %d", s)
	}
	if s := c.status(procSetAttr, wire{}.opaque(fh).append(modeAttrs(0o600)).u32(1).u32(1700000001).u32(5)); s != uint32(nfsOK) {
		t.Fatalf("right guard: %d", s)
	}
}

// The server checks rights itself: another user's directory cannot be
// written to, the sticky bit keeps others' files, and only root gives a
// file away.
func TestMutationRights(t *testing.T) {
	f, _, c, root := writable(t)
	f.add("theirs", 1010, syscall.S_IFDIR|0o755, "").uid = 502
	f.add("sticky", 1011, syscall.S_IFDIR|0o1777, "").uid = 502
	f.add("sticky/f", 1012, syscall.S_IFREG|0o644, "x").uid = 502
	theirs, _ := c.lookup(root, "theirs")
	sticky, _ := c.lookup(root, "sticky")
	if s := c.status(procCreate, wire{}.opaque(theirs).str("n").u32(createGuarded).append(noAttrs)); s != uint32(nfsErrAccess) {
		t.Fatalf("CREATE in another user's directory: %d", s)
	}
	if s := c.status(procRemove, wire{}.opaque(sticky).str("f")); s != uint32(nfsErrAccess) {
		t.Fatalf("REMOVE of another user's file under the sticky bit: %d", s)
	}
	fh, _ := c.lookup(root, "a.txt")
	chown := wire{}.opaque(fh).u32(0).u32(1).u32(502).u32(0).u32(0).u32(timeDontChange).u32(timeDontChange).u32(0)
	if s := c.status(procSetAttr, chown); s != uint32(nfsErrPerm) {
		t.Fatalf("chown: %d", s)
	}
	if s := c.callAs(0, procSetAttr, chown).u32(); s != uint32(nfsOK) {
		t.Fatalf("chown as root: %d", s)
	}
}

// A clean stop persists what was accepted and not committed, and lets the
// write handles go.
func TestFlushOnStop(t *testing.T) {
	f, srv, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	srv.Flush()
	if f.fsyncsNow() != 1 || f.releasesNow() != 1 {
		t.Fatalf("%d flushes, %d releases", f.fsyncsNow(), f.releasesNow())
	}
}

// A REMOVE persists the file's accepted writes and lets its write handle
// go before the name goes.
func TestRemoveSettlesWrites(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	if s := c.status(procRemove, wire{}.opaque(root).str("a.txt")); s != uint32(nfsOK) {
		t.Fatalf("REMOVE: %d", s)
	}
	if f.fsyncsNow() != 1 || f.releasesNow() != 1 {
		t.Fatalf("%d flushes, %d releases", f.fsyncsNow(), f.releasesNow())
	}
}

func (w wire) append(more wire) wire { return append(w, more...) }

// REMOVE, RMDIR and RENAME take single names, as LOOKUP does: a path or
// a name the store would rewrite names nothing.
func TestRemovedNamesAreSingleNames(t *testing.T) {
	f, _, c, root := writable(t)
	for _, name := range []string{"dir/b", "\xff", ""} {
		if s := c.status(procRemove, wire{}.opaque(root).str(name)); s != uint32(nfsErrNoEnt) {
			t.Fatalf("REMOVE %q: %d", name, s)
		}
		if s := c.status(procRename, wire{}.opaque(root).str(name).opaque(root).str("x")); s != uint32(nfsErrNoEnt) {
			t.Fatalf("RENAME %q: %d", name, s)
		}
	}
	if f.count("remove") != 0 || f.count("rename") != 0 {
		t.Fatal("something was changed")
	}
}

// UNCHECKED CREATE of an existing name: another user's read-only file is
// not truncated, a directory is no file, and a size past the limit is
// refused; the caller's own file is truncated, its writes persisted
// first.
func TestUncheckedCreateOfAnExistingFile(t *testing.T) {
	f, _, c, root := writable(t)
	f.add("theirs", 1020, syscall.S_IFREG|0o444, "keep").uid = 502
	truncate := func(name string, size uint64) uint32 {
		return c.status(procCreate, wire{}.opaque(root).str(name).u32(createUnchecked).u32(0).u32(0).u32(0).u32(1).u64(size).u32(0).u32(0))
	}
	if s := truncate("theirs", 0); s != uint32(nfsErrAccess) {
		t.Fatalf("another user's file: %d", s)
	}
	if s := truncate("dir", 0); s != uint32(nfsErrIsDir) {
		t.Fatalf("a directory: %d", s)
	}
	if s := truncate("new", maxFileSize+1); s != uint32(nfsErrFBig) {
		t.Fatalf("past the limit: %d", s)
	}
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	if s := truncate("a.txt", 0); s != uint32(nfsOK) {
		t.Fatalf("the caller's file: %d", s)
	}
	f.mu.Lock()
	theirs, mine := string(f.objects[f.root+"/theirs"].data), len(f.objects[f.root+"/a.txt"].data)
	f.mu.Unlock()
	if theirs != "keep" || mine != 0 || f.fsyncsNow() != 1 {
		t.Fatalf("theirs %q, mine %d bytes, %d flushes", theirs, mine, f.fsyncsNow())
	}
}

// A handle that took writeFlushBytes flushes at once and keeps its handle
// and lease for the writes that follow.
func TestFlushAfterManyBytesKeepsTheHandle(t *testing.T) {
	saved := writeFlushBytes
	writeFlushBytes = 4
	defer func() { writeFlushBytes = saved }()
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(8).u32(stableUnstable).str("12345678"))
	for deadline := time.Now().Add(5 * time.Second); f.fsyncsNow() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no flush")
		}
	}
	c.call(procWrite, wire{}.opaque(fh).u64(8).u32(1).u32(stableUnstable).str("9"))
	f.mu.Lock()
	opens, releases := f.opens, f.releases
	f.mu.Unlock()
	if opens != 1 || releases != 0 {
		t.Fatalf("%d opens, %d releases", opens, releases)
	}
}

// A flush failing in a SETATTR's preparation that was already answered
// "retry later" is not lost: the next COMMIT reports it.
func TestSettleFailureAnsweredLaterIsKept(t *testing.T) {
	short(t, 1500*time.Millisecond, 200*time.Millisecond)
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	gate := make(chan struct{})
	f.mu.Lock()
	f.fsyncGate, f.fsyncErr = gate, fuse.Status(syscall.EDQUOT)
	f.mu.Unlock()
	if s := c.status(procSetAttr, wire{}.opaque(fh).append(modeAttrs(0o600)).u32(0)); s != uint32(nfsErrJukebox) {
		t.Fatalf("SETATTR during the flush: %d", s)
	}
	close(gate)
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsErrDQuot) {
		t.Fatalf("COMMIT: %d", s)
	}
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("COMMIT again: %d", s)
	}
}

// A failed flush lets its handle go: the next WRITE opens a fresh one,
// and the failure is reported once.
func TestFailedFlushReleasesTheHandle(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	f.mu.Lock()
	f.fsyncErr = fuse.Status(syscall.EDQUOT)
	f.mu.Unlock()
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsErrDQuot) {
		t.Fatalf("COMMIT: %d", s)
	}
	f.mu.Lock()
	f.fsyncErr = fuse.OK
	f.mu.Unlock()
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("y"))
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("COMMIT after the new write: %d", s)
	}
	f.mu.Lock()
	opens, releases := f.opens, f.releases
	f.mu.Unlock()
	if opens != 2 || releases != 1 {
		t.Fatalf("%d opens, %d releases", opens, releases)
	}
}

// An export that ended flushes nothing more: the idle flush is stopped.
func TestEndStopsIdleFlushes(t *testing.T) {
	saved := writeIdle
	writeIdle = 50 * time.Millisecond
	defer func() { writeIdle = saved }()
	f, srv, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	srv.End()
	time.Sleep(200 * time.Millisecond)
	if f.fsyncsNow() != 0 {
		t.Fatalf("%d flushes after End", f.fsyncsNow())
	}
}

// RENAME onto another name of the same file does nothing (RFC 1813
// §3.3.14): both names stay.
func TestRenameOntoAnotherNameOfTheFile(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	if s := c.status(procLink, wire{}.opaque(fh).opaque(root).str("alias")); s != uint32(nfsOK) {
		t.Fatalf("LINK: %d", s)
	}
	if s := c.status(procRename, wire{}.opaque(root).str("a.txt").opaque(root).str("alias")); s != uint32(nfsOK) {
		t.Fatalf("RENAME: %d", s)
	}
	c.lookup(root, "a.txt")
	c.lookup(root, "alias")
	if f.count("rename") != 0 {
		t.Fatal("the rename was sent")
	}
}

// Arguments that do not decode are GARBAGE_ARGS; COMMIT of a symlink is
// INVAL.
func TestMutationArguments(t *testing.T) {
	_, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	if got := c.raw(programNFS, 3, procSetAttr, 501, wire{}.opaque(fh).append(noAttrs).u32(1).u32(1).u32(1000000000)); !bytes.HasSuffix(got, wire{}.u32(acceptGarbageArgs)) {
		t.Fatalf("a guard of 10^9 nanoseconds: % x", got)
	}
	if got := c.raw(programNFS, 3, procMknod, 501, wire{}.opaque(root).str("f").u32(typeFifo).append(noAttrs).u32(7)); !bytes.HasSuffix(got, wire{}.u32(acceptGarbageArgs)) {
		t.Fatalf("MKNOD with trailing bytes: % x", got)
	}
	link, _ := c.lookup(root, "link")
	if s := c.status(procCommit, wire{}.opaque(link).u64(0).u32(0)); s != uint32(nfsErrInval) {
		t.Fatalf("COMMIT of a symlink: %d", s)
	}
}

// A change of owner by anyone but root drops the set-id bits.
func TestChownDropsSetIDBits(t *testing.T) {
	f, _, c, root := writable(t)
	f.add("tool", 1030, syscall.S_IFREG|0o6755, "x")
	fh, _ := c.lookup(root, "tool")
	chgrp := wire{}.opaque(fh).u32(0).u32(0).u32(1).u32(20).u32(0).u32(timeDontChange).u32(timeDontChange).u32(0)
	if s := c.status(procSetAttr, chgrp); s != uint32(nfsOK) {
		t.Fatalf("chgrp: %d", s)
	}
	f.mu.Lock()
	mode := f.objects[f.root+"/tool"].mode
	f.mu.Unlock()
	if mode&0o7777 != 0o755 {
		t.Fatalf("mode %o", mode&0o7777)
	}
}

// A WRITE of no bytes past the end, even FILE_SYNC, leaves the file as
// it is and answers with the stability asked for.
func TestEmptyWriteChangesNothing(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	r := c.call(procWrite, wire{}.opaque(fh).u64(100).u32(0).u32(stableFileSync).str(""))
	if s := r.u32(); s != uint32(nfsOK) {
		t.Fatalf("WRITE: %d", s)
	}
	r.wcc()
	if n, committed := r.u32(), r.u32(); n != 0 || committed != stableFileSync {
		t.Fatalf("wrote %d, committed %d", n, committed)
	}
	f.mu.Lock()
	size := len(f.objects[f.root+"/a.txt"].data)
	f.mu.Unlock()
	if size != len("hello world") {
		t.Fatalf("the file is %d bytes", size)
	}
}

// RENAME to or from "." or ".." is INVAL (RFC 1813 §3.3.14).
func TestRenameDotNames(t *testing.T) {
	_, _, c, root := writable(t)
	for _, args := range []wire{
		wire{}.opaque(root).str("a.txt").opaque(root).str("."),
		wire{}.opaque(root).str("a.txt").opaque(root).str(".."),
		wire{}.opaque(root).str("..").opaque(root).str("x"),
	} {
		if s := c.status(procRename, args); s != uint32(nfsErrInval) {
			t.Fatalf("RENAME: %d", s)
		}
	}
}

// A mutation whose settling flush fails is still applied: the failure
// stays for COMMIT, which reports it once, so no COMMIT succeeds over the
// lost writes.
func TestSettleLeavesItsFailureToCommit(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	f.mu.Lock()
	f.fsyncErr = fuse.Status(syscall.EDQUOT)
	f.mu.Unlock()
	if s := c.status(procSetAttr, wire{}.opaque(fh).append(modeAttrs(0o600)).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("SETATTR: %d", s)
	}
	f.mu.Lock()
	mode := f.objects[f.root+"/a.txt"].mode & 0o7777
	f.mu.Unlock()
	if mode != 0o600 {
		t.Fatalf("mode %o", mode)
	}
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsErrDQuot) {
		t.Fatalf("COMMIT: %d", s)
	}
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("COMMIT again: %d", s)
	}
}

// A WRITE arriving while a mutation settles the file waits for the
// mutation, instead of taking the file's lease back from it.
func TestWritesWaitForTheMutationSettlingThem(t *testing.T) {
	short(t, 1500*time.Millisecond, 500*time.Millisecond)
	f, srv, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	gate := make(chan struct{})
	f.mu.Lock()
	f.fsyncGate, f.prepareDelay = gate, 100*time.Millisecond
	f.mu.Unlock()
	setAttr := make(chan []byte, 1)
	go func() {
		c.send(callMessage(c.xid+1, programNFS, 3, procSetAttr, 501, wire{}.opaque(fh).append(modeAttrs(0o600)).u32(0)))
		setAttr <- c.receive()
	}()
	time.Sleep(50 * time.Millisecond) // settling, held at the gate
	other := dial(t, srv.listener.Addr().String())
	other.xid = 5000
	other.send(callMessage(5001, programNFS, 3, procWrite, 501, wire{}.opaque(fh).u64(1).u32(1).u32(stableUnstable).str("y")))
	time.Sleep(50 * time.Millisecond) // the WRITE waits for the writer
	close(gate)
	// The status follows the 24-byte accepted reply header.
	if s := binary.BigEndian.Uint32((<-setAttr)[24:]); s != uint32(nfsOK) {
		t.Fatalf("SETATTR: %d", s)
	}
	if s := binary.BigEndian.Uint32(other.receive()[24:]); s != uint32(nfsOK) {
		t.Fatalf("WRITE: %d", s)
	}
}

// CREATE in any mode of a name whose file this mount is writing settles
// that file first, so the create's own lease is free.
func TestCreateSettlesTheFileAtItsName(t *testing.T) {
	short(t, 1500*time.Millisecond, 300*time.Millisecond)
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	if s := c.status(procCreate, wire{}.opaque(root).str("a.txt").u32(createGuarded).append(noAttrs)); s != uint32(nfsErrExist) {
		t.Fatalf("CREATE GUARDED: %d", s)
	}
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	if s := c.status(procCreate, wire{}.opaque(root).str("a.txt").u32(createUnchecked).append(noAttrs)); s != uint32(nfsOK) {
		t.Fatalf("CREATE UNCHECKED: %d", s)
	}
	if f.fsyncsNow() != 2 {
		t.Fatalf("%d flushes", f.fsyncsNow())
	}
}

// A create by anyone but root gives the new object no other owner, and
// a group only of the caller's or the directory's, as knfsd does.
func TestCreatesKeepTheCallersOwnership(t *testing.T) {
	f, _, c, root := writable(t)
	owner := func(uid, gid uint32) wire {
		return wire{}.u32(1).u32(0o4755).u32(1).u32(uid).u32(1).u32(gid).u32(0).u32(timeDontChange).u32(timeDontChange)
	}
	made := func() (uid, gid *uint32) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.madeAttrs.Uid, f.madeAttrs.Gid
	}
	c.call(procCreate, wire{}.opaque(root).str("n").u32(createGuarded).append(owner(0, 0))).created()
	if uid, gid := made(); uid != nil || gid != nil {
		t.Fatalf("CREATE kept %v, %v", uid, gid)
	}
	c.call(procMkdir, wire{}.opaque(root).str("d").append(owner(501, 20))).created()
	if uid, gid := made(); uid == nil || *uid != 501 || gid == nil || *gid != 20 {
		t.Fatalf("MKDIR set %v, %v", uid, gid)
	}
	c.callAs(0, procMkdir, wire{}.opaque(root).str("r").append(owner(7, 7))).created()
	if uid, gid := made(); uid == nil || *uid != 7 || gid == nil || *gid != 7 {
		t.Fatalf("root's MKDIR set %v, %v", uid, gid)
	}
}

// A flush the gateway asks to retry later keeps the writes: COMMIT and a
// mutation settling them answer "retry later", and a retry flushes them.
func TestFlushToRetryLaterKeepsTheWrites(t *testing.T) {
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	f.mu.Lock()
	f.fsyncErr = fuse.Status(syscall.EAGAIN)
	f.mu.Unlock()
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsErrJukebox) {
		t.Fatalf("COMMIT: %d", s)
	}
	if s := c.status(procSetAttr, wire{}.opaque(fh).append(modeAttrs(0o600)).u32(0)); s != uint32(nfsErrJukebox) {
		t.Fatalf("SETATTR: %d", s)
	}
	f.mu.Lock()
	f.fsyncErr = fuse.OK
	releases := f.releases
	f.mu.Unlock()
	if releases != 0 {
		t.Fatalf("%d releases", releases)
	}
	if s := c.status(procCommit, wire{}.opaque(fh).u64(0).u32(0)); s != uint32(nfsOK) || f.fsyncsNow() != 3 {
		t.Fatalf("COMMIT retried: %d, %d flushes", s, f.fsyncsNow())
	}
}

// A clean stop counts the files whose accepted writes it could not
// persist, dropping them.
func TestFlushOnStopCountsWhatItLost(t *testing.T) {
	f, srv, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	f.mu.Lock()
	f.fsyncErr = fuse.Status(syscall.EAGAIN)
	f.mu.Unlock()
	if lost := srv.Flush(); lost != 1 {
		t.Fatalf("%d lost", lost)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.discards != 1 {
		t.Fatalf("%d discards", f.discards)
	}
}

// An idle flush is its last writer's, as a COMMIT is.
func TestIdleFlushIsTheWritersOwn(t *testing.T) {
	saved := writeIdle
	writeIdle = 50 * time.Millisecond
	defer func() { writeIdle = saved }()
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.callAs(0, procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	for deadline := time.Now().Add(5 * time.Second); f.releasesNow() == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the idle handle was not released")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.fsyncUids) != 1 || f.fsyncUids[0] != 0 {
		t.Fatalf("flushed as %v", f.fsyncUids)
	}
}

// An idle flush whose time came while a WRITE used the writer waits for
// the idle time that WRITE started.
func TestIdleTimeRestartsWithEachWrite(t *testing.T) {
	saved := writeIdle
	writeIdle = 300 * time.Millisecond
	defer func() { writeIdle = saved }()
	f, _, c, root := writable(t)
	fh, _ := c.lookup(root, "a.txt")
	c.call(procWrite, wire{}.opaque(fh).u64(0).u32(1).u32(stableUnstable).str("x"))
	gate := make(chan struct{})
	f.mu.Lock()
	f.writeGate = gate
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.call(procWrite, wire{}.opaque(fh).u64(1).u32(1).u32(stableUnstable).str("y"))
	}()
	time.Sleep(400 * time.Millisecond) // the first idle time passes
	f.mu.Lock()
	f.writeGate = nil
	f.mu.Unlock()
	close(gate)
	<-done
	time.Sleep(100 * time.Millisecond)
	if f.releasesNow() != 0 {
		t.Fatal("released at once after the WRITE")
	}
}

// A mutation's own step ends in time for the engine to let its locks go
// before the call's deadline.
func TestMutationLeavesTimeToRelease(t *testing.T) {
	o := &op{call: &call{received: time.Now()}, cancel: make(chan struct{})}
	step, done := (&service{}).step(o)
	defer done()
	if end, _ := step.Mutate.Deadline(); !end.Equal(o.received.Add(rpcDeadline - weedmount.NFSReleaseTime)) {
		t.Fatalf("the mutation ends %v after the call came", end.Sub(o.received))
	}
}

// A CREATE looks its name up to settle the file there only while this
// export holds writes.
func TestCreateLooksUpOnlyWhileWriting(t *testing.T) {
	f, _, c, root := writable(t)
	lookups := 0
	f.afterLookup = func() { lookups++ }
	if s := c.status(procCreate, wire{}.opaque(root).str("a.txt").u32(createGuarded).append(noAttrs)); s != uint32(nfsErrExist) || lookups != 0 {
		t.Fatalf("CREATE: %d, %d lookups", s, lookups)
	}
}
