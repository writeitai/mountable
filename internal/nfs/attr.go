package nfs

import (
	"math"
	"slices"
	"syscall"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

// fattrSize is the encoded size of fattr3.
const fattrSize = 84

// putAttr writes fattr3 for attributes WFS reported. fileid is the WFS inode.
func putAttr(e *encoder, a *fuse.Attr, fsid uint64) {
	e.uint32(fileType(a.Mode))
	e.uint32(a.Mode & 0o7777)
	e.uint32(a.Nlink)
	e.uint32(a.Uid)
	e.uint32(a.Gid)
	e.uint64(a.Size)
	e.uint64(a.Blocks * 512) // WFS counts 512-byte blocks
	e.uint32(0)              // rdev: no device files are served
	e.uint32(0)
	e.uint64(fsid)
	e.uint64(a.Ino)
	putTime(e, a.Atime, a.Atimensec)
	putTime(e, a.Mtime, a.Mtimensec)
	putTime(e, a.Ctime, a.Ctimensec)
}

// putPostOpAttr writes post_op_attr: the attributes, or none.
func putPostOpAttr(e *encoder, a *fuse.Attr, fsid uint64) {
	if a == nil {
		e.bool(false)
		return
	}
	e.bool(true)
	putAttr(e, a, fsid)
}

// putTime writes nfstime3. WFS carries signed seconds in an unsigned field;
// times before 1970 or after 2106 are clamped to what nfstime3 can hold.
func putTime(e *encoder, sec uint64, nsec uint32) {
	switch {
	case int64(sec) < 0:
		sec, nsec = 0, 0
	case sec > math.MaxUint32:
		sec, nsec = math.MaxUint32, 999999999
	}
	e.uint32(uint32(sec))
	e.uint32(nsec)
}

func fileType(mode uint32) uint32 {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		return typeDir
	case syscall.S_IFLNK:
		return typeLnk
	case syscall.S_IFBLK:
		return typeBlk
	case syscall.S_IFCHR:
		return typeChr
	case syscall.S_IFSOCK:
		return typeSock
	case syscall.S_IFIFO:
		return typeFifo
	}
	return typeReg
}

func isDir(a *fuse.Attr) bool { return a.Mode&syscall.S_IFMT == syscall.S_IFDIR }

// Permission bits, as in a mode's owner, group or other triple.
const (
	permRead    = 4
	permWrite   = 2
	permExecute = 1
)

// allowed reports whether cred may access an object with attributes a in
// every way want asks (permRead, permWrite, permExecute), by the POSIX
// rules: root may read and write anything, and execute what anyone may.
func allowed(cred credential, a *fuse.Attr, want uint32) bool {
	if cred.uid == 0 {
		return want&permExecute == 0 || isDir(a) || a.Mode&0o111 != 0
	}
	var bits uint32
	switch {
	case cred.uid == a.Uid:
		bits = a.Mode >> 6 & 7
	case cred.gid == a.Gid || slices.Contains(cred.gids, a.Gid):
		bits = a.Mode >> 3 & 7
	default:
		bits = a.Mode & 7
	}
	return bits&want == want
}

// granted is ACCESS's answer: of the rights asked for, those cred has on an
// object with attributes a. The export is read-only, so modifying,
// extending and deleting are never granted.
func granted(cred credential, a *fuse.Attr, asked uint32) uint32 {
	var g uint32
	if allowed(cred, a, permRead) {
		g |= accessRead
	}
	if allowed(cred, a, permExecute) {
		if isDir(a) {
			g |= accessLookup
		} else {
			g |= accessExecute
		}
	}
	return g & asked
}

// mayRead is READ's own check, with RFC 1813 §4.4's owner exception: the
// owner reads whatever the mode bits say, because the client checked
// permission when the file was opened.
func mayRead(cred credential, a *fuse.Attr) bool {
	return cred.uid == a.Uid || allowed(cred, a, permRead)
}

// statusOf maps the engine's result to nfsstat3, name by name; anything
// without a precise meaning is an I/O error.
func statusOf(st fuse.Status) nfsStatus {
	switch st {
	case fuse.OK:
		return nfsOK
	case fuse.ENOENT:
		return nfsErrNoEnt
	case fuse.EACCES:
		return nfsErrAccess
	case fuse.EPERM:
		return nfsErrPerm
	case fuse.EROFS:
		return nfsErrROFS
	case fuse.Status(syscall.ENOSPC):
		return nfsErrNoSpc
	case fuse.Status(syscall.EDQUOT):
		return nfsErrDQuot
	case fuse.ENOTDIR:
		return nfsErrNotDir
	case fuse.EISDIR:
		return nfsErrIsDir
	case fuse.Status(syscall.EEXIST):
		return nfsErrExist
	case fuse.Status(syscall.ENOTEMPTY):
		return nfsErrNotEmpty
	case fuse.Status(syscall.ENAMETOOLONG):
		return nfsErrNameLong
	case fuse.EINVAL:
		return nfsErrInval
	case fuse.Status(syscall.EFBIG):
		return nfsErrFBig
	case fuse.EXDEV:
		return nfsErrXDev
	case fuse.Status(syscall.EMLINK):
		return nfsErrMLink
	case fuse.ENOSYS, fuse.ENOTSUP:
		return nfsErrNotSupp
	}
	return nfsErrIO
}
