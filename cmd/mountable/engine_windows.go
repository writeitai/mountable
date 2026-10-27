package main

// On Windows the engine is served through WinFsp, which the user installs
// once per machine, using the mount engine's WinFsp adapter: no service or
// launcher, one volume per process. The design is
// design/designs/windows-mount.md in writeitai/fsit.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"

	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/mount/winfsp"
	"golang.org/x/sys/windows"
)

// abortFallback is what still ends a mount the abort could not fully end:
// WinFsp removes a volume whose process has exited.
const abortFallback = "it ends when this process exits"

// abortTimeout bounds how long an abort waits for the volume to go.
const abortTimeout = 5 * time.Second

// dirInfoTimeout is how long Windows may serve a directory listing, a
// path's resolution or the volume's statistics from its cache. File data
// and attributes stay cached until a change notification purges them.
const dirInfoTimeout = time.Second

// engine is one mount served by this process through WinFsp.
type engine struct {
	dir      string
	readOnly bool
	wfs      *weedmount.WFS
	host     *winfsp.Host
	fs       *privateFS
	done     chan struct{} // closed when the volume is gone
	serveErr error         // why serving ended, once done is closed
}

// attach serves wfs through WinFsp at c.dir, a drive letter or a directory
// that does not exist yet.
func attach(ctx context.Context, wfs *weedmount.WFS, c mountConfig) (*engine, error) {
	if err := loadWinFsp(); err != nil {
		return nil, err
	}
	me, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	e := &engine{dir: c.dir, readOnly: c.readOnly, wfs: wfs, done: make(chan struct{})}
	initialized := make(chan struct{})
	e.host = winfsp.New(wfs, winfsp.Options{
		VolumeName:   "mountable",
		Uid:          ownerID,
		Gid:          ownerID,
		CacheTimeout: dirInfoTimeout,
		ReadOnly:     c.readOnly,
		// Every file's access list, whatever its mode: full access for
		// this user and SYSTEM, nobody else, so other local users cannot
		// open anything in the mount (design §3). From mode bits alone
		// WinFsp would also let Everyone read attributes and extended
		// attributes.
		ExtraOptions: []string{"FileSecurity=" + fileSecurity(me)},
		Wrap: func(adapter cgofuse.FileSystemInterface) cgofuse.FileSystemInterface {
			e.fs = newPrivateFS(adapter, c.readOnly, initialized)
			return e.fs
		},
	})
	e.host.Notify(wfs)
	go func() {
		e.serveErr = e.host.Serve(c.dir)
		close(e.done)
	}()
	select {
	case <-initialized:
		// Kept above WinFsp's own handler while the mount lasts (design §5).
		keepConsole(e.done)
	case <-e.done:
		return nil, fmt.Errorf("mounting %s: %w", c.dir, e.servedError())
	case <-ctx.Done():
		e.abandon()
		return nil, ctx.Err()
	}
	err = unlessCancelled(ctx, func() error {
		return withTimeout(helperTimeout, func() error { return readRoot(mountRoot(c.dir)) })
	})
	if err != nil {
		// Attached but not serving: never leave it behind.
		if abortErr := e.abort(); abortErr != nil {
			err = fmt.Errorf("%w; cleaning up the mount: %v", err, abortErr)
		}
		return nil, fmt.Errorf("mounting %s: %w", c.dir, err)
	}
	return e, nil
}

// abandon ends a mount whose start was cancelled before WinFsp reported it
// mounted: once it does, or fails, it is unmounted at once.
func (e *engine) abandon() {
	go func() {
		select {
		case <-e.fs.initialized:
			_ = e.abort()
		case <-e.done:
		}
	}()
}

func (e *engine) servedError() error {
	if e.serveErr != nil {
		return e.serveErr
	}
	return errors.New("WinFsp stopped serving the volume")
}

// mountRoot is the path of the mount's root directory: "X:\" for a drive.
func mountRoot(dir string) string {
	if letter, ok := driveLetter(dir); ok {
		return letter + `\`
	}
	return dir
}

// readRoot reads the first name in the mounted root.
func readRoot(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading the mounted root: %w", err)
	}
	return nil
}

func (e *engine) mountPoint() string { return e.dir }

func (e *engine) served() <-chan struct{} { return e.done }

// unmount removes the volume and waits for serving to stop. WinFsp
// discards the handles still open without their Close, which is where a
// closed file's writes are committed (design §5). So from now on opens are
// refused, and every file still open for writing is first committed
// through the engine: all its program has written out so far. A program
// that still holds such a file loses what it writes after that, which the
// warning says.
func (e *engine) unmount() error {
	select {
	case <-e.done:
		return nil
	default:
	}
	held, err := e.fs.closing()
	if err != nil {
		return err
	}
	if held > 0 {
		diagnose("%d files were open for writing; what was written to them is saved, and anything their programs write from now on is lost", held)
	}
	if !e.host.Unmount() {
		return errors.New("WinFsp did not accept the unmount")
	}
	<-e.done
	return nil
}

// flush runs once serving has stopped: it waits for pending writes, then
// drops the local caches.
func (e *engine) flush() {
	e.wfs.WaitForAsyncFlush()
	e.wfs.ClearCacheDir()
}

// close has nothing to release: serving ends with the volume.
func (e *engine) close() {}

// abort ends the mount at once without flushing: every further operation
// fails, and the volume is removed. It fails if the volume is still there
// after abortTimeout; exiting the process then removes it.
func (e *engine) abort() error {
	e.fs.end()
	select {
	case <-e.done:
		return nil
	default:
	}
	if !e.host.Unmount() {
		return errors.New("WinFsp did not accept the unmount")
	}
	select {
	case <-e.done:
		return nil
	case <-time.After(abortTimeout):
		return fmt.Errorf("the volume was still mounted after %s", abortTimeout)
	}
}

// closing refuses every open from now on, then commits every file still
// open for writing, and returns how many there were. A failed commit is
// the error: the clean unmount then turns into an abort.
func (p *privateFS) closing() (int, error) {
	p.mu.Lock()
	p.isClosing = true
	var writers []uint64
	for fh, f := range p.handles {
		if f.writing {
			writers = append(writers, fh)
		}
	}
	p.mu.Unlock()
	for _, fh := range writers {
		if errc := p.FileSystemInterface.Fsync("", false, fh); errc != 0 {
			return len(writers), fmt.Errorf("committing a file open for writing: %v", cgofuse.Error(errc))
		}
	}
	return len(writers), nil
}

// fileSecurity is the access list every file in the mount gets, in SDDL:
// protected (nothing inherited), full access for user and SYSTEM.
func fileSecurity(user *windows.SID) string {
	return "D:P(A;;FA;;;" + user.String() + ")(A;;FA;;;SY)"
}

// privateFS is the adapter as WinFsp is served it (design §3, §4, §5):
//   - a chmod changes only the owner's read and write bits, keeping the
//     stored execute, group and other bits other mounts see, which Windows
//     cannot express;
//   - files and directories created from Windows are stored 0644 and 0755,
//     as on other mounts, not with the mode WinFsp derives from the
//     owner-only access list;
//   - a read-only session refuses opens for writing before they reach the
//     engine, which would take the file's writer lock;
//   - it counts the files open for writing, which a clean unmount commits
//     first, refusing new opens meanwhile;
//   - once the mount has ended (revocation, expiry), every operation fails
//     at once without reaching the engine.
type privateFS struct {
	cgofuse.FileSystemInterface
	readOnly    bool
	initialized chan struct{}
	initOnce    sync.Once
	ended       atomic.Bool

	// The engine gives repeated opens of one file the same handle, and a
	// release does not say which open it ends; so a handle counts as open
	// for writing from its first open for writing until its last release.
	mu        sync.Mutex
	handles   map[uint64]*openFile
	isClosing bool // a clean unmount has begun: opens are refused
}

type openFile struct {
	opens   int
	writing bool
}

func newPrivateFS(adapter cgofuse.FileSystemInterface, readOnly bool, initialized chan struct{}) *privateFS {
	return &privateFS{FileSystemInterface: adapter, readOnly: readOnly, initialized: initialized, handles: map[uint64]*openFile{}}
}

// noHandle is what WinFsp passes when an operation carries no open handle.
const noHandle = ^uint64(0)

// writes reports whether open flags ask for writing.
func writes(flags int) bool {
	return flags&cgofuse.O_ACCMODE != cgofuse.O_RDONLY || flags&(cgofuse.O_TRUNC|cgofuse.O_APPEND) != 0
}

// admit refuses an open once a clean unmount has begun.
func (p *privateFS) admit() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosing {
		return -cgofuse.EBUSY
	}
	return 0
}

func (p *privateFS) opened(fh uint64, writing bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.handles[fh]
	if f == nil {
		f = &openFile{}
		p.handles[fh] = f
	}
	f.opens++
	f.writing = f.writing || writing
}

func (p *privateFS) released(fh uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f := p.handles[fh]; f != nil {
		if f.opens--; f.opens <= 0 {
			delete(p.handles, fh)
		}
	}
}

// openWriters is how many files are open for writing.
func (p *privateFS) openWriters() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, f := range p.handles {
		if f.writing {
			n++
		}
	}
	return n
}

func (p *privateFS) end() { p.ended.Store(true) }

func (p *privateFS) Init() {
	p.FileSystemInterface.Init()
	p.initOnce.Do(func() { close(p.initialized) })
}

func (p *privateFS) Statfs(path string, stat *cgofuse.Statfs_t) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Statfs(path, stat)
}

func (p *privateFS) Mknod(path string, mode uint32, dev uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Mknod(path, mode, dev)
}

func (p *privateFS) Mkdir(path string, mode uint32) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Mkdir(path, mode&^0o7777|0o755)
}

func (p *privateFS) Unlink(path string) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Unlink(path)
}

func (p *privateFS) Rmdir(path string) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Rmdir(path)
}

func (p *privateFS) Link(oldpath, newpath string) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Link(oldpath, newpath)
}

func (p *privateFS) Symlink(target, newpath string) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Symlink(target, newpath)
}

func (p *privateFS) Readlink(path string) (int, string) {
	if p.ended.Load() {
		return -cgofuse.EIO, ""
	}
	return p.FileSystemInterface.Readlink(path)
}

func (p *privateFS) Rename(oldpath, newpath string) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Rename(oldpath, newpath)
}

// Chmod applies the owner's read and write bits from Windows and keeps the
// stored rest: Windows has no group and others to set, and its access
// list's full access would read as execute on every file.
func (p *privateFS) Chmod(path string, mode uint32) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	var stat cgofuse.Stat_t
	if errc := p.FileSystemInterface.Getattr(path, &stat, noHandle); errc != 0 {
		return errc
	}
	return p.FileSystemInterface.Chmod(path, stat.Mode&0o7177|mode&0o600)
}

func (p *privateFS) Chown(path string, uid, gid uint32) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Chown(path, uid, gid)
}

func (p *privateFS) Utimens(path string, tmsp []cgofuse.Timespec) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Utimens(path, tmsp)
}

func (p *privateFS) Access(path string, mask uint32) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Access(path, mask)
}

func (p *privateFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if p.ended.Load() {
		return -cgofuse.EIO, noHandle
	}
	if p.readOnly {
		return -cgofuse.EROFS, noHandle
	}
	if errc := p.admit(); errc != 0 {
		return errc, noHandle
	}
	errc, fh := p.FileSystemInterface.Create(path, flags, mode&^0o7777|0o644)
	if errc == 0 {
		p.opened(fh, true)
	}
	return errc, fh
}

func (p *privateFS) Open(path string, flags int) (int, uint64) {
	if p.ended.Load() {
		return -cgofuse.EIO, noHandle
	}
	if p.readOnly && writes(flags) {
		return -cgofuse.EROFS, noHandle
	}
	if errc := p.admit(); errc != 0 {
		return errc, noHandle
	}
	errc, fh := p.FileSystemInterface.Open(path, flags)
	if errc == 0 {
		p.opened(fh, writes(flags))
	}
	return errc, fh
}

func (p *privateFS) Getattr(path string, stat *cgofuse.Stat_t, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Getattr(path, stat, fh)
}

func (p *privateFS) Truncate(path string, size int64, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Truncate(path, size, fh)
}

func (p *privateFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Read(path, buff, ofst, fh)
}

func (p *privateFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Write(path, buff, ofst, fh)
}

func (p *privateFS) Flush(path string, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Flush(path, fh)
}

// Release after the end does not reach the engine, whose release would
// flush to a gateway that no longer accepts the session.
func (p *privateFS) Release(path string, fh uint64) int {
	p.released(fh)
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Release(path, fh)
}

func (p *privateFS) Fsync(path string, datasync bool, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Fsync(path, datasync, fh)
}

func (p *privateFS) Opendir(path string) (int, uint64) {
	if p.ended.Load() {
		return -cgofuse.EIO, noHandle
	}
	return p.FileSystemInterface.Opendir(path)
}

func (p *privateFS) Readdir(path string, fill func(name string, stat *cgofuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Readdir(path, fill, ofst, fh)
}

func (p *privateFS) Releasedir(path string, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Releasedir(path, fh)
}

func (p *privateFS) Fsyncdir(path string, datasync bool, fh uint64) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Fsyncdir(path, datasync, fh)
}

func (p *privateFS) Setxattr(path, name string, value []byte, flags int) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Setxattr(path, name, value, flags)
}

func (p *privateFS) Getxattr(path, name string) (int, []byte) {
	if p.ended.Load() {
		return -cgofuse.EIO, nil
	}
	return p.FileSystemInterface.Getxattr(path, name)
}

func (p *privateFS) Removexattr(path, name string) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Removexattr(path, name)
}

func (p *privateFS) Listxattr(path string, fill func(name string) bool) int {
	if p.ended.Load() {
		return -cgofuse.EIO
	}
	return p.FileSystemInterface.Listxattr(path, fill)
}
