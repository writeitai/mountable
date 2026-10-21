package nfs

import (
	"os"
	"path"
	"sort"
	"sync"
	"syscall"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// fakeEngine stands in for WFS the way the server sees it: a store of
// objects by path, each with the inode the store keeps for it, and WFS's
// inode table, which hands out an inode per path, counts lookup references,
// and keeps a path's inode when the object there is replaced.
type fakeEngine struct {
	mu       sync.Mutex
	root     string
	objects  map[string]*fakeObject // by path
	paths    map[uint64]string      // the inode table
	inodes   map[string]uint64
	nlookup  map[uint64]uint64
	nextFh   uint64
	listings int // filer listing calls
	// block, when set, holds GetAttr until it is closed.
	block chan struct{}
	// beforeOpen and beforeRead, when set, run just before Open and Read
	// do their work, unlocked: they stand for another mount changing
	// things at that moment.
	beforeOpen, beforeRead func()
	// afterLookup, when set, runs once a Lookup has succeeded;
	// beforeReadlink and beforeList before Readlink and a listing.
	afterLookup, beforeReadlink, beforeList func()
	// namespaceFails, while set, makes NamespaceEntry answer EIO;
	// namespaceGate, while set, holds it until closed.
	namespaceFails bool
	namespaceGate  chan struct{}
	getAttrs       int // GetAttr calls begun
	// The write path's: see engine_writes_test.go.
	writeState
}

type fakeObject struct {
	ino    uint64
	mode   uint32
	data   []byte
	target string
	uid    uint32 // the owner, if not the mount's user
	// verifier is an EXCLUSIVE create's, until a SETATTR clears it.
	verifier *[8]byte
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		root:    "/c/fs1",
		objects: map[string]*fakeObject{},
		paths:   map[uint64]string{rootInode: "/c/fs1"},
		inodes:  map[string]uint64{"/c/fs1": rootInode},
		nlookup: map[uint64]uint64{rootInode: 1},
	}
}

// add stores an object at rel, below the root.
func (f *fakeEngine) add(rel string, ino uint64, mode uint32, data string) *fakeObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := &fakeObject{ino: ino, mode: mode, data: []byte(data)}
	if mode&syscall.S_IFMT == syscall.S_IFLNK {
		o.target, o.data = data, nil
	}
	f.objects[f.root+"/"+rel] = o
	return o
}

// truncate empties the object at rel in place.
func (f *fakeEngine) truncate(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[f.root+"/"+rel].data = nil
}

// remove deletes the object at rel, as another mount would: WFS's table
// keeps the path's inode.
func (f *fakeEngine) remove(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, f.root+"/"+rel)
}

// rename moves an object, and WFS's table with it.
func (f *fakeEngine) rename(from, to string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	from, to = f.root+"/"+from, f.root+"/"+to
	f.objects[to] = f.objects[from]
	delete(f.objects, from)
	if inode, ok := f.inodes[from]; ok {
		delete(f.inodes, from)
		f.inodes[to], f.paths[inode] = inode, to
	}
}

func (f *fakeEngine) refs(rel string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nlookup[f.inodes[f.root+"/"+rel]]
}

func (f *fakeEngine) attr(inode uint64, o *fakeObject, out *fuse.Attr) {
	out.Ino = inode
	out.Mode = o.mode
	out.Size = uint64(len(o.data))
	if o.target != "" {
		out.Size = uint64(len(o.target))
	}
	out.Blocks = (out.Size + 511) / 512
	out.Nlink = 1
	out.Uid, out.Gid = 501, 20
	if o.uid != 0 {
		out.Uid, out.Gid = o.uid, 30
	}
	out.Mtime, out.Mtimensec = 1700000000, 123456789
	out.Atime, out.Atimensec = 1700000000, 123456789
	out.Ctime, out.Ctimensec = 1700000001, 5
}

// register is WFS's inode table lookup: the path's inode, or the stored
// one for a path seen for the first time.
func (f *fakeEngine) register(p string, o *fakeObject) uint64 {
	if inode, ok := f.inodes[p]; ok {
		return inode
	}
	f.inodes[p], f.paths[o.ino] = o.ino, p
	return o.ino
}

func (f *fakeEngine) GetAttr(_ <-chan struct{}, in *fuse.GetAttrIn, out *fuse.AttrOut) fuse.Status {
	f.mu.Lock()
	f.getAttrs++
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.NodeId == rootInode {
		out.Ino, out.Mode, out.Nlink, out.Uid, out.Gid = rootInode, syscall.S_IFDIR|0o755, 2, 501, 20
		return fuse.OK
	}
	p, ok := f.paths[in.NodeId]
	if !ok {
		return fuse.ENOENT
	}
	o := f.objects[p]
	if o == nil {
		return fuse.ENOENT
	}
	f.attr(in.NodeId, o, &out.Attr)
	out.Ino = o.ino // GetAttr reports the stored inode
	return fuse.OK
}

func (f *fakeEngine) Lookup(_ <-chan struct{}, h *fuse.InHeader, name string, out *fuse.EntryOut) fuse.Status {
	st := f.lookup(h.NodeId, name, out)
	if st == fuse.OK && f.afterLookup != nil {
		f.afterLookup()
	}
	return st
}

func (f *fakeEngine) lookup(parent uint64, name string, out *fuse.EntryOut) fuse.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir, ok := f.paths[parent]
	if !ok {
		return fuse.ENOENT
	}
	p := dir + "/" + name
	o := f.objects[p]
	if o == nil {
		return fuse.ENOENT
	}
	inode := f.register(p, o)
	f.nlookup[inode]++
	out.NodeId = inode
	f.attr(inode, o, &out.Attr)
	return fuse.OK
}

func (f *fakeEngine) Forget(inode, n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nlookup[inode] < n {
		panic("forget over-decrement")
	}
	f.nlookup[inode] -= n
	if f.nlookup[inode] == 0 {
		delete(f.nlookup, inode)
		delete(f.inodes, f.paths[inode])
		delete(f.paths, inode)
	}
}

func (f *fakeEngine) Open(_ <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	if f.beforeOpen != nil {
		f.beforeOpen()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFh++
	out.Fh = f.nextFh
	return fuse.OK
}

// ReadAs reads the object at the inode's path, refusing (ESTALE) one that
// is not the expected object, as the patched WFS does.
func (f *fakeEngine) ReadAs(_ <-chan struct{}, in *fuse.ReadIn, buf []byte, stored uint64) (fuse.ReadResult, fuse.Status) {
	if f.beforeRead != nil {
		f.beforeRead()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[f.paths[in.NodeId]]
	if o == nil {
		return nil, fuse.EIO
	}
	if o.ino != stored {
		return nil, fuse.Status(syscall.ESTALE)
	}
	n := copy(buf, o.data[min(int(in.Offset), len(o.data)):])
	return fuse.ReadResultData(buf[:n]), fuse.OK
}

func (f *fakeEngine) Release(_ <-chan struct{}, in *fuse.ReleaseIn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeHandles[in.Fh] {
		delete(f.writeHandles, in.Fh)
		f.releases++
	}
}

func (f *fakeEngine) Readlink(_ <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	if f.beforeReadlink != nil {
		f.beforeReadlink()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[f.paths[h.NodeId]]
	if o == nil {
		return nil, fuse.ENOENT
	}
	return []byte(o.target), fuse.OK
}

// ReadDirectoryAfter lists the way the patched WFS does: the children
// after a name, in name order; a plus entry the caller accepts is entered
// and takes a lookup reference.
func (f *fakeEngine) ReadDirectoryAfter(dir uint64, after string, plus bool, fits func(string) bool, add func(*weedmount.ListedEntry) bool) (bool, fuse.Status) {
	if f.beforeList != nil {
		f.beforeList()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listings++
	dirPath, ok := f.paths[dir]
	if !ok {
		return false, fuse.ENOENT
	}
	var names []string
	for p := range f.objects {
		if path.Dir(p) == dirPath && path.Base(p) > after {
			names = append(names, path.Base(p))
		}
	}
	sort.Strings(names)
	for _, name := range names {
		p := dirPath + "/" + name
		o := f.objects[p]
		inode := o.ino
		if known, ok := f.inodes[p]; ok {
			inode = known
		}
		if fits != nil && !fits(name) {
			return false, fuse.OK
		}
		e := &weedmount.ListedEntry{Name: name, Inode: inode, StoredInode: o.ino}
		if plus {
			e.Out = &fuse.EntryOut{NodeId: inode}
			f.attr(inode, o, &e.Out.Attr)
		}
		if !add(e) {
			return false, fuse.OK
		}
		if plus {
			f.nlookup[f.register(p, o)]++
		}
	}
	return true, fuse.OK
}

func (f *fakeEngine) StatFs(_ <-chan struct{}, _ *fuse.InHeader, out *fuse.StatfsOut) fuse.Status {
	out.Blocks, out.Bfree, out.Bavail, out.Bsize = 2048, 1024, 1024, 512
	out.Files, out.Ffree = 1000, 990
	return fuse.OK
}

func (f *fakeEngine) PathForInode(inode uint64) (util.FullPath, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.paths[inode]
	return util.FullPath(p), ok
}

func (f *fakeEngine) MountRoot() util.FullPath { return util.FullPath(f.root) }

// NamespaceEntry answers from the objects by path, as the patched WFS does
// (the fake has no open handles to prefer).
func (f *fakeEngine) NamespaceEntry(inode uint64) (*filer_pb.Entry, fuse.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.namespaceFails {
		return nil, fuse.EIO
	}
	if f.namespaceGate != nil {
		gate := f.namespaceGate
		f.mu.Unlock()
		<-gate
		f.mu.Lock()
	}
	p, ok := f.paths[inode]
	if !ok {
		return nil, fuse.ENOENT
	}
	o := f.objects[p]
	if o == nil {
		return nil, fuse.ENOENT
	}
	mode := os.FileMode(o.mode & 0o777)
	switch o.mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	}
	return &filer_pb.Entry{IsDirectory: mode.IsDir(), Attributes: &filer_pb.FuseAttributes{Inode: o.ino, FileMode: uint32(mode)}}, fuse.OK
}
