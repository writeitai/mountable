package nfs

import (
	"math"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/seaweedfs/go-fuse/v2/fuse"
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
	id, ok := s.objects.current(inode, incarnation)
	if !ok {
		return object{}, nfsErrStale
	}
	var out fuse.AttrOut
	st := s.engine.GetAttr(o.cancel, &fuse.GetAttrIn{InHeader: header(inode, o.cred)}, &out)
	if st == fuse.ENOENT || st == fuse.OK && (out.Ino != id.ino || out.Mode&syscall.S_IFMT != id.kind) {
		s.objects.retire(inode, incarnation)
		return object{}, nfsErrStale
	}
	if st != fuse.OK {
		return object{}, statusOf(st)
	}
	out.Ino = inode
	return object{inode: inode, incarnation: incarnation, attr: out.Attr}, nfsOK
}

// found turns an inode a WFS lookup just returned, holding one more
// reference, into an object with a handle.
func (s *service) found(o *op, inode uint64) (object, nfsStatus) {
	var out fuse.AttrOut
	if st := s.engine.GetAttr(o.cancel, &fuse.GetAttrIn{InHeader: header(inode, o.cred)}, &out); st != fuse.OK {
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
	if d.err != nil {
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

// child looks name up in the directory at inode.
func (s *service) child(o *op, inode uint64, name string) (object, nfsStatus) {
	var out fuse.EntryOut
	if st := s.engine.Lookup(o.cancel, ptr(header(inode, o.cred)), name, &out); st != fuse.OK {
		return object{}, statusOf(st)
	}
	return s.found(o, out.NodeId)
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
	if d.err != nil {
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
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	if d.err != nil {
		return garbage()
	}
	obj, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	var target []byte
	st := fuse.EINVAL
	if obj.attr.Mode&syscall.S_IFMT == syscall.S_IFLNK {
		target, st = s.engine.Readlink(o.cancel, ptr(header(obj.inode, o.cred)))
	}
	e := &encoder{}
	e.uint32(uint32(statusOf(st)))
	putPostOpAttr(e, &obj.attr, s.fsid)
	if st == fuse.OK {
		e.opaque(target)
	}
	return reply{body: e.buf}
}

func (s *service) read(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	offset, count := d.uint64(), d.uint32()
	if d.err != nil {
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
		data, status = s.readData(o, obj.inode, offset, min(count, maxTransfer))
	}
	e := &encoder{}
	e.uint32(uint32(status))
	putPostOpAttr(e, &obj.attr, s.fsid)
	if status == nfsOK {
		e.uint32(uint32(len(data)))
		e.bool(offset+uint64(len(data)) >= obj.attr.Size)
		e.opaque(data)
	}
	return reply{body: e.buf}
}

// readData reads through a WFS handle opened for this one READ.
func (s *service) readData(o *op, inode, offset uint64, count uint32) ([]byte, nfsStatus) {
	h := header(inode, o.cred)
	var opened fuse.OpenOut
	if st := s.engine.Open(o.cancel, &fuse.OpenIn{InHeader: h, Flags: syscall.O_RDONLY}, &opened); st != fuse.OK {
		return nil, statusOf(st)
	}
	defer s.engine.Release(o.cancel, &fuse.ReleaseIn{InHeader: h, Fh: opened.Fh, Flags: syscall.O_RDONLY})
	buf := make([]byte, count)
	result, st := s.engine.Read(o.cancel, &fuse.ReadIn{InHeader: h, Fh: opened.Fh, Offset: offset, Size: count}, buf)
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
	if d.err != nil {
		return object{}, garbage(), false
	}
	obj, status := s.resolve(o, fh)
	if status != nfsOK {
		return object{}, s.failure(o.call, status), false
	}
	return obj, reply{}, true
}

func ptr(h fuse.InHeader) *fuse.InHeader { return &h }
