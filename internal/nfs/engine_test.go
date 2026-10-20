package nfs

import (
	"path"
	"sort"
	"sync"
	"syscall"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// fakeEngine stands in for WFS the way the server sees it: a store of
// objects by path, each with the inode the store keeps for it, and WFS's
// inode table, which hands out an inode per path, counts lookup references,
// and keeps a path's inode when the object there is replaced.
type fakeEngine struct {
	mu      sync.Mutex
	root    string
	objects map[string]*fakeObject // by path
	paths   map[uint64]string      // the inode table
	inodes  map[string]uint64
	nlookup map[uint64]uint64
	dirs    map[uint64]bool // open directory handles
	nextFh  uint64
	// block, when set, holds GetAttr until it is closed.
	block chan struct{}
}

type fakeObject struct {
	ino    uint64
	mode   uint32
	data   []byte
	target string
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		root:    "/c/fs1",
		objects: map[string]*fakeObject{},
		paths:   map[uint64]string{rootInode: "/c/fs1"},
		inodes:  map[string]uint64{"/c/fs1": rootInode},
		nlookup: map[uint64]uint64{rootInode: 1},
		dirs:    map[uint64]bool{},
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
	f.mu.Lock()
	defer f.mu.Unlock()
	dir, ok := f.paths[h.NodeId]
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFh++
	out.Fh = f.nextFh
	return fuse.OK
}

func (f *fakeEngine) Read(_ <-chan struct{}, in *fuse.ReadIn, buf []byte) (fuse.ReadResult, fuse.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[f.paths[in.NodeId]]
	if o == nil {
		return nil, fuse.EIO
	}
	n := copy(buf, o.data[min(int(in.Offset), len(o.data)):])
	return fuse.ReadResultData(buf[:n]), fuse.OK
}

func (f *fakeEngine) Release(<-chan struct{}, *fuse.ReleaseIn) {}

func (f *fakeEngine) Readlink(_ <-chan struct{}, h *fuse.InHeader) ([]byte, fuse.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return []byte(f.objects[f.paths[h.NodeId]].target), fuse.OK
}

func (f *fakeEngine) OpenDir(_ <-chan struct{}, in *fuse.OpenIn, out *fuse.OpenOut) fuse.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextFh++
	out.Fh = f.nextFh
	f.dirs[out.Fh] = true
	return fuse.OK
}

func (f *fakeEngine) ReleaseDir(in *fuse.ReleaseIn) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.dirs, in.Fh)
}

// ReadDirectoryInto lists the way WFS does: "." and ".." at offsets 1 and
// 2, then the children by name, each at its index plus 3; a READDIRPLUS
// entry takes a lookup reference once the sink accepts it.
func (f *fakeEngine) ReadDirectoryInto(in *fuse.ReadIn, sink weedmount.DirEntrySink, plus bool) fuse.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirs[in.Fh] {
		panic("readdir on a directory handle that is not open")
	}
	dir := f.paths[in.NodeId]
	var names []string
	for p := range f.objects {
		if path.Dir(p) == dir {
			names = append(names, path.Base(p))
		}
	}
	sort.Strings(names)
	add := func(e fuse.DirEntry) bool {
		if plus {
			return sink.AddEntryPlus(e) != nil
		}
		return sink.AddEntry(e)
	}
	offset := in.Offset
	if offset < 2 {
		if offset == 0 {
			add(fuse.DirEntry{Name: ".", Mode: syscall.S_IFDIR, Off: 1})
		}
		add(fuse.DirEntry{Name: "..", Mode: syscall.S_IFDIR, Off: 2})
		offset = 2
	}
	for i := int(offset - 2); i < len(names); i++ {
		p := dir + "/" + names[i]
		o := f.objects[p]
		inode := o.ino
		if known, ok := f.inodes[p]; ok {
			inode = known
		}
		if !add(fuse.DirEntry{Name: names[i], Mode: o.mode, Ino: inode, Off: uint64(i) + 3}) {
			return fuse.OK
		}
		if plus {
			inode = f.register(p, o)
			f.nlookup[inode]++
		}
	}
	return fuse.OK
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
