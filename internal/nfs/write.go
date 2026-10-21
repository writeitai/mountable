package nfs

import (
	"crypto/rand"
	"sync"
	"syscall"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
)

// WRITE and COMMIT (design §4, "Writes"). A WRITE adds its data to one
// shared WFS write handle per dirty inode, holding the inode's -dlm write
// lease, and replies UNSTABLE with this process's write verifier. COMMIT,
// and a WRITE asking for DATA_SYNC or FILE_SYNC, waits for WFS to persist
// the chunks and the entry, then replies FILE_SYNC. A dirty handle also
// flushes once idle for writeIdle, and once writeFlushBytes were written
// to it, so data reaches whole chunks without one entry write per request;
// a clean, idle handle is released with its lease. A failed flush is kept,
// sticky, until a COMMIT or stable WRITE reports it: a COMMIT never
// succeeds later because its handle went away.

// stable_how (RFC 1813 §3.3.7).
const (
	stableUnstable = 0
	stableDataSync = 1
	stableFileSync = 2
)

var (
	// writeIdle is how long a dirty handle waits for more writes before it
	// flushes, and a clean one before it is released.
	writeIdle = 2 * time.Second
	// writeFlushBytes is how much a handle takes before it flushes without
	// waiting to be idle.
	writeFlushBytes int64 = 64 << 20
)

// writer is one inode's shared write handle.
type writer struct {
	inode uint64
	// slot is held while a call or a flush uses the writer.
	slot chan struct{}
	fh   uint64 // the WFS handle; 0 when none is open
	// dirty is set once a write was accepted and not yet persisted;
	// unflushed counts its bytes.
	dirty     bool
	unflushed int64
	// failed is a flush's failure not yet reported.
	failed nfsStatus
	timer  *time.Timer
}

// writes holds the writers, by inode.
type writes struct {
	s        *service
	verifier [8]byte
	mu       sync.Mutex
	byInode  map[uint64]*writer
	ended    bool
}

func newWrites(s *service) (*writes, error) {
	w := &writes{s: s, byInode: map[uint64]*writer{}}
	if _, err := rand.Read(w.verifier[:]); err != nil {
		return nil, err
	}
	return w, nil
}

// take returns inode's writer, made if new, with its slot held, or false
// once cancel closes first.
func (t *writes) take(inode uint64, cancel <-chan struct{}) (*writer, bool) {
	t.mu.Lock()
	w := t.byInode[inode]
	if w == nil {
		w = &writer{inode: inode, slot: make(chan struct{}, 1)}
		t.byInode[inode] = w
	}
	t.mu.Unlock()
	select {
	case w.slot <- struct{}{}:
	case <-cancel:
		return nil, false
	}
	t.mu.Lock()
	current := t.byInode[inode]
	t.mu.Unlock()
	if current != w { // dropped while this call waited for it
		<-w.slot
		return t.take(inode, cancel)
	}
	return w, true
}

// give lets the writer go, scheduling its idle flush while it has a
// handle, and dropping it when it holds nothing.
func (t *writes) give(w *writer) {
	switch {
	case w.fh != 0:
		t.schedule(w, writeIdle)
	case w.failed == nfsOK:
		t.mu.Lock()
		if t.byInode[w.inode] == w {
			delete(t.byInode, w.inode)
		}
		t.mu.Unlock()
	}
	<-w.slot
}

// schedule flushes and releases the writer after d, unless the export
// ended. Timers are set under t.mu, which end stops them under.
func (t *writes) schedule(w *writer, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	if !t.ended {
		w.timer = time.AfterFunc(d, func() { t.idle(w) })
	}
}

// idle flushes a writer nothing used for writeIdle, recording a failure,
// and releases its handle, which lets its lease go. A writer replaced or
// ended while it waited is left alone.
func (t *writes) idle(w *writer) {
	w.slot <- struct{}{}
	t.mu.Lock()
	current := t.byInode[w.inode] == w && !t.ended
	t.mu.Unlock()
	if !current {
		<-w.slot
		return
	}
	t.release(w)
	t.give(w)
}

// end stops every idle flush: an export that ended flushes nothing more.
func (t *writes) end() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ended = true
	for _, w := range t.byInode {
		if timer := w.timer; timer != nil {
			timer.Stop()
		}
	}
}

// flush persists what the writer accepted. A failure is kept until it is
// reported, and the handle, whose writes may be lost with it, released.
func (t *writes) flush(w *writer, cred credential) nfsStatus {
	if w.fh == 0 || !w.dirty {
		return nfsOK
	}
	h := header(w.inode, cred)
	if st := t.s.engine.Fsync(nil, &fuse.FsyncIn{InHeader: h, Fh: w.fh}); st != fuse.OK {
		w.failed = statusOf(st)
		t.closeHandle(w, true)
		return w.failed
	}
	w.dirty, w.unflushed = false, 0
	return nfsOK
}

// release flushes the writer, then closes its handle.
func (t *writes) release(w *writer) {
	t.flush(w, credential{uid: t.s.uid})
	t.closeHandle(w, false)
}

// closeHandle closes the writer's handle, letting its lease go, without a
// flush; discard drops what a failed flush left behind.
func (t *writes) closeHandle(w *writer, discard bool) {
	if w.fh == 0 {
		return
	}
	t.s.engine.NFSCloseWrite(weedmount.FileHandleId(w.fh), discard)
	w.fh, w.dirty, w.unflushed = 0, false, 0
}

// report hands a failure not reported yet to the call, and lets the
// writer go. The failure counts as reported only when the call will be
// answered with it: a call already answered "retry later" leaves it for
// the next.
func (t *writes) report(o *op, w *writer) nfsStatus {
	failed := w.failed
	if failed != nfsOK {
		if o.beginMutation() {
			w.failed = nfsOK
		} else {
			failed = nfsErrJukebox
		}
	}
	t.give(w)
	return failed
}

// settle persists inode's accepted writes and releases its handle, as a
// mutation's preparation does before it changes the file (design §4,
// "Step 1"). A failure not reported yet is this call's. A flush outlasting
// the call goes on, holding the writer.
func (t *writes) settle(o *op, inode uint64) nfsStatus {
	t.mu.Lock()
	_, known := t.byInode[inode]
	t.mu.Unlock()
	if !known {
		return nfsOK
	}
	w, ok := t.take(inode, o.cancel)
	if !ok {
		return nfsErrJukebox
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		t.release(w)
	}()
	select {
	case <-done:
	case <-o.cancel:
		go func() {
			<-done
			t.give(w)
		}()
		return nfsErrJukebox
	}
	return t.report(o, w)
}

// flushAll persists and releases every writer, as a clean stop does.
func (t *writes) flushAll() {
	t.mu.Lock()
	all := make([]*writer, 0, len(t.byInode))
	for _, w := range t.byInode {
		all = append(all, w)
	}
	t.mu.Unlock()
	for _, w := range all {
		w.slot <- struct{}{}
		t.mu.Lock()
		if w.timer != nil {
			w.timer.Stop()
		}
		t.mu.Unlock()
		t.release(w)
		<-w.slot
	}
}

func (s *service) write(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	offset, count, stable := d.uint64(), d.uint32(), d.uint32()
	data := d.opaque(maxTransfer)
	if !d.done() || stable > stableFileSync || int(count) != len(data) {
		return garbage()
	}
	obj, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	fail := func(status nfsStatus) reply {
		if status == nfsErrJukebox {
			return s.retryLater(o.call)
		}
		e := &encoder{}
		e.uint32(uint32(status))
		s.putWcc(e, &obj.attr, s.now(o, obj))
		return reply{body: e.buf}
	}
	switch {
	case isDir(&obj.attr):
		return fail(nfsErrIsDir)
	case obj.attr.Mode&syscall.S_IFMT != syscall.S_IFREG:
		return fail(nfsErrInval)
	case !mayWrite(o.cred, &obj.attr):
		return fail(nfsErrAccess)
	case offset+uint64(count) > maxFileSize || offset+uint64(count) < offset:
		return fail(nfsErrFBig)
	}
	w, ok := s.writes.take(obj.inode, o.cancel)
	if !ok {
		return fail(nfsErrJukebox)
	}
	written, status := s.writes.accept(o, w, offset, data)
	if status != nfsOK {
		s.writes.give(w)
		return fail(status)
	}
	committed := uint32(stableUnstable)
	if stable != stableUnstable {
		if status := s.writes.commit(o, w); status != nfsOK {
			return fail(status)
		}
		committed = stableFileSync
	} else if w.unflushed >= writeFlushBytes {
		// Flushed now, the handle and its lease kept: the writer goes with
		// the flush, which lets it go.
		go func(cred credential) {
			s.writes.flush(w, cred)
			s.writes.give(w)
		}(o.cred)
	} else {
		s.writes.give(w)
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	s.putWcc(e, &obj.attr, s.now(o, obj))
	e.uint32(written)
	e.uint32(committed)
	e.buf = append(e.buf, s.writes.verifier[:]...)
	return reply{body: e.buf}
}

// accept adds data at offset to the writer's handle, opened first if it
// has none, with the file's write lease taken within the call's
// preparation budget.
func (t *writes) accept(o *op, w *writer, offset uint64, data []byte) (uint32, nfsStatus) {
	if w.fh == 0 {
		step, done := t.s.step(o)
		fh, st := t.s.engine.NFSOpenForWrite(step.Prepare, w.inode, o.cred.uid, o.cred.gid)
		done()
		if st != fuse.OK {
			return 0, resultOf(st)
		}
		w.fh = uint64(fh)
	}
	if len(data) == 0 {
		return 0, nfsOK // nothing to write: the file stays as it is
	}
	h := header(w.inode, o.cred)
	written, st := t.s.engine.Write(o.cancel, &fuse.WriteIn{InHeader: h, Fh: w.fh, Offset: offset, Size: uint32(len(data))}, data)
	if st != fuse.OK {
		return 0, statusOf(st)
	}
	w.dirty = true
	w.unflushed += int64(written)
	return written, nfsOK
}

func (s *service) commit(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	d.uint64() // offset and count: the whole file is persisted
	d.uint32()
	if !d.done() {
		return garbage()
	}
	obj, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	switch {
	case isDir(&obj.attr):
		status = nfsErrIsDir
	case obj.attr.Mode&syscall.S_IFMT != syscall.S_IFREG:
		status = nfsErrInval
	default:
		status = s.writes.commitInode(o, obj.inode)
	}
	if status == nfsErrJukebox {
		return s.retryLater(o.call)
	}
	e := &encoder{}
	e.uint32(uint32(status))
	s.putWcc(e, &obj.attr, s.now(o, obj))
	if status == nfsOK {
		e.buf = append(e.buf, s.writes.verifier[:]...)
	}
	return reply{body: e.buf}
}

// commitInode persists inode's accepted writes, reporting a failure not
// reported yet.
func (t *writes) commitInode(o *op, inode uint64) nfsStatus {
	t.mu.Lock()
	_, known := t.byInode[inode]
	t.mu.Unlock()
	if !known {
		return nfsOK // nothing accepted, or all persisted and reported
	}
	w, ok := t.take(inode, o.cancel)
	if !ok {
		return nfsErrJukebox
	}
	return t.commit(o, w)
}

// commit persists the writer's accepted writes for a call, and lets the
// writer go. The call is answered before its deadline: a flush still
// running then goes on, holding the writer, and the call answers "retry
// later"; the retry waits for the writer and reports how the flush went.
// A failure is reported once.
func (t *writes) commit(o *op, w *writer) nfsStatus {
	done := make(chan struct{})
	go func(cred credential) {
		defer close(done)
		t.flush(w, cred)
	}(o.cred)
	limit := time.NewTimer(time.Until(o.received.Add(rpcDeadline - time.Second)))
	defer limit.Stop()
	select {
	case <-done:
	case <-limit.C:
		go func() {
			<-done
			t.give(w)
		}()
		return nfsErrJukebox
	}
	return t.report(o, w)
}

// mayWrite is WRITE's own check, with RFC 1813 §4.4's owner exception.
func mayWrite(cred credential, a *fuse.Attr) bool {
	return cred.uid == a.Uid || allowed(cred, a, permWrite)
}

// exclusiveSet holds the files this process created through CREATE
// EXCLUSIVE that have not had a SETATTR since: their stored times hold the
// verifier (design §4, "CREATE EXCLUSIVE").
type exclusiveSet struct {
	mu     sync.Mutex
	inodes map[uint64]struct{}
}

func (e *exclusiveSet) add(inode uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inodes[inode] = struct{}{}
}

func (e *exclusiveSet) holds(inode uint64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.inodes[inode]
	return ok
}

func (e *exclusiveSet) remove(inode uint64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.inodes, inode)
}
