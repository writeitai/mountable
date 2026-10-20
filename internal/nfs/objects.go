package nfs

import (
	"sync"
	"syscall"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// Engine is the part of the SeaweedFS mount engine (weed/mount.WFS) the
// server uses; WFS implements it as it is.
type Engine interface {
	GetAttr(cancel <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status
	Lookup(cancel <-chan struct{}, header *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status
	Forget(nodeid, nlookup uint64)
	Open(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status
	Read(cancel <-chan struct{}, in *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status)
	Release(cancel <-chan struct{}, in *fuse.ReleaseIn)
	Readlink(cancel <-chan struct{}, header *fuse.InHeader) ([]byte, fuse.Status)
	OpenDir(cancel <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status
	ReadDirectoryInto(in *fuse.ReadIn, sink weedmount.DirEntrySink, plus bool) fuse.Status
	ReleaseDir(in *fuse.ReleaseIn)
	StatFs(cancel <-chan struct{}, header *fuse.InHeader, out *fuse.StatfsOut) fuse.Status
	PathForInode(inode uint64) (util.FullPath, bool)
	MountRoot() util.FullPath
}

var _ Engine = (*weedmount.WFS)(nil)

// rootInode is WFS's inode for the export's root, whose identity never
// changes.
const (
	rootInode       = 1
	rootIncarnation = 1
)

// identity is one object the server has issued handles for.
type identity struct {
	incarnation uint64
	// ino and kind are what the object itself says it is: the inode the
	// store keeps with the entry (GetAttr reports it) and its file type.
	// WFS reuses its own inode for whatever later takes a vanished
	// object's path, so these are what tell the two apart.
	ino  uint64
	kind uint32
	gone bool
}

// objects holds every identity handed out, and for each WFS inode one
// lookup reference, until the export ends: nothing ever forgets an inode a
// live handle names. Memory grows with the objects clients have looked at,
// not with the filesystem.
type objects struct {
	engine  Engine
	mu      sync.Mutex
	byInode map[uint64]*identity
	last    uint64 // the last incarnation handed out
}

func newObjects(engine Engine) *objects {
	return &objects{
		engine:  engine,
		byInode: map[uint64]*identity{rootInode: {incarnation: rootIncarnation, ino: rootInode, kind: syscall.S_IFDIR}},
		last:    rootIncarnation,
	}
}

// issue returns the incarnation to put in a handle for inode, which a WFS
// lookup has just returned holding one more reference; attr are its
// current attributes. The first reference to an inode is kept, later ones
// are given back. Another object at an inode once issued gets a new
// incarnation, so handles to the old one go stale.
func (o *objects) issue(inode uint64, attr *fuse.Attr) uint64 {
	o.mu.Lock()
	id, held := o.byInode[inode]
	if !held || id.gone || id.ino != attr.Ino || id.kind != attr.Mode&syscall.S_IFMT {
		o.last++
		id = &identity{incarnation: o.last, ino: attr.Ino, kind: attr.Mode & syscall.S_IFMT}
		o.byInode[inode] = id
	}
	incarnation := id.incarnation
	o.mu.Unlock()
	if held && inode != rootInode {
		o.engine.Forget(inode, 1)
	}
	return incarnation
}

// current returns the identity a handle names while it is still the one
// issued at its inode.
func (o *objects) current(inode, incarnation uint64) (identity, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	id, ok := o.byInode[inode]
	if !ok || id.gone || id.incarnation != incarnation {
		return identity{}, false
	}
	return *id, true
}

// retire marks an identity as gone for good: its object no longer exists,
// or something else now occupies its inode.
func (o *objects) retire(inode, incarnation uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if id, ok := o.byInode[inode]; ok && id.incarnation == incarnation && inode != rootInode {
		id.gone = true
	}
}
