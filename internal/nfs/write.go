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

// WRITE and COMMIT (design §4, "Writes"; D45). WRITE is write-through: it
// is persisted through one shared WFS write handle per inode, data and
// entry, and only then answered FILE_SYNC with this process's write
// verifier. A failure is that WRITE's own, because macOS's client never
// passes on a failed COMMIT, and it leaves nothing in the handle. WRITEs of
// a file go one at a time under its writer. The handle holds the file's
// -dlm writer lock and is released once unused for writeIdle. COMMIT has
// nothing left to flush and answers with the verifier.

// stable_how (RFC 1813 §3.3.7).
const (
	stableUnstable = 0
	stableDataSync = 1
	stableFileSync = 2
)

// writeIdle is how long an unused write handle, and with it the file's
// lease, is kept for the next WRITE.
var writeIdle = 2 * time.Second

// writer is one inode's shared write handle.
type writer struct {
	inode uint64
	// slot is held while a call uses the writer.
	slot chan struct{}
	fh   uint64 // the WFS handle; 0 when none is open
	// timer releases the handle once unused; gen tells the one last set.
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

// none reports whether no writer is held: nothing is being written.
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

// give lets the writer go, timing its release while it has a handle, and
// dropping it when it has none.
func (t *writes) give(w *writer) {
	if w.fh != 0 {
		t.schedule(w, writeIdle)
	} else {
		t.mu.Lock()
		if t.byInode[w.inode] == w {
			delete(t.byInode, w.inode)
		}
		t.mu.Unlock()
	}
	<-w.slot
}

// schedule releases the writer's handle after d, unless the export ended.
// Timers are set under t.mu, which end stops them under.
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

// idle releases the handle of a writer nothing used for writeIdle, which
// lets its lease go. A writer replaced, rescheduled or ended while it
// waited is left alone.
func (t *writes) idle(w *writer, gen uint64) {
	w.slot <- struct{}{}
	t.mu.Lock()
	current := t.byInode[w.inode] == w && w.gen == gen && !t.ended
	t.mu.Unlock()
	if !current {
		<-w.slot
		return
	}
	t.closeHandle(w)
	t.give(w)
}

// end stops every release timer: an export that ended changes nothing
// more.
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

// closeHandle closes the writer's handle, letting its lease go. Nothing
// is left to flush: every WRITE was persisted before its reply.
func (t *writes) closeHandle(w *writer) {
	if w.fh == 0 {
		return
	}
	t.s.engine.NFSCloseWrite(weedmount.FileHandleId(w.fh), false)
	w.fh = 0
}

// settle takes the writers of the inodes a mutation changes, in inode
// order, waiting for any WRITE in progress, and releases their handles, as
// its preparation does before it changes them (design §4, "Step 1"). It
// holds them until the returned give: a WRITE then waits for the mutation
// instead of taking the lease back from it.
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
		t.closeHandle(w)
		held = append(held, w)
	}
	return give, nfsOK
}

// closeAll releases every writer's handle, as a clean stop does.
func (t *writes) closeAll() {
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
		t.closeHandle(w)
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
	written, status := s.writes.persist(o, w, offset, data)
	if status != nfsOK {
		// Nothing of it is left in the handle; its lease goes too.
		s.writes.closeHandle(w)
	}
	s.writes.give(w)
	if status != nfsOK {
		return fail(status)
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	s.putWcc(e, &obj.attr, s.now(o, obj))
	e.uint32(written)
	e.uint32(stableFileSync)
	e.buf = append(e.buf, s.writes.verifier[:]...)
	return reply{body: e.buf}
}

// persist writes data at offset through the writer's handle, opened first
// if it has none with the file's lease taken within the call's
// preparation, and persists it, data and entry, before it returns. Until
// the entry write is sent the call may still answer "retry later"; after
// that, its own status, or IO at the deadline.
func (t *writes) persist(o *op, w *writer, offset uint64, data []byte) (uint32, nfsStatus) {
	step, done := t.s.step(o)
	defer done()
	if w.fh == 0 {
		fh, st := t.s.engine.NFSOpenForWrite(step.Prepare, w.inode, o.cred.uid, o.cred.gid)
		if st != fuse.OK {
			return 0, resultOf(st)
		}
		w.fh = uint64(fh)
	}
	if len(data) == 0 {
		return 0, nfsOK // nothing to write: the file stays as it is
	}
	written, st := t.s.engine.NFSWrite(step, weedmount.FileHandleId(w.fh), offset, data, o.cred.uid, o.cred.gid)
	return written, resultOf(st)
}

func (s *service) commit(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	d.uint64() // offset and count: every WRITE was persisted already
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
	}
	e := &encoder{}
	e.uint32(uint32(status))
	s.putWcc(e, &obj.attr, s.now(o, obj))
	if status == nfsOK {
		e.buf = append(e.buf, s.writes.verifier[:]...)
	}
	return reply{body: e.buf}
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
