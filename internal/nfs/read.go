package nfs

import (
	"math"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// Transfer sizes: what FSINFO advertises and the mount options ask for.
const (
	maxTransfer = 1 << 20  // READ and WRITE
	dirPref     = 64 << 10 // READDIR
	// maxFileSize is the gateway's limit on one file: 9,999 chunks of at
	// most 2 MiB.
	maxFileSize = 9999 * (2 << 20)
	nameMax     = 255
)

// object is a handle resolved to what it names, with current attributes
// whose Ino is the WFS inode, the fileid clients see.
type object struct {
	inode, incarnation uint64
	attr               fuse.Attr
}

// resolve checks a handle and reads its object's attributes. A handle whose
// object is gone, or whose inode something else now occupies, is STALE: it
// never names whatever later took its object's path.
func (s *service) resolve(o *op, fh []byte) (object, nfsStatus) {
	inode, incarnation, status := s.handles.parse(fh)
	if status != nfsOK {
		return object{}, status
	}
	attr, status := s.verified(o, inode, incarnation)
	if status != nfsOK {
		return object{}, status
	}
	return object{inode: inode, incarnation: incarnation, attr: attr}, nfsOK
}

// verified reads the attributes of the object at inode, checking that it
// is still the identity issued: the same stored inode and type, and, for
// anything but a directory, still in the namespace. A stateless NFS
// handle has no open-unlink semantics: an object only an open WFS handle
// still holds (nlink 0) is gone for it, and the identity is retired.
func (s *service) verified(o *op, inode, incarnation uint64) (fuse.Attr, nfsStatus) {
	id, ok := s.objects.current(inode, incarnation)
	if !ok {
		return fuse.Attr{}, nfsErrStale
	}
	var out fuse.AttrOut
	st := s.engine.GetAttr(o.cancel, &fuse.GetAttrIn{InHeader: header(inode, o.cred)}, &out)
	switch {
	case st == fuse.ENOENT || st == fuse.OK && (out.Ino != id.ino || out.Mode&syscall.S_IFMT != id.kind ||
		id.kind != syscall.S_IFDIR && out.Nlink == 0):
		s.objects.retire(inode, incarnation)
		return fuse.Attr{}, nfsErrStale
	case st != fuse.OK:
		return fuse.Attr{}, statusOf(st)
	}
	out.Ino = inode
	return out.Attr, nfsOK
}

// found turns an inode a WFS lookup just returned, holding one more
// reference, into an object with a handle.
func (s *service) found(o *op, inode uint64) (object, nfsStatus) {
	var out fuse.AttrOut
	if st := s.engine.GetAttr(o.cancel, &fuse.GetAttrIn{InHeader: header(inode, o.cred)}, &out); st != fuse.OK {
		if st == fuse.ENOENT {
			// Seen gone: whatever takes the name next is another object.
			s.objects.retire(inode, 0)
		}
		s.engine.Forget(inode, 1)
		return object{}, statusOf(st)
	}
	incarnation := s.objects.issue(inode, &out.Attr)
	out.Ino = inode
	return object{inode: inode, incarnation: incarnation, attr: out.Attr}, nfsOK
}

// fh is the handle of an object.
func (s *service) fh(obj object) []byte { return s.handles.make(obj.inode, obj.incarnation) }

func (s *service) getAttr(o *op) reply {
	obj, fail, ok := s.resolveOnly(o)
	if !ok {
		return fail
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putAttr(e, &obj.attr, s.fsid)
	return reply{body: e.buf}
}

func (s *service) lookup(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	name := d.string(mountPathLen)
	if !d.done() {
		return garbage()
	}
	dir, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	fail := func(status nfsStatus) reply {
		e := &encoder{}
		e.uint32(uint32(status))
		putPostOpAttr(e, &dir.attr, s.fsid)
		return reply{body: e.buf}
	}
	if !isDir(&dir.attr) {
		return fail(nfsErrNotDir)
	}
	if !allowed(o.cred, &dir.attr, permExecute) {
		return fail(nfsErrAccess)
	}
	var child object
	switch {
	case name == ".":
		child = dir
	case name == "..":
		child, status = s.parent(o, dir)
	case len(name) > nameMax:
		status = nfsErrNameLong
	case name == "" || strings.ContainsAny(name, "/\x00") || !utf8.ValidString(name):
		status = nfsErrNoEnt // no such name can be stored
	default:
		child, status = s.child(o, dir.inode, name)
	}
	if status == nfsOK {
		// The lookup went by the directory's path: answer only if that
		// is still this directory.
		status = s.still(o, dir)
	}
	if status == nfsErrStale {
		return s.failure(o.call, status)
	}
	if status != nfsOK {
		return fail(status)
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	e.opaque(s.fh(child))
	putPostOpAttr(e, &child.attr, s.fsid)
	putPostOpAttr(e, &dir.attr, s.fsid)
	return reply{body: e.buf}
}

// child looks a name up in the directory at inode. When the name is gone,
// the object it last named is checked in the namespace: still there under
// another name (a rename, a surviving hard link), it keeps its handles;
// gone, it is retired, so whatever takes the name next is a new object even
// with the same stored inode.
func (s *service) child(o *op, inode uint64, n string) (object, nfsStatus) {
	key := name{dir: inode, name: n}
	var out fuse.EntryOut
	st := s.engine.Lookup(o.cancel, ptr(header(inode, o.cred)), n, &out)
	if st == fuse.ENOENT {
		// The name is forgotten only once the check was conclusive (after
		// an error, the next miss checks again), and only as it was bound
		// when checked.
		if last, ok := s.objects.lastNamed(key); ok && s.checkExists(last.inode) {
			s.objects.unnamed(key, last)
		}
	}
	if st != fuse.OK {
		return object{}, statusOf(st)
	}
	obj, status := s.found(o, out.NodeId)
	if status == nfsOK {
		s.rebind(key, obj.inode)
	}
	return obj, status
}

// rebind records that key names inode now. An object the name led to
// before was overwritten or moved away: it keeps its handles only if the
// namespace still has it.
func (s *service) rebind(key name, inode uint64) {
	if previous, rebound := s.objects.named(key, inode); rebound {
		s.checkExists(previous)
	}
}

// parent finds a directory's parent by walking its current path from the
// root; the root is its own parent.
func (s *service) parent(o *op, dir object) (object, nfsStatus) {
	root, status := s.root(o)
	if status != nfsOK || dir.inode == rootInode {
		return root, status
	}
	path, ok := s.engine.PathForInode(dir.inode)
	if !ok {
		return object{}, nfsErrStale
	}
	rel, ok := strings.CutPrefix(string(path), strings.TrimSuffix(string(s.engine.MountRoot()), "/")+"/")
	if !ok {
		return object{}, nfsErrStale
	}
	names := strings.Split(rel, "/")
	current := root
	for _, name := range names[:len(names)-1] {
		if current, status = s.child(o, current.inode, name); status != nfsOK {
			return object{}, status
		}
	}
	return current, nfsOK
}

func (s *service) root(o *op) (object, nfsStatus) {
	return s.resolve(o, s.fh(object{inode: rootInode, incarnation: rootIncarnation}))
}

func (s *service) access(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	asked := d.uint32()
	if !d.done() {
		return garbage()
	}
	obj, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putPostOpAttr(e, &obj.attr, s.fsid)
	e.uint32(granted(o.cred, &obj.attr, asked))
	return reply{body: e.buf}
}

func (s *service) readlink(o *op) reply {
	obj, fail, ok := s.resolveOnly(o)
	if !ok {
		return fail
	}
	var target []byte
	status := nfsErrInval
	if obj.attr.Mode&syscall.S_IFMT == syscall.S_IFLNK {
		var st fuse.Status
		target, st = s.engine.Readlink(o.cancel, ptr(header(obj.inode, o.cred)))
		if st == fuse.ENOENT {
			// Gone since it was resolved: the handle is stale, and whatever
			// takes the name next is another link.
			s.objects.retire(obj.inode, obj.incarnation)
			return s.failure(o.call, nfsErrStale)
		}
		if status = statusOf(st); status == nfsOK {
			// Readlink reads by path: the target is this link's only if
			// the link is still there.
			status = s.still(o, obj)
		}
	}
	if status == nfsErrStale {
		return s.failure(o.call, status)
	}
	e := &encoder{}
	e.uint32(uint32(status))
	putPostOpAttr(e, &obj.attr, s.fsid)
	if status == nfsOK {
		e.opaque(target)
	}
	return reply{body: e.buf}
}

func (s *service) read(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	offset, count := d.uint64(), d.uint32()
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
	case !mayRead(o.cred, &obj.attr):
		status = nfsErrAccess
	case offset > math.MaxInt64:
		status = nfsErrInval
	}
	var data []byte
	if status == nfsOK && offset < obj.attr.Size {
		count = min(count, maxTransfer)
		data, status = s.readData(o, obj, offset, count)
	}
	if status == nfsErrStale {
		return s.failure(o.call, status)
	}
	e := &encoder{}
	e.uint32(uint32(status))
	putPostOpAttr(e, &obj.attr, s.fsid)
	if status == nfsOK {
		e.uint32(uint32(len(data)))
		// Nothing read where data was asked for is the end, whatever the
		// size said a moment ago.
		e.bool(offset+uint64(len(data)) >= obj.attr.Size || count > 0 && len(data) == 0)
		e.opaque(data)
	}
	return reply{body: e.buf}
}

// readData reads through a WFS handle opened for this one READ. Open loads
// the file by its path, which another object may have taken since the
// handle was resolved, so the object is checked again through the opened
// handle, and read bound to it: the bytes are returned only if they are
// the handle's object's, and the read is STALE otherwise.
func (s *service) readData(o *op, obj object, offset uint64, count uint32) ([]byte, nfsStatus) {
	h := header(obj.inode, o.cred)
	var opened fuse.OpenOut
	if st := s.engine.Open(o.cancel, &fuse.OpenIn{InHeader: h, Flags: syscall.O_RDONLY}, &opened); st != fuse.OK {
		if st == fuse.ENOENT {
			s.objects.retire(obj.inode, obj.incarnation) // gone since resolve
			return nil, nfsErrStale
		}
		return nil, statusOf(st)
	}
	defer s.engine.Release(o.cancel, &fuse.ReleaseIn{InHeader: h, Fh: opened.Fh, Flags: syscall.O_RDONLY})
	if status := s.still(o, obj); status != nfsOK {
		return nil, status
	}
	id, ok := s.objects.current(obj.inode, obj.incarnation)
	if !ok {
		return nil, nfsErrStale
	}
	buf := make([]byte, count)
	// ReadAs checks the opened handle's object and reads it under one lock:
	// the bytes are this object's, whatever was swapped in and out between.
	result, st := s.engine.ReadAs(o.cancel, &fuse.ReadIn{InHeader: h, Fh: opened.Fh, Offset: offset, Size: count}, buf, id.ino)
	if st == fuse.Status(syscall.ESTALE) {
		s.objects.retire(obj.inode, obj.incarnation)
		return nil, nfsErrStale
	}
	if st != fuse.OK {
		return nil, statusOf(st)
	}
	defer result.Done()
	data, st := result.Bytes(buf)
	if st != fuse.OK {
		return nil, statusOf(st)
	}
	return data, nfsOK
}

// checkExists retires the identity an inode holds if the namespace no
// longer has its object, and reports whether it could tell. It asks the
// namespace itself, not GetAttr, which answers from a handle that holds the
// file open, even after the file was deleted. An error proves nothing, and
// retires nothing.
func (s *service) checkExists(inode uint64) (conclusive bool) {
	incarnation, ok := s.objects.at(inode)
	if !ok {
		return true // retired already
	}
	id, _ := s.objects.current(inode, incarnation)
	entry, st := s.engine.NamespaceEntry(inode)
	if st == fuse.OK {
		stored := entry.GetAttributes().GetInode()
		if stored == 0 {
			stored = inode // as GetAttr reports an entry without one
		}
		if stored == id.ino && kindOf(entry) == id.kind {
			return true
		}
	}
	if st != fuse.OK && st != fuse.ENOENT {
		return false
	}
	s.objects.retire(inode, incarnation)
	return true
}

// still checks that the inode still holds obj's identity, as resolve
// does, retiring it if not.
func (s *service) still(o *op, obj object) nfsStatus {
	_, status := s.verified(o, obj.inode, obj.incarnation)
	return status
}

// kindOf is an entry's file type as S_IFMT bits.
func kindOf(entry *filer_pb.Entry) uint32 {
	mode := os.FileMode(entry.GetAttributes().GetFileMode())
	switch {
	case entry.GetIsDirectory() || mode.IsDir():
		return syscall.S_IFDIR
	case mode&os.ModeSymlink != 0:
		return syscall.S_IFLNK
	case mode&os.ModeNamedPipe != 0:
		return syscall.S_IFIFO
	case mode&os.ModeSocket != 0:
		return syscall.S_IFSOCK
	case mode&os.ModeCharDevice != 0:
		return syscall.S_IFCHR
	case mode&os.ModeDevice != 0:
		return syscall.S_IFBLK
	}
	return syscall.S_IFREG
}

func ptr(h fuse.InHeader) *fuse.InHeader { return &h }

func (s *service) fsStat(o *op) reply {
	obj, fail, ok := s.resolveOnly(o)
	if !ok {
		return fail
	}
	var out fuse.StatfsOut
	h := header(obj.inode, o.cred)
	st := s.engine.StatFs(o.cancel, &h, &out)
	e := &encoder{}
	e.uint32(uint32(statusOf(st)))
	putPostOpAttr(e, &obj.attr, s.fsid)
	if st == fuse.OK {
		bsize := uint64(out.Bsize)
		e.uint64(out.Blocks * bsize)
		e.uint64(out.Bfree * bsize)
		e.uint64(out.Bavail * bsize)
		e.uint64(out.Files)
		e.uint64(out.Ffree)
		e.uint64(out.Ffree)
		e.uint32(0) // invarsec: it may change at any time
	}
	return reply{body: e.buf}
}

func (s *service) fsInfo(o *op) reply {
	obj, fail, ok := s.resolveOnly(o)
	if !ok {
		return fail
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putPostOpAttr(e, &obj.attr, s.fsid)
	e.uint32(maxTransfer) // rtmax
	e.uint32(maxTransfer) // rtpref
	e.uint32(4096)        // rtmult
	e.uint32(maxTransfer) // wtmax
	e.uint32(maxTransfer) // wtpref
	e.uint32(4096)        // wtmult
	e.uint32(dirPref)     // dtpref
	e.uint64(maxFileSize)
	e.uint32(0) // time_delta: nanoseconds
	e.uint32(1)
	e.uint32(fsfLink | fsfSymlink | fsfHomogeneous | fsfCanSetTime)
	return reply{body: e.buf}
}

func (s *service) pathConf(o *op) reply {
	obj, fail, ok := s.resolveOnly(o)
	if !ok {
		return fail
	}
	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putPostOpAttr(e, &obj.attr, s.fsid)
	e.uint32(math.MaxInt32) // linkmax: the store counts links in an int32
	e.uint32(nameMax)
	e.bool(true)  // no_trunc
	e.bool(true)  // chown_restricted
	e.bool(false) // case_insensitive
	e.bool(true)  // case_preserving
	return reply{body: e.buf}
}

// resolveOnly decodes and resolves the handle that is a procedure's only
// argument.
func (s *service) resolveOnly(o *op) (object, reply, bool) {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	if !d.done() {
		return object{}, garbage(), false
	}
	obj, status := s.resolve(o, fh)
	if status != nfsOK {
		return object{}, s.failure(o.call, status), false
	}
	return obj, reply{}, true
}
