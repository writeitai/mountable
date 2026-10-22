package nfs

import (
	"crypto/rand"
	"slices"
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
// succeeds later because its handle went away. A flush the gateway asks
// to retry later keeps what it could not persist, to be flushed again.

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
	// cred is the last writer's, whose flushes the idle ones are.
	cred credential
	// timer is the idle flush; gen tells the one last scheduled.
	timer *time.Timer
	gen   uint64
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

// none reports whether no writer is held: nothing accepted, or being
// written, waits to be settled.
func (t *writes) none() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byInode) == 0
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
	w.gen++
	if gen := w.gen; !t.ended {
		w.timer = time.AfterFunc(d, func() { t.idle(w, gen) })
	}
}

// idle flushes a writer nothing used for writeIdle, recording a failure,
// and releases its handle, which lets its lease go. A writer replaced,
// rescheduled or ended while it waited is left alone.
func (t *writes) idle(w *writer, gen uint64) {
	w.slot <- struct{}{}
	t.mu.Lock()
	current := t.byInode[w.inode] == w && w.gen == gen && !t.ended
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
// reported, and the handle, whose writes may be lost with it, released;
// a flush to retry later keeps the handle and its writes, and answers
// "retry later".
func (t *writes) flush(w *writer, cred credential) nfsStatus {
	if w.fh == 0 || !w.dirty {
		return nfsOK
	}
	switch st := t.s.engine.NFSFsync(weedmount.FileHandleId(w.fh), cred.uid, cred.gid); st {
	case fuse.OK:
	case weedmount.NFSRetryLater:
		return nfsErrJukebox // unavailable, out of time, or over the metadata rate
	default:
		w.failed = statusOf(st)
		t.closeHandle(w, true)
		return w.failed
	}
	w.dirty, w.unflushed = false, 0
	return nfsOK
}

// release flushes the writer with its last writer's credential, then
// closes its handle, unless the flush is to be retried later.
func (t *writes) release(w *writer) nfsStatus {
	if status := t.flush(w, w.cred); status == nfsErrJukebox {
		return status
	}
	t.closeHandle(w, false)
	return nfsOK
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

// settle persists the accepted writes of the inodes a mutation changes
// and releases their handles, as its preparation does before it changes
// them (design §4, "Step 1"), and holds their writers until the returned
// give: a WRITE then waits for the mutation instead of taking the lease
// back from it. Writers are taken in inode order. A failed flush stays
// for a COMMIT or stable WRITE to report; the mutation goes on. A flush
// outlasting the call goes on, holding its writer, and the call, as one
// to be retried, answers "retry later".
func (t *writes) settle(o *op, inodes ...uint64) (give func(), status nfsStatus) {
	var held []*writer
	give = func() {
		for _, w := range held {
			t.give(w)
		}
	}
	for _, inode := range slices.Compact(slices.Sorted(slices.Values(inodes))) {
		w, ok := t.take(inode, o.cancel)
		if !ok {
			give()
			return nil, nfsErrJukebox
		}
		done := make(chan nfsStatus, 1)
		go func() { done <- t.release(w) }()
		select {
		case status = <-done:
		case <-o.cancel:
			go func() {
				<-done
				t.give(w)
			}()
			give()
			return nil, nfsErrJukebox
		}
		held = append(held, w)
		if status != nfsOK {
			give()
			return nil, status
		}
	}
	return give, nfsOK
}

// flushAll persists and releases every writer, as a clean stop does,
// returning how many files' accepted writes could not be persisted.
func (t *writes) flushAll() (lost int) {
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
		if t.release(w) != nfsOK {
			t.closeHandle(w, true) // nothing retries once the mount is gone
			w.failed = nfsErrIO
		}
		if w.failed != nfsOK {
			lost++
		}
		<-w.slot
	}
	return lost
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
	w.cred = o.cred
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
// A failure is reported once; a flush to retry later is retried by the
// call's retry.
func (t *writes) commit(o *op, w *writer) nfsStatus {
	done := make(chan nfsStatus, 1)
	go func(cred credential) { done <- t.flush(w, cred) }(o.cred)
	limit := time.NewTimer(time.Until(o.received.Add(rpcDeadline - time.Second)))
	defer limit.Stop()
	select {
	case status := <-done:
		if status == nfsErrJukebox {
			t.give(w)
			return status
		}
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
