package nfs

import (
	"bytes"
	"hash/fnv"
	"io"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

// Byte-for-byte replies, from fixtures assembled by hand.

func TestNullCallsReplyEmpty(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	for _, prog := range []uint32{programNFS, programMount} {
		got := c.raw(prog, 3, 0, 501, nil)
		want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(0)
		if !bytes.Equal(got, want) {
			t.Errorf("program %d NULL: % x, want % x", prog, got, want)
		}
	}
}

// NULL needs no credential at all.
func TestNullWithAuthNone(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	c.send(wire{}.u32(7).u32(0).u32(2).u32(programNFS).u32(3).u32(0).u32(0).u32(0).u32(0).u32(0))
	want := wire{}.u32(7).u32(1).u32(0).u32(0).u32(0).u32(0)
	if got := c.receive(); !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
}

func TestOnlyVersion3IsServed(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	for _, tc := range []struct{ prog, vers uint32 }{{programNFS, 4}, {programNFS, 2}, {programMount, 1}} {
		got := c.raw(tc.prog, tc.vers, 0, 501, nil)
		// PROG_MISMATCH with mismatch_info {3, 3}.
		want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(2).u32(3).u32(3)
		if !bytes.Equal(got, want) {
			t.Errorf("%d v%d: % x, want % x", tc.prog, tc.vers, got, want)
		}
	}
	// Any other program: PROG_UNAVAIL.
	got := c.raw(100021, 4, 0, 501, nil)
	want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(1)
	if !bytes.Equal(got, want) {
		t.Errorf("NLM: % x, want % x", got, want)
	}
}

func TestOtherRPCVersionIsDenied(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	c.send(wire{}.u32(9).u32(0).u32(3).u32(programNFS).u32(3).u32(0).u32(0).u32(0).u32(0).u32(0))
	// MSG_DENIED, RPC_MISMATCH, {2, 2}.
	want := wire{}.u32(9).u32(1).u32(1).u32(0).u32(2).u32(2)
	if got := c.receive(); !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
}

func TestCallsNeedAuthSys(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	fh := c.root()
	args := wire{}.opaque(fh)
	c.send(append(wire{}.u32(11).u32(0).u32(2).u32(programNFS).u32(3).u32(procGetAttr).u32(0).u32(0).u32(0).u32(0), args...))
	// MSG_DENIED, AUTH_ERROR, AUTH_TOOWEAK, big-endian.
	want := wire{}.u32(11).u32(1).u32(1).u32(1).u32(5)
	if got := c.receive(); !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
	// A malformed AUTH_SYS body: AUTH_BADCRED.
	c.send(wire{}.u32(12).u32(0).u32(2).u32(programNFS).u32(3).u32(procGetAttr).u32(1).opaque(wire{}.u32(0)).u32(0).u32(0))
	want = wire{}.u32(12).u32(1).u32(1).u32(1).u32(1)
	if got := c.receive(); !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
}

func TestGetAttrRootFixture(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	fh := c.root()
	if len(fh) != 48 {
		t.Fatalf("root handle of %d bytes", len(fh))
	}
	got := c.raw(programNFS, 3, procGetAttr, 501, wire{}.opaque(fh))
	h := fnv.New64a()
	h.Write([]byte("/c/fs1"))
	want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(0).
		u32(0).                                    // NFS3_OK
		u32(2).u32(0o755).u32(2).u32(501).u32(20). // NF3DIR, mode, nlink, uid, gid
		u64(0).u64(0).                             // size, used
		u32(0).u32(0).                             // rdev
		u64(h.Sum64() | 1).u64(1).                 // fsid, fileid
		u64(0).u64(0).u64(0)                       // atime, mtime, ctime
	if !bytes.Equal(got, want) {
		t.Fatalf("\n got % x\nwant % x", got, want)
	}
}

func TestMountOnlyExportsRoot(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	r := c.accepted(c.raw(programMount, 3, mountProcMnt, 501, wire{}.str("/c/fs1")))
	if s := r.u32(); s != mountErrNoEnt {
		t.Fatalf("MNT of another path: %d", s)
	}
	for _, proc := range []uint32{mountProcDump, mountProcExport} {
		got := c.raw(programMount, 3, proc, 501, nil)
		want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(0).u32(0)
		if !bytes.Equal(got, want) {
			t.Errorf("proc %d: % x, want an empty list", proc, got)
		}
	}
	got := c.raw(programMount, 3, mountProcUmnt, 501, wire{}.str("/"))
	if want := (wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(0)); !bytes.Equal(got, want) {
		t.Errorf("UMNT: % x", got)
	}
	r = c.accepted(c.raw(programMount, 3, mountProcMnt, 502, wire{}.str("/")))
	if s := r.u32(); s != mountErrAccess {
		t.Fatalf("MNT by another user: %d", s)
	}
}

// A record in several fragments is one call.
func TestFragmentedRecord(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	msg := callMessage(21, programNFS, 3, 0, 501, nil)
	first, second := msg[:10], msg[10:]
	frames := append(wire{}.u32(uint32(len(first))), first...)
	frames = append(frames.u32(1<<31|uint32(len(second))), second...)
	if _, err := c.conn.Write(frames); err != nil {
		t.Fatal(err)
	}
	want := wire{}.u32(21).u32(1).u32(0).u32(0).u32(0).u32(0)
	if got := c.receive(); !bytes.Equal(got, want) {
		t.Fatalf("% x", got)
	}
}

// A record over the size limit ends the connection before it is read.
func TestOversizedRecordClosesTheConnection(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	if _, err := c.conn.Write(wire{}.u32(1<<31 | (maxRecord + 1))); err != nil {
		t.Fatal(err)
	}
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("read %v, want the connection closed", err)
	}
}

// Close ends every connection, not only the listener.
func TestCloseEndsConnections(t *testing.T) {
	srv, c := startServer(t, newFakeEngine())
	c.raw(programNFS, 3, 0, 501, nil) // the connection is being served
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("read %v, want the connection closed", err)
	}
}

// An empty fragment that is not the last would never end the record: the
// connection is closed.
func TestEmptyFragmentsCloseTheConnection(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	if _, err := c.conn.Write(wire{}.u32(0).u32(0).u32(0)); err != nil {
		t.Fatal(err)
	}
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("read %v, want the connection closed", err)
	}
}

// Arguments with bytes left over are garbage, for every program.
func TestTrailingBytesAreGarbage(t *testing.T) {
	_, c := startServer(t, newFakeEngine())
	fh := c.root()
	garbage := func(prog, proc uint32, args wire) {
		t.Helper()
		got := c.raw(prog, 3, proc, 501, args)
		if want := (wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(4)); !bytes.Equal(got, want) {
			t.Errorf("program %d procedure %d: % x", prog, proc, got)
		}
	}
	garbage(programNFS, procNull, wire{}.u32(0))
	garbage(programNFS, procGetAttr, wire{}.opaque(fh).u32(0))
	garbage(programNFS, procLookup, wire{}.opaque(fh).str("a").u32(0))
	garbage(programNFS, procRead, wire{}.opaque(fh).u64(0).u32(1).u32(0))
	garbage(programNFS, procReadDirPlus, wire{}.opaque(fh).u64(0).u64(0).u32(1).u32(1).u32(0))
	garbage(programMount, mountProcNull, wire{}.u32(0))
	garbage(programMount, mountProcMnt, wire{}.str("/").u32(0))
	garbage(programMount, mountProcUmnt, wire{}.str("/").u32(0))
	garbage(programMount, mountProcUmnt, wire{})
	garbage(programMount, mountProcDump, wire{}.u32(0))
}

func TestDecoderBounds(t *testing.T) {
	// A length past the data, and one past the limit, never allocate.
	for _, b := range [][]byte{wire{}.u32(1 << 30), wire{}.u32(65).opaque(make([]byte, 65))} {
		d := &decoder{buf: b}
		if d.opaque(64); d.err == nil {
			t.Errorf("% x decoded", b[:4])
		}
	}
	d := &decoder{buf: wire{}.u32(2)}
	if d.bool(); d.err == nil {
		t.Error("a bool of 2 decoded")
	}
	d = &decoder{buf: wire{}.str("abcde").u32(7)}
	if s := d.string(10); s != "abcde" || d.uint32() != 7 || d.err != nil {
		t.Errorf("padding mishandled: %q %v", s, d.err)
	}
}

func TestReplyCache(t *testing.T) {
	c := newReplyCache()
	k := replyKey{xid: 1}
	e, state := c.begin(k, nil)
	if state != drcNew {
		t.Fatal("a new call was found")
	}
	if _, state := c.begin(k, nil); state != drcInProgress {
		t.Fatal("a call in progress was not recognised")
	}
	c.finish(e, reply{body: []byte{1}})
	if again, state := c.begin(k, nil); state != drcDone || !bytes.Equal(again.r.body, []byte{1}) {
		t.Fatal("the reply was not kept")
	}
	// "Retry later" is never kept: the resent call runs again.
	j := replyKey{xid: 2}
	e, _ = c.begin(j, nil)
	c.finish(e, reply{retry: true})
	if _, state := c.begin(j, nil); state != drcNew {
		t.Fatal("a retry-later reply was cached")
	}
}

// Finished entries make room; unfinished ones are never evicted, and with
// every entry unfinished a new call is turned away.
func TestReplyCacheNeverEvictsUnfinished(t *testing.T) {
	c := newReplyCache()
	running, _ := c.begin(replyKey{xid: 1}, nil)
	for i := range drcSize - 1 {
		e, _ := c.begin(replyKey{xid: uint32(100 + i)}, nil)
		c.finish(e, reply{})
	}
	for i := range drcSize - 1 { // each evicts a finished entry
		if _, state := c.begin(replyKey{xid: uint32(5000 + i)}, nil); state != drcNew {
			t.Fatalf("call %d: state %d", i, state)
		}
	}
	if _, state := c.begin(replyKey{xid: 1}, nil); state != drcInProgress {
		t.Fatal("the unfinished entry was evicted")
	}
	if _, state := c.begin(replyKey{xid: 9999}, nil); state != drcFull {
		t.Fatalf("state %d with every entry unfinished", state)
	}
	c.finish(running, reply{})
	if len(c.entries) > drcSize || c.order.Len() != len(c.entries) {
		t.Fatalf("%d entries", len(c.entries))
	}
}

// The cache key covers the arguments and the whole credential, so a reused
// XID with other contents is a new call.
func TestReplyKeyCoversArguments(t *testing.T) {
	a := callFor(5, procRemove, []byte("one"))
	other := func(change func(c *call)) *call {
		c := *a
		c.cred.gids = slices.Clone(a.cred.gids)
		change(&c)
		return &c
	}
	for name, c := range map[string]*call{
		"arguments":      other(func(c *call) { c.args = []byte("two") }),
		"user":           other(func(c *call) { c.cred.uid = 9 }),
		"a group":        other(func(c *call) { c.cred.gids[0] = 21 }),
		"one more group": other(func(c *call) { c.cred.gids = append(c.cred.gids, 80) }),
		"no groups":      other(func(c *call) { c.cred.gids = nil }),
	} {
		if a.key() == c.key() {
			t.Errorf("other %s, same key", name)
		}
	}
}

// A resent mutating call gets the cached reply; procedures that change
// nothing are not cached.
func TestRetransmissionIsAnsweredFromTheCache(t *testing.T) {
	srv, c := startServer(t, newFakeEngine())
	fh := c.root()
	args := wire{}.opaque(fh).str("x")
	c.send(callMessage(77, programNFS, 3, procRemove, 501, args))
	first := c.receive()
	c.send(callMessage(77, programNFS, 3, procRemove, 501, args))
	if again := c.receive(); !bytes.Equal(first, again) {
		t.Fatal("the resent call got another reply")
	}
	if _, state := srv.drc.begin(callFor(77, procRemove, args).key(), nil); state != drcDone {
		t.Fatal("REMOVE's reply was not cached")
	}
	if nonIdempotent(procGetAttr) || nonIdempotent(procRead) || !nonIdempotent(procWrite) {
		t.Fatal("wrong procedures are cached")
	}
}

// slowRemove makes REMOVE wait for release, standing in for slice 2's
// mutations.
func slowRemove(t *testing.T) (started, release chan struct{}) {
	started, release = make(chan struct{}, 4), make(chan struct{})
	saved := procedures[procRemove]
	procedures[procRemove] = procedure{fn: func(s *service, o *op) reply {
		started <- struct{}{}
		<-release
		return s.failure(o.call, nfsErrROFS)
	}}
	t.Cleanup(func() { procedures[procRemove] = saved })
	return started, release
}

// A client that reconnects while its call is still running resends it on
// the new connection, and gets the answer there, without a second run.
func TestRetransmissionAfterReconnect(t *testing.T) {
	started, release := slowRemove(t)
	srv, c := startServer(t, newFakeEngine())
	fh := c.root()
	args := wire{}.opaque(fh).str("x")
	c.send(callMessage(88, programNFS, 3, procRemove, 501, args))
	<-started
	again := dial(t, srv.listener.Addr().String())
	again.send(callMessage(88, programNFS, 3, procRemove, 501, args))
	close(release)
	got := again.receive()
	want := wire{}.u32(88).u32(1).u32(0).u32(0).u32(0).u32(0).u32(uint32(nfsErrROFS)).u32(0).u32(0)
	if !bytes.Equal(got, want) {
		t.Fatalf("% x, want % x", got, want)
	}
	select {
	case <-started:
		t.Fatal("the resent call ran again")
	default:
	}
}

// With every pending slot taken, calls are still refused for what they
// are, and turning one away leaves the cache alone.
func TestAdmissionComesFirst(t *testing.T) {
	srv, c := startServer(t, newFakeEngine())
	fh := c.root()
	for range maxPending {
		srv.pending <- struct{}{}
	}
	if s := c.callAs(502, procGetAttr, wire{}.opaque(fh)).u32(); s != uint32(nfsErrAccess) {
		t.Fatalf("another user: %d", s)
	}
	c.send(append(wire{}.u32(11).u32(0).u32(2).u32(programNFS).u32(3).u32(procRemove).u32(0).u32(0).u32(0).u32(0), wire{}.opaque(fh)...))
	if got, want := c.receive(), (wire{}.u32(11).u32(1).u32(1).u32(1).u32(5)); !bytes.Equal(got, want) {
		t.Fatalf("AUTH_NONE: % x", got)
	}
	if s := c.status(procRemove, wire{}.opaque(fh).str("x")); s != uint32(nfsErrJukebox) {
		t.Fatalf("a call without a slot: %d", s)
	}
	if len(srv.drc.entries) != 0 {
		t.Fatal("a call turned away entered the cache")
	}
	srv.End()
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != uint32(nfsErrIO) {
		t.Fatalf("after End: %d", s)
	}
}

func callFor(xid, proc uint32, args []byte) *call {
	return &call{xid: xid, prog: programNFS, vers: 3, proc: proc, cred: credential{flavor: authSys, uid: 501, gid: 20, gids: []uint32{20}}, args: args}
}

func TestHandles(t *testing.T) {
	h, err := newHandles()
	if err != nil {
		t.Fatal(err)
	}
	fh := h.make(42, 7)
	if len(fh) != handleSize {
		t.Fatalf("%d bytes", len(fh))
	}
	if inode, inc, st := h.parse(fh); st != nfsOK || inode != 42 || inc != 7 {
		t.Fatalf("parsed %d %d %d", inode, inc, st)
	}
	// Each field is covered by the MAC.
	for i := 4; i < handleSize; i++ {
		bad := bytes.Clone(fh)
		bad[i] ^= 1
		if _, _, st := h.parse(bad); st != nfsErrStale {
			t.Fatalf("byte %d flipped: status %d", i, st)
		}
	}
	other, _ := newHandles()
	if _, _, st := other.parse(fh); st != nfsErrStale {
		t.Fatalf("another process's handle: status %d", st)
	}
	// Same key, another epoch: stale even with a valid MAC.
	other.key = h.key
	if _, _, st := other.parse(other.make(42, 7)); st != nfsOK {
		t.Fatal("sanity")
	}
	if _, _, st := h.parse(other.make(42, 7)); st != nfsErrStale {
		t.Fatalf("another epoch: status %d", st)
	}
	for _, bad := range [][]byte{fh[:47], append(bytes.Clone(fh), 0), append([]byte("XXXX"), fh[4:]...)} {
		if _, _, st := h.parse(bad); st != nfsErrBadFH {
			t.Fatalf("malformed handle: status %d", st)
		}
	}
}

func TestStatusMapping(t *testing.T) {
	cases := map[uint32]nfsStatus{
		0:                            nfsOK,
		uint32(syscall.ENOENT):       nfsErrNoEnt,
		uint32(syscall.EACCES):       nfsErrAccess,
		uint32(syscall.EPERM):        nfsErrPerm,
		uint32(syscall.EROFS):        nfsErrROFS,
		uint32(syscall.ENOSPC):       nfsErrNoSpc,
		uint32(syscall.EDQUOT):       nfsErrDQuot,
		uint32(syscall.ENOTDIR):      nfsErrNotDir,
		uint32(syscall.EISDIR):       nfsErrIsDir,
		uint32(syscall.EEXIST):       nfsErrExist,
		uint32(syscall.ENOTEMPTY):    nfsErrNotEmpty,
		uint32(syscall.ENAMETOOLONG): nfsErrNameLong,
		uint32(syscall.EINVAL):       nfsErrInval,
		uint32(syscall.EFBIG):        nfsErrFBig,
		uint32(syscall.EXDEV):        nfsErrXDev,
		uint32(syscall.EMLINK):       nfsErrMLink,
		uint32(syscall.ENOTSUP):      nfsErrNotSupp,
		uint32(syscall.EIO):          nfsErrIO,
		uint32(syscall.EAGAIN):       nfsErrIO,
		uint32(syscall.ESTALE):       nfsErrIO,
	}
	for errno, want := range cases {
		if got := statusOf(fuse.Status(errno)); got != want {
			t.Errorf("errno %d: %d, want %d", errno, got, want)
		}
	}
}

// With every pending slot taken, a copy of a finished call still gets its
// reply, and a copy of a running call resent on a new connection still
// gets the first copy's answer: told to retry later instead, the client
// would resend it under a new XID and it would run twice.
func TestDuplicatesUnderSaturation(t *testing.T) {
	started, release := slowRemove(t)
	srv, c := startServer(t, newFakeEngine())
	fh := c.root()
	args := wire{}.opaque(fh).str("x")
	c.send(callMessage(60, programNFS, 3, procCreate, 501, args))
	finished := c.receive()
	c.send(callMessage(61, programNFS, 3, procRemove, 501, args))
	<-started
	for range maxPending - 1 {
		srv.pending <- struct{}{}
	}
	c.send(callMessage(60, programNFS, 3, procCreate, 501, args))
	if got := c.receive(); !bytes.Equal(got, finished) {
		t.Fatalf("finished duplicate: % x", got)
	}
	again := dial(t, srv.listener.Addr().String())
	again.send(callMessage(61, programNFS, 3, procRemove, 501, args))
	close(release)
	got := again.receive()
	if want := (wire{}.u32(61).u32(1).u32(0).u32(0).u32(0).u32(0).u32(uint32(nfsErrROFS))); !bytes.HasPrefix(got, want) {
		t.Fatalf("running duplicate: % x", got)
	}
	select {
	case <-started:
		t.Fatal("the duplicate ran again")
	default:
	}
}
