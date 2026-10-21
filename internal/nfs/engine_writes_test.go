package nfs

import (
	"context"
	"path"
	"syscall"
	"time"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
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
	// context, as for a lock another mount holds.
	held bool
	// writeHandles are the shared write handles open; opens and releases
	// count them, fsyncs the flushes.
	writeHandles            map[uint64]bool
	opens, releases, fsyncs int
	// fsyncErr fails flushes; fsyncGate, while set, holds them until
	// closed.
	fsyncErr  fuse.Status
	fsyncGate chan struct{}
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

// prepare is a helper's preparation: waiting while held, then Begin.
func (f *fakeEngine) prepare(step weedmount.NFSStep) fuse.Status {
	f.mu.Lock()
	held := f.held
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
	if attrs.Mode != nil {
		mode = mode&syscall.S_IFMT | *attrs.Mode&0o7777
	}
	o := &fakeObject{ino: f.nextIno, mode: mode}
	f.objects[p] = o
	inode := f.register(p, o)
	f.nlookup[inode]++
	return inode
}

func (f *fakeEngine) NFSCreate(step weedmount.NFSStep, dir uint64, name string, mode weedmount.NFSCreateMode, attrs weedmount.NFSAttrs, verifier [8]byte, uid, gid uint32) (uint64, fuse.Status) {
	if st := f.prepare(step); st != fuse.OK {
		return 0, st
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, st := f.child(dir, name)
	if st != fuse.OK {
		return 0, st
	}
	if o := f.objects[p]; o != nil {
		switch {
		case mode == weedmount.NFSGuarded,
			mode == weedmount.NFSExclusive && (o.verifier == nil || *o.verifier != verifier):
			return 0, fuse.Status(syscall.EEXIST)
		case attrs.Size != nil:
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
	if st := f.prepare(step); st != fuse.OK {
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
	if st := f.prepare(step); st != fuse.OK {
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
	if st := f.prepare(step); st != fuse.OK {
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
	if st := f.prepare(step); st != fuse.OK {
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
		f.writeHandles = map[uint64]bool{}
	}
	f.nextFh++
	f.writeHandles[f.nextFh] = true
	f.opens++
	return weedmount.FileHandleId(f.nextFh), fuse.OK
}

func (f *fakeEngine) Write(_ <-chan struct{}, in *fuse.WriteIn, data []byte) (uint32, fuse.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.writeHandles[in.Fh] {
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

func (f *fakeEngine) Fsync(_ <-chan struct{}, in *fuse.FsyncIn) fuse.Status {
	f.mu.Lock()
	gate := f.fsyncGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fsyncs++
	if f.fsyncErr != fuse.OK {
		return f.fsyncErr
	}
	return fuse.OK
}
