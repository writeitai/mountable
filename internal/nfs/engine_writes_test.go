package nfs

import (
	"context"
	"path"
	"slices"
	"syscall"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
)

// writeState is the fake engine's write path: what the mutation helpers
// did, and hooks standing for another mount's locks and a slow or failing
// store.
type writeState struct {
	nextIno uint64
	// mutations counts the helpers' changes, by name ("create", "remove",
	// "setattr", ...); clearedVerifiers the SETATTRs that cleared one.
	mutations        map[string]int
	clearedVerifiers int
	// held, while set, makes every helper's preparation wait for its
	// context, as for a lock another mount holds; prepareDelay delays its
	// look at the locks.
	held         bool
	prepareDelay time.Duration
	// writeHandles are the shared write handles open, each holding its
	// inode's lease, which this mount's own mutations of the inode wait
	// for as for another mount's; opens and releases count them, discards
	// the releases dropping writes, fsyncs the flushes, fsyncUids whose.
	writeHandles                      map[uint64]uint64
	opens, releases, discards, fsyncs int
	fsyncUids                         []uint32
	// fsyncErr fails flushes; fsyncGate and writeGate, while set, hold
	// flushes and writes until closed.
	fsyncErr             fuse.Status
	fsyncGate, writeGate chan struct{}
	// madeAttrs are what the last object made was given.
	madeAttrs weedmount.NFSAttrs
}

func (f *fakeEngine) mutated(what string) {
	if f.mutations == nil {
		f.mutations = map[string]int{}
	}
	f.mutations[what]++
}

func (f *fakeEngine) count(what string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mutations[what]
}

// prepare is a helper's preparation of a change to inodes: waiting while
// held, or while one of them is leased, then Begin.
func (f *fakeEngine) prepare(step weedmount.NFSStep, inodes ...uint64) fuse.Status {
	f.mu.Lock()
	delay := f.prepareDelay
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	held := f.held
	for _, leased := range f.writeHandles {
		held = held || slices.Contains(inodes, leased)
	}
	f.mu.Unlock()
	if held {
		<-step.Prepare.Done()
		return weedmount.NFSRetryLater
	}
	if !step.Begin() {
		return weedmount.NFSRetryLater
	}
	return fuse.OK
}

// inodeOf is the inode of name in the directory dir, or 0.
func (f *fakeEngine) inodeOf(dir uint64, name string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inodes[f.paths[dir]+"/"+name]
}

// child is the path of name in the directory dir, under mu.
func (f *fakeEngine) child(dir uint64, name string) (string, fuse.Status) {
	d, ok := f.paths[dir]
	if !ok {
		return "", fuse.ENOENT
	}
	return d + "/" + name, fuse.OK
}

// make stores a new object at p and enters it, with a lookup reference,
// under mu.
func (f *fakeEngine) make(p string, mode uint32, attrs weedmount.NFSAttrs) uint64 {
	if f.nextIno == 0 {
		f.nextIno = 5000
	}
	f.nextIno++
	f.madeAttrs = attrs
	if attrs.Mode != nil {
		mode = mode&syscall.S_IFMT | *attrs.Mode&0o7777
	}
	o := &fakeObject{ino: f.nextIno, mode: mode}
	f.objects[p] = o
	inode := f.register(p, o)
	f.nlookup[inode]++
	return inode
}

func (f *fakeEngine) NFSCreate(step weedmount.NFSStep, dir uint64, name string, mode weedmount.NFSCreateMode, attrs weedmount.NFSAttrs, verifier [8]byte, uid, gid uint32, reuse func(*filer_pb.Entry) fuse.Status) (uint64, fuse.Status) {
	if st := f.prepare(step, f.inodeOf(dir, name)); st != fuse.OK {
		return 0, st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, st := f.child(dir, name)
	if st != fuse.OK {
		return 0, st
	}
	if o := f.objects[p]; o != nil {
		owner := uint32(501)
		if o.uid != 0 {
			owner = o.uid
		}
		switch {
		case mode == weedmount.NFSUnchecked && o.mode&syscall.S_IFMT == syscall.S_IFDIR:
			return 0, fuse.Status(syscall.EISDIR)
		case mode == weedmount.NFSGuarded, o.mode&syscall.S_IFMT != syscall.S_IFREG,
			mode == weedmount.NFSExclusive && (o.verifier == nil || *o.verifier != verifier):
			return 0, fuse.Status(syscall.EEXIST)
		}
		if mode == weedmount.NFSUnchecked && reuse != nil {
			if st := reuse(&filer_pb.Entry{Attributes: &filer_pb.FuseAttributes{FileMode: o.mode & 0o7777, Uid: owner, Gid: 20}}); st != fuse.OK {
				return 0, st
			}
		}
		if attrs.Size != nil {
			o.data = o.data[:min(len(o.data), int(*attrs.Size))]
		}
		inode := f.register(p, o)
		f.nlookup[inode]++
		return inode, fuse.OK
	}
	f.mutated("create")
	inode := f.make(p, syscall.S_IFREG|0o644, attrs)
	if mode == weedmount.NFSExclusive {
		v := verifier
		f.objects[p].verifier = &v
	}
	return inode, fuse.OK
}

func (f *fakeEngine) NFSMkdir(step weedmount.NFSStep, dir uint64, name string, attrs weedmount.NFSAttrs, uid, gid uint32) (uint64, fuse.Status) {
	if st := f.prepare(step); st != fuse.OK {
		return 0, st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, st := f.child(dir, name)
	if st != fuse.OK {
		return 0, st
	}
	if f.objects[p] != nil {
		return 0, fuse.Status(syscall.EEXIST)
	}
	f.mutated("mkdir")
	return f.make(p, syscall.S_IFDIR|0o755, attrs), fuse.OK
}

func (f *fakeEngine) NFSSymlink(step weedmount.NFSStep, dir uint64, name, target string, attrs weedmount.NFSAttrs, uid, gid uint32) (uint64, fuse.Status) {
	if st := f.prepare(step); st != fuse.OK {
		return 0, st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, st := f.child(dir, name)
	if st != fuse.OK {
		return 0, st
	}
	if f.objects[p] != nil {
		return 0, fuse.Status(syscall.EEXIST)
	}
	f.mutated("symlink")
	inode := f.make(p, syscall.S_IFLNK|0o777, weedmount.NFSAttrs{})
	f.objects[p].target = target
	return inode, fuse.OK
}

func (f *fakeEngine) NFSRemove(step weedmount.NFSStep, dir uint64, name string) fuse.Status {
	return f.removeName(step, dir, name, false)
}

func (f *fakeEngine) NFSRmdir(step weedmount.NFSStep, dir uint64, name string) fuse.Status {
	return f.removeName(step, dir, name, true)
}

func (f *fakeEngine) removeName(step weedmount.NFSStep, dir uint64, name string, directory bool) fuse.Status {
	if st := f.prepare(step, f.inodeOf(dir, name)); st != fuse.OK {
		return st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, st := f.child(dir, name)
	if st != fuse.OK {
		return st
	}
	if f.objects[p] == nil {
		return fuse.ENOENT
	}
	if directory {
		for other := range f.objects {
			if path.Dir(other) == p {
				return fuse.Status(syscall.ENOTEMPTY)
			}
		}
	}
	f.mutated("remove")
	delete(f.objects, p)
	delete(f.inodes, p) // the name; the inode stays while referenced
	return fuse.OK
}

func (f *fakeEngine) NFSRename(step weedmount.NFSStep, fromDir uint64, fromName string, toDir uint64, toName string) fuse.Status {
	if st := f.prepare(step, f.inodeOf(fromDir, fromName), f.inodeOf(toDir, toName)); st != fuse.OK {
		return st
	}
	f.mu.Lock()
	from, st := f.child(fromDir, fromName)
	if st != fuse.OK {
		f.mu.Unlock()
		return st
	}
	to, st := f.child(toDir, toName)
	if st != fuse.OK {
		f.mu.Unlock()
		return st
	}
	if f.objects[from] == nil {
		f.mu.Unlock()
		return fuse.ENOENT
	}
	f.mutated("rename")
	delete(f.inodes, to)
	f.mu.Unlock()
	root := f.root + "/"
	f.rename(from[len(root):], to[len(root):])
	return fuse.OK
}

func (f *fakeEngine) NFSLink(step weedmount.NFSStep, source, dir uint64, name string, uid, gid uint32) fuse.Status {
	if st := f.prepare(step, source); st != fuse.OK {
		return st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, st := f.child(dir, name)
	if st != fuse.OK {
		return st
	}
	if f.objects[p] != nil {
		return fuse.Status(syscall.EEXIST)
	}
	o := f.objects[f.paths[source]]
	if o == nil {
		return fuse.ENOENT
	}
	f.mutated("link")
	f.objects[p], f.inodes[p] = o, source
	return fuse.OK
}

func (f *fakeEngine) NFSSetAttr(step weedmount.NFSStep, inode uint64, attrs weedmount.NFSAttrs, guard *time.Time, clearVerifier bool) fuse.Status {
	if st := f.prepare(step, inode); st != fuse.OK {
		return st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[f.paths[inode]]
	if o == nil {
		return fuse.ENOENT
	}
	if guard != nil && !guard.Equal(time.Unix(1700000001, 5)) { // the fake's ctime
		return weedmount.NFSNotSync
	}
	f.mutated("setattr")
	if attrs.Mode != nil {
		o.mode = o.mode&syscall.S_IFMT | *attrs.Mode&0o7777
	}
	if attrs.Size != nil {
		o.data = append(o.data, make([]byte, max(0, int(*attrs.Size)-len(o.data)))...)[:*attrs.Size]
	}
	if clearVerifier {
		f.clearedVerifiers++
		o.verifier = nil
	}
	return fuse.OK
}

func (f *fakeEngine) NFSOpenForWrite(ctx context.Context, inode uint64, uid, gid uint32) (weedmount.FileHandleId, fuse.Status) {
	f.mu.Lock()
	held := f.held
	f.mu.Unlock()
	if held {
		<-ctx.Done()
		return 0, weedmount.NFSRetryLater
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeHandles == nil {
		f.writeHandles = map[uint64]uint64{}
	}
	f.nextFh++
	f.writeHandles[f.nextFh] = inode
	f.opens++
	return weedmount.FileHandleId(f.nextFh), fuse.OK
}

func (f *fakeEngine) NFSCloseRead(weedmount.FileHandleId) {}

func (f *fakeEngine) NFSCloseWrite(fh weedmount.FileHandleId, discard bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, open := f.writeHandles[uint64(fh)]; open {
		delete(f.writeHandles, uint64(fh))
		f.releases++
		if discard {
			f.discards++
		}
	}
}

func (f *fakeEngine) Write(_ <-chan struct{}, in *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	f.mu.Lock()
	gate := f.writeGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, open := f.writeHandles[in.Fh]; !open {
		return 0, fuse.EBADF
	}
	o := f.objects[f.paths[in.NodeId]]
	if o == nil {
		return 0, fuse.ENOENT
	}
	if end := int(in.Offset) + len(data); end > len(o.data) {
		o.data = append(o.data, make([]byte, end-len(o.data))...)
	}
	copy(o.data[in.Offset:], data)
	return uint32(len(data)), fuse.OK
}

func (f *fakeEngine) NFSFsync(fh weedmount.FileHandleId, uid, gid uint32) fuse.Status {
	f.mu.Lock()
	gate := f.fsyncGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fsyncs++
	f.fsyncUids = append(f.fsyncUids, uid)
	if f.fsyncErr != fuse.OK {
		return f.fsyncErr
	}
	return fuse.OK
}
