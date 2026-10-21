package nfs

import (
	"context"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// The namespace and attribute procedures of a read-write export (design
// §4, "Writes", "Creation and attributes" and "Bounded replies"). Each runs
// through an engine helper in two steps: a preparation, cancelled at the
// call's preparation budget (the call then answers "retry later", having
// changed nothing), and a mutation whose calls are sent once, bounded by
// the call's deadline. The adapter checks permissions itself: ACCESS is
// advisory.

// createhow3 (RFC 1813 §3.3.8).
const (
	createUnchecked = 0
	createGuarded   = 1
	createExclusive = 2
)

// time_how (RFC 1813 §2.6).
const (
	timeDontChange   = 0
	timeServer       = 1
	timeClient       = 2
	maxNfsTimeOffset = 1<<32 - 1
)

// step is the engine's view of the call's two steps. done must be called
// once the procedure is finished with it.
func (s *service) step(o *op) (step weedmount.NFSStep, done func()) {
	prepare, cancelPrepare := context.WithDeadline(context.Background(), o.received.Add(prepareBudget))
	mutate, cancelMutate := context.WithDeadline(context.Background(), o.received.Add(rpcDeadline))
	finished := make(chan struct{})
	go func() {
		select {
		case <-o.cancel: // abandoned, timed out, or the export ended
			cancelPrepare()
			cancelMutate()
		case <-finished:
		}
	}()
	return weedmount.NFSStep{Prepare: prepare, Mutate: mutate, Begin: o.beginMutation}, func() {
		close(finished)
		cancelPrepare()
		cancelMutate()
	}
}

// sattr is a decoded sattr3: what a SETATTR, CREATE, MKDIR or SYMLINK
// sets.
type sattr struct {
	mode, uid, gid *uint32
	size           *uint64
	// atimeHow and mtimeHow are time_how; the times are the client's.
	atimeHow, mtimeHow uint32
	atime, mtime       time.Time
}

func decodeSattr(d *decoder) sattr {
	var a sattr
	if d.bool() {
		v := d.uint32()
		a.mode = &v
	}
	if d.bool() {
		v := d.uint32()
		a.uid = &v
	}
	if d.bool() {
		v := d.uint32()
		a.gid = &v
	}
	if d.bool() {
		v := d.uint64()
		a.size = &v
	}
	a.atimeHow, a.atime = decodeSetTime(d)
	a.mtimeHow, a.mtime = decodeSetTime(d)
	return a
}

func decodeSetTime(d *decoder) (uint32, time.Time) {
	how := d.uint32()
	switch how {
	case timeDontChange, timeServer:
		return how, time.Time{}
	case timeClient:
		sec, nsec := d.uint32(), d.uint32()
		if nsec > 999999999 {
			d.err = errGarbage
		}
		return how, time.Unix(int64(sec), int64(nsec))
	}
	d.err = errGarbage
	return how, time.Time{}
}

// engineAttrs is a in the engine's terms, server times resolved to now.
func (a sattr) engineAttrs(now time.Time) weedmount.NFSAttrs {
	e := weedmount.NFSAttrs{Mode: a.mode, Uid: a.uid, Gid: a.gid, Size: a.size}
	resolve := func(how uint32, t time.Time) *time.Time {
		switch how {
		case timeServer:
			return &now
		case timeClient:
			return &t
		}
		return nil
	}
	e.Atime, e.Mtime = resolve(a.atimeHow, a.atime), resolve(a.mtimeHow, a.mtime)
	return e
}

// setsTimes reports whether a sets either time.
func (a sattr) setsTimes() bool { return a.atimeHow != timeDontChange || a.mtimeHow != timeDontChange }

// newName checks a name to be created in a directory.
func newName(name string) nfsStatus {
	switch {
	case len(name) > nameMax:
		return nfsErrNameLong
	case name == "." || name == "..":
		return nfsErrExist
	case name == "" || strings.ContainsAny(name, "/\x00") || !utf8.ValidString(name):
		return nfsErrInval
	}
	return nfsOK
}

// oldName checks the name of an existing object a call removes or moves:
// a single name, as LOOKUP takes it, never a path or a name the store
// would rewrite.
func oldName(name string) nfsStatus {
	switch {
	case len(name) > nameMax:
		return nfsErrNameLong
	case name == "" || strings.ContainsAny(name, "/\x00") || !utf8.ValidString(name):
		return nfsErrNoEnt // no such name can be stored
	}
	return nfsOK
}

// putPreOpAttr writes pre_op_attr: the size and times before a change.
func putPreOpAttr(e *encoder, a *fuse.Attr) {
	if a == nil {
		e.bool(false)
		return
	}
	e.bool(true)
	e.uint64(a.Size)
	putTime(e, a.Mtime, a.Mtimensec)
	putTime(e, a.Ctime, a.Ctimensec)
}

// putWcc writes wcc_data for an object whose attributes were before and,
// read again now, after (nil when it could not be).
func (s *service) putWcc(e *encoder, before *fuse.Attr, after *fuse.Attr) {
	putPreOpAttr(e, before)
	putPostOpAttr(e, after, s.fsid)
}

// now reads an object's attributes again after a change; nil when it is
// gone or cannot be read.
func (s *service) now(o *op, obj object) *fuse.Attr {
	attr, status := s.verified(o, obj.inode, obj.incarnation)
	if status != nfsOK {
		return nil
	}
	return &attr
}

// resultOf is an engine helper's status: "retry later" when it changed
// nothing and may be tried again, a NOT_SYNC guard failure, or the
// mapped error.
func resultOf(st fuse.Status) nfsStatus {
	switch st {
	case weedmount.NFSRetryLater:
		return nfsErrJukebox
	case weedmount.NFSNotSync:
		return nfsErrNotSync
	}
	return statusOf(st)
}

// dirFailure is a namespace procedure's failure carrying the directory's
// wcc_data, or "retry later".
func (s *service) dirFailure(o *op, status nfsStatus, dir object) reply {
	if status == nfsErrJukebox {
		return s.retryLater(o.call)
	}
	e := &encoder{}
	e.uint32(uint32(status))
	s.putWcc(e, &dir.attr, s.now(o, dir))
	if o.proc == procRename {
		e.bool(false) // the other directory's
		e.bool(false)
	}
	return reply{body: e.buf}
}

// writableDir resolves a directory a name is created in or removed from,
// with the caller's permission to change it.
func (s *service) writableDir(o *op, fh []byte) (object, nfsStatus) {
	dir, status := s.resolve(o, fh)
	switch {
	case status != nfsOK:
		return object{}, status
	case !isDir(&dir.attr):
		return dir, nfsErrNotDir
	case !allowed(o.cred, &dir.attr, permWrite|permExecute):
		return dir, nfsErrAccess
	}
	return dir, nfsOK
}

// mayRemove applies the sticky bit: in a directory that has it, only the
// owner of the directory or of the object, or root, removes or renames.
func mayRemove(cred credential, dir, child *fuse.Attr) bool {
	return dir.Mode&syscall.S_ISVTX == 0 || cred.uid == 0 || cred.uid == dir.Uid || cred.uid == child.Uid
}

// createdReply answers a create: the new object's handle and attributes,
// and the directory's wcc_data. A name, once created, names the inode.
func (s *service) createdReply(o *op, dir object, name string, inode uint64, status nfsStatus) reply {
	if status != nfsOK {
		return s.dirFailure(o, status, dir)
	}
	obj, status := s.found(o, inode)
	if status != nfsOK {
		// Created, but gone or unreadable at once: the client looks it up.
		e := &encoder{}
		e.uint32(uint32(nfsOK))
		e.bool(false)
		e.bool(false)
		s.putWcc(e, &dir.attr, s.now(o, dir))
		return reply{body: e.buf}
	}
	s.rebind(nameOf(dir, name), obj.inode)
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	e.bool(true)
	e.opaque(s.fh(obj))
	putPostOpAttr(e, &obj.attr, s.fsid)
	s.putWcc(e, &dir.attr, s.now(o, dir))
	return reply{body: e.buf}
}

func nameOf(dir object, n string) name { return name{dir: dir.inode, name: n} }

func (s *service) create(o *op) reply {
	d := &decoder{buf: o.args}
	fh, n := d.opaque(64), d.string(mountPathLen)
	how := d.uint32()
	var attrs sattr
	var verifier [8]byte
	switch how {
	case createUnchecked, createGuarded:
		attrs = decodeSattr(d)
	case createExclusive:
		copy(verifier[:], d.take(8))
	default:
		d.err = errGarbage
	}
	if !d.done() {
		return garbage()
	}
	dir, status := s.writableDir(o, fh)
	if status == nfsOK {
		status = newName(n)
	}
	if status == nfsOK && attrs.size != nil && *attrs.size > maxFileSize {
		status = nfsErrFBig
	}
	if status == nfsOK && how == createUnchecked {
		status = s.settleExisting(o, dir, n, attrs)
	}
	if status != nfsOK {
		return s.failureWithDir(o, status, dir)
	}
	step, done := s.step(o)
	defer done()
	mode := map[uint32]weedmount.NFSCreateMode{createUnchecked: weedmount.NFSUnchecked, createGuarded: weedmount.NFSGuarded, createExclusive: weedmount.NFSExclusive}[how]
	inode, st := s.engine.NFSCreate(step, dir.inode, n, mode, attrs.engineAttrs(time.Now()), verifier, o.cred.uid, o.cred.gid, reuse(o.cred, attrs))
	if st == fuse.OK && how == createExclusive {
		// Its times hold the verifier until the SETATTR that must follow
		// (RFC 1813 §3.3.8) replaces them.
		s.exclusive.add(inode)
	}
	return s.createdReply(o, dir, n, inode, resultOf(st))
}

// settleExisting persists the accepted writes of a file an UNCHECKED
// CREATE may truncate, before the create's own preparation. Whether it may
// is decided on the entry the engine reads under the file's lease (reuse).
func (s *service) settleExisting(o *op, dir object, n string, attrs sattr) nfsStatus {
	if attrs.size == nil {
		return nfsOK
	}
	if existing, status := s.child(o, dir, n); status == nfsOK {
		return s.writes.settle(o, existing.inode)
	}
	return nfsOK // none known here: the engine finds out
}

// reuse is the check an UNCHECKED CREATE makes, under the lease, on an
// existing file it would open: truncating it takes SETATTR's rights for
// its size.
func reuse(cred credential, attrs sattr) func(*filer_pb.Entry) fuse.Status {
	return func(e *filer_pb.Entry) fuse.Status {
		if attrs.size == nil {
			return fuse.OK
		}
		a := fuse.Attr{Mode: e.Attributes.FileMode & 0o7777, Owner: fuse.Owner{Uid: e.Attributes.Uid, Gid: e.Attributes.Gid}}
		switch mayChange(cred, &a, sattr{size: attrs.size}) {
		case nfsOK:
			return fuse.OK
		case nfsErrPerm:
			return fuse.EPERM
		}
		return fuse.EACCES
	}
}

func (s *service) mkdir(o *op) reply {
	d := &decoder{buf: o.args}
	fh, n := d.opaque(64), d.string(mountPathLen)
	attrs := decodeSattr(d)
	if !d.done() {
		return garbage()
	}
	dir, status := s.writableDir(o, fh)
	if status == nfsOK {
		status = newName(n)
	}
	if status != nfsOK {
		return s.failureWithDir(o, status, dir)
	}
	step, done := s.step(o)
	defer done()
	inode, st := s.engine.NFSMkdir(step, dir.inode, n, attrs.engineAttrs(time.Now()), o.cred.uid, o.cred.gid)
	return s.createdReply(o, dir, n, inode, resultOf(st))
}

func (s *service) symlink(o *op) reply {
	d := &decoder{buf: o.args}
	fh, n := d.opaque(64), d.string(mountPathLen)
	attrs := decodeSattr(d)
	target := d.string(mountPathLen)
	if !d.done() {
		return garbage()
	}
	dir, status := s.writableDir(o, fh)
	if status == nfsOK {
		status = newName(n)
	}
	if status == nfsOK && (target == "" || strings.ContainsRune(target, 0) || !utf8.ValidString(target)) {
		status = nfsErrInval
	}
	if status != nfsOK {
		return s.failureWithDir(o, status, dir)
	}
	step, done := s.step(o)
	defer done()
	inode, st := s.engine.NFSSymlink(step, dir.inode, n, target, attrs.engineAttrs(time.Now()), o.cred.uid, o.cred.gid)
	return s.createdReply(o, dir, n, inode, resultOf(st))
}

// mknod refuses devices, FIFOs and sockets: none are served (design §4,
// "Server limits").
func (s *service) mknod(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	d.string(mountPathLen)
	switch d.uint32() { // mknoddata3
	case typeChr, typeBlk:
		decodeSattr(d)
		d.uint32() // specdata3
		d.uint32()
	case typeSock, typeFifo:
		decodeSattr(d)
	case typeReg, typeDir, typeLnk:
	default:
		d.err = errGarbage
	}
	if !d.done() {
		return garbage()
	}
	dir, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	return s.failureWithDir(o, nfsErrNotSupp, dir)
}

// failureWithDir is dirFailure for a directory that may not have been
// resolved (status STALE or BADHANDLE): no attributes then.
func (s *service) failureWithDir(o *op, status nfsStatus, dir object) reply {
	if status == nfsErrStale || status == nfsErrBadFH || dir.inode == 0 {
		return s.failure(o.call, status)
	}
	e := &encoder{}
	e.uint32(uint32(status))
	s.putWcc(e, &dir.attr, &dir.attr)
	if o.proc == procRename {
		e.bool(false)
		e.bool(false)
	}
	return reply{body: e.buf}
}

func (s *service) remove(o *op) reply { return s.removeName(o, false) }

func (s *service) rmdir(o *op) reply { return s.removeName(o, true) }

// removeName answers REMOVE and RMDIR. The object a removed name led to
// keeps its handles only while the namespace still has it, through
// another hard link.
func (s *service) removeName(o *op, directory bool) reply {
	d := &decoder{buf: o.args}
	fh, n := d.opaque(64), d.string(mountPathLen)
	if !d.done() {
		return garbage()
	}
	dir, status := s.writableDir(o, fh)
	switch {
	case status != nfsOK:
	case n == "." && directory:
		status = nfsErrInval
	case n == ".." && directory:
		status = nfsErrNotEmpty
	case n == "." || n == "..":
		status = nfsErrIsDir
	default:
		status = oldName(n)
	}
	if status != nfsOK {
		return s.failureWithDir(o, status, dir)
	}
	child, status := s.child(o, dir, n)
	if status != nfsOK {
		return s.failureWithDir(o, status, dir)
	}
	switch {
	case !mayRemove(o.cred, &dir.attr, &child.attr):
		status = nfsErrAccess
	case directory && !isDir(&child.attr):
		status = nfsErrNotDir
	case !directory && isDir(&child.attr):
		status = nfsErrIsDir
	}
	if status != nfsOK {
		return s.failureWithDir(o, status, dir)
	}
	if !directory {
		// Its accepted writes are persisted first, and its write handle,
		// with the lease, let go (design §4, "Step 1").
		if status = s.writes.settle(o, child.inode); status != nfsOK {
			return s.dirFailure(o, status, dir)
		}
	}
	step, done := s.step(o)
	defer done()
	var st fuse.Status
	if directory {
		st = s.engine.NFSRmdir(step, dir.inode, n)
	} else {
		st = s.engine.NFSRemove(step, dir.inode, n)
	}
	status = resultOf(st)
	if status == nfsOK || status == nfsErrIO {
		// Gone, or perhaps: the object keeps its handles only if the
		// namespace still has it.
		s.forgetName(nameOf(dir, n))
	}
	if status != nfsOK {
		return s.dirFailure(o, status, dir)
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	s.putWcc(e, &dir.attr, s.now(o, dir))
	return reply{body: e.buf}
}

// forgetName drops what a name was bound to, retiring the object if the
// namespace no longer has it.
func (s *service) forgetName(key name) {
	if last, ok := s.objects.lastNamed(key); ok {
		s.checkExists(last.inode)
		s.objects.unnamed(key, last)
	}
}

func (s *service) rename(o *op) reply {
	d := &decoder{buf: o.args}
	fromFh, fromName := d.opaque(64), d.string(mountPathLen)
	toFh, toName := d.opaque(64), d.string(mountPathLen)
	if !d.done() {
		return garbage()
	}
	from, status := s.writableDir(o, fromFh)
	if status != nfsOK {
		return s.failureWithDir(o, status, from)
	}
	to, status := s.writableDir(o, toFh)
	switch {
	case status != nfsOK:
	case fromName == "." || fromName == ".." || toName == "." || toName == "..":
		status = nfsErrInval // RFC 1813 §3.3.14
	default:
		if status = oldName(fromName); status == nfsOK {
			status = newName(toName)
		}
	}
	if status != nfsOK {
		return s.renameReply(o, status, from, to)
	}
	source, status := s.child(o, from, fromName)
	if status != nfsOK {
		return s.renameReply(o, status, from, to)
	}
	if !mayRemove(o.cred, &from.attr, &source.attr) {
		return s.renameReply(o, nfsErrAccess, from, to)
	}
	target, targetStatus := s.child(o, to, toName)
	switch {
	case targetStatus == nfsOK && target.inode == source.inode:
		// Two names of one file: nothing to do (RFC 1813 §3.3.14).
		return s.renameReply(o, nfsOK, from, to)
	case targetStatus == nfsOK && !mayRemove(o.cred, &to.attr, &target.attr):
		return s.renameReply(o, nfsErrAccess, from, to)
	case targetStatus != nfsOK && targetStatus != nfsErrNoEnt:
		return s.renameReply(o, targetStatus, from, to)
	}
	if status = s.writes.settle(o, source.inode); status == nfsOK && targetStatus == nfsOK {
		status = s.writes.settle(o, target.inode)
	}
	if status != nfsOK {
		return s.renameReply(o, status, from, to)
	}
	step, done := s.step(o)
	defer done()
	status = resultOf(s.engine.NFSRename(step, from.inode, fromName, to.inode, toName))
	if status == nfsOK || status == nfsErrIO {
		s.forgetName(nameOf(to, toName)) // replaced, perhaps
		s.objects.unnamedAny(nameOf(from, fromName))
		if status == nfsOK {
			s.rebind(nameOf(to, toName), source.inode)
		}
	}
	return s.renameReply(o, status, from, to)
}

// renameReply answers RENAME, with both directories' wcc_data.
func (s *service) renameReply(o *op, status nfsStatus, from, to object) reply {
	if status == nfsErrJukebox {
		return s.retryLater(o.call)
	}
	if from.inode == 0 {
		return s.failure(o.call, status)
	}
	e := &encoder{}
	e.uint32(uint32(status))
	s.putWcc(e, &from.attr, s.now(o, from))
	if to.inode == 0 {
		e.bool(false)
		e.bool(false)
	} else {
		s.putWcc(e, &to.attr, s.now(o, to))
	}
	return reply{body: e.buf}
}

func (s *service) link(o *op) reply {
	d := &decoder{buf: o.args}
	fileFh := d.opaque(64)
	dirFh, n := d.opaque(64), d.string(mountPathLen)
	if !d.done() {
		return garbage()
	}
	file, status := s.resolve(o, fileFh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	fail := func(status nfsStatus, dir *object) reply {
		if status == nfsErrJukebox {
			return s.retryLater(o.call)
		}
		e := &encoder{}
		e.uint32(uint32(status))
		putPostOpAttr(e, s.now(o, file), s.fsid)
		if dir == nil {
			e.bool(false)
			e.bool(false)
		} else {
			s.putWcc(e, &dir.attr, s.now(o, *dir))
		}
		return reply{body: e.buf}
	}
	dir, status := s.writableDir(o, dirFh)
	if status != nfsOK {
		if dir.inode == 0 {
			return fail(status, nil)
		}
		return fail(status, &dir)
	}
	switch {
	case isDir(&file.attr):
		status = nfsErrIsDir
	default:
		status = newName(n)
	}
	if status == nfsOK {
		status = s.writes.settle(o, file.inode)
	}
	if status != nfsOK {
		return fail(status, &dir)
	}
	step, done := s.step(o)
	defer done()
	if status = resultOf(s.engine.NFSLink(step, file.inode, dir.inode, n, o.cred.uid, o.cred.gid)); status != nfsOK {
		return fail(status, &dir)
	}
	s.rebind(nameOf(dir, n), file.inode)
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putPostOpAttr(e, s.now(o, file), s.fsid)
	s.putWcc(e, &dir.attr, s.now(o, dir))
	return reply{body: e.buf}
}

func (s *service) setAttr(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	attrs := decodeSattr(d)
	var guard *time.Time
	if d.bool() {
		sec, nsec := d.uint32(), d.uint32()
		if nsec > 999999999 {
			d.err = errGarbage
		}
		t := time.Unix(int64(sec), int64(nsec))
		guard = &t
	}
	if !d.done() {
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
	if status := mayChange(o.cred, &obj.attr, attrs); status != nfsOK {
		return fail(status)
	}
	if attrs.size != nil && !(obj.attr.Mode&syscall.S_IFMT == syscall.S_IFREG) {
		if isDir(&obj.attr) {
			return fail(nfsErrIsDir)
		}
		return fail(nfsErrInval)
	}
	if attrs.size != nil && *attrs.size > maxFileSize {
		return fail(nfsErrFBig)
	}
	if (attrs.uid != nil || attrs.gid != nil) && attrs.mode == nil && o.cred.uid != 0 && obj.attr.Mode&0o6000 != 0 {
		// A change of owner by anyone but root drops the set-id bits,
		// as chown(2) does.
		cleared := obj.attr.Mode & 0o7777 &^ 0o6000
		attrs.mode = &cleared
	}
	// Accepted writes are persisted first: the change applies to the file
	// as written (design §4, "Step 1").
	if status = s.writes.settle(o, obj.inode); status != nfsOK {
		return fail(status)
	}
	clearVerifier := s.exclusive.holds(obj.inode)
	step, done := s.step(o)
	defer done()
	if status = resultOf(s.engine.NFSSetAttr(step, obj.inode, attrs.engineAttrs(time.Now()), guard, clearVerifier)); status != nfsOK {
		return fail(status)
	}
	if clearVerifier {
		s.exclusive.remove(obj.inode)
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	s.putWcc(e, &obj.attr, s.now(o, obj))
	return reply{body: e.buf}
}

// mayChange checks a SETATTR's rights by the POSIX rules: the owner (or
// root) changes the mode and explicit times, and the group to one of its
// own; only root gives a file away; setting the times to now and the size
// need write permission, or, for the size, ownership, as the client
// checked an open descriptor's rights already (RFC 1813 §4.4).
func mayChange(cred credential, a *fuse.Attr, attrs sattr) nfsStatus {
	owner := cred.uid == 0 || cred.uid == a.Uid
	switch {
	case attrs.uid != nil && *attrs.uid != a.Uid && cred.uid != 0:
		return nfsErrPerm
	case attrs.gid != nil && *attrs.gid != a.Gid && cred.uid != 0 && (cred.uid != a.Uid || cred.gid != *attrs.gid && !slices.Contains(cred.gids, *attrs.gid)):
		return nfsErrPerm
	case attrs.mode != nil && !owner:
		return nfsErrPerm
	case (attrs.atimeHow == timeClient || attrs.mtimeHow == timeClient) && !owner:
		return nfsErrPerm
	case attrs.setsTimes() && !owner && !allowed(cred, a, permWrite):
		return nfsErrAccess
	case attrs.size != nil && !owner && !allowed(cred, a, permWrite):
		return nfsErrAccess
	}
	return nfsOK
}
