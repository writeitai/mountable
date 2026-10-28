package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	cgofuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// testMountPoint is a mount point the CLI accepts: a directory that does
// not exist yet.
func testMountPoint(t *testing.T) string { return filepath.Join(t.TempDir(), "mnt") }

// missingMountPoint is one it refuses for want of a directory: its parent
// does not exist.
func missingMountPoint(t *testing.T) string { return filepath.Join(t.TempDir(), "missing", "mnt") }

// engineCacheDir is a cache directory for an engine a test leaves running.
// Windows cannot remove the files it still holds open, so the directory is
// removed as far as it can be.
func engineCacheDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "cache")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// shortTempDir is a temporary directory with a short path: a control
// socket's path must fit in 107 bytes, and t.TempDir's are long.
func shortTempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "m")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A mount's cache directory goes under %LOCALAPPDATA%, is removed when the
// mount ends, and a dead mount's is pruned by the next.
func TestCacheDirs(t *testing.T) {
	t.Setenv("LocalAppData", shortTempDir(t))
	dead, releaseDead, err := mountCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dead, os.Getenv("LocalAppData")) {
		t.Fatalf("cache directory %s is not under %%LOCALAPPDATA%%", dead)
	}
	running, releaseRunning, err := mountCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRunning()
	releaseDead() // its process is gone
	dir, release, err := mountCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{dead: false, running: true, dir: true} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s: exists %v, want %v", path, err == nil, want)
		}
	}
	release()
	if err := os.RemoveAll(dir); err != nil {
		t.Errorf("removing a released cache directory: %v", err)
	}
}

func TestDriveLetter(t *testing.T) {
	for in, want := range map[string]string{"x:": "X:", `X:\`: "X:", "z:/": "Z:"} {
		if got, ok := driveLetter(in); !ok || got != want {
			t.Errorf("driveLetter(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "x", `x:\dir`, "1:", `\\server\share`, "xy:"} {
		if got, ok := driveLetter(in); ok {
			t.Errorf("driveLetter(%q) = %q, want none", in, got)
		}
	}
	if got, err := canonicalDir("q:"); err != nil || got != "Q:" {
		t.Errorf("canonicalDir(q:) = %q, %v", got, err)
	}
}

func TestCheckMountDir(t *testing.T) {
	base := t.TempDir()
	if err := checkMountDir(filepath.Join(base, "new")); err != nil {
		t.Errorf("a directory that does not exist yet: %v", err)
	}
	if err := checkMountDir(base); err == nil {
		t.Error("an existing directory was accepted")
	}
	if err := checkMountDir(filepath.Join(base, "missing", "new")); err == nil {
		t.Error("a missing parent was accepted")
	}
	if err := checkMountDir(`\\server\share`); err == nil {
		t.Error("a network name was accepted")
	}
	system, _ := driveLetter(os.Getenv("SystemDrive"))
	if err := checkMountDir(system); err == nil {
		t.Errorf("the system drive %s was accepted", system)
	}
}

func TestCheckPrivate(t *testing.T) {
	dir := t.TempDir() // under %LOCALAPPDATA%\Temp, so it inherits a private access list
	if err := checkPrivate(dir); err != nil {
		t.Fatalf("a fresh temporary directory: %v", err)
	}
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", open, "/grant", "*S-1-1-0:(R)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	if err := checkPrivate(open); err == nil {
		t.Error("a directory everyone can read was accepted")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(file); err == nil {
		t.Error("a file was accepted")
	}
}

func TestLockFileIsPerOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	a, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := tryLockFile(a); err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(b); err == nil {
		t.Fatal("a second open took a held lock")
	}
	if err := unlockFile(a); err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(b); err != nil {
		t.Fatalf("after unlocking: %v", err)
	}
	b.Close()
	if err := lockFile(a); err != nil {
		t.Fatalf("after the holder closed: %v", err)
	}
}

func TestControlSocket(t *testing.T) {
	t.Setenv("LocalAppData", shortTempDir(t))
	dir := filepath.Join(t.TempDir(), "mnt")
	signals := make(chan os.Signal, 2)
	finish, err := serveControl(dir, signals)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serveControl(dir, make(chan os.Signal, 2)); err == nil {
		t.Fatal("a second mount at the same mount point was accepted")
	}
	// Windows names are case-insensitive; the peer is this process.
	m, err := connectMount(strings.ToUpper(dir))
	if err != nil {
		t.Fatal(err)
	}
	if pid, err := windows.GetProcessId(m.process); err != nil || int(pid) != os.Getpid() {
		t.Fatalf("the peer process is %d (%v), want %d", pid, err, os.Getpid())
	}
	m.close()
	answered := make(chan error, 1)
	go func() { answered <- cleanUnmount(dir) }()
	select {
	case <-signals:
	case <-time.After(5 * time.Second):
		t.Fatal("unmount did not arrive as a signal")
	}
	select {
	case err := <-answered:
		t.Fatalf("answered before the mount ended: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	finish(nil)
	if err := <-answered; err != nil {
		t.Fatalf("unmount: %v", err)
	}
	if _, err := connectMount(dir); !notMounted(err) {
		t.Fatalf("after the mount ended: %v, want not mounted", err)
	}
}

func TestControlSocketReportsAFailedEnd(t *testing.T) {
	t.Setenv("LocalAppData", shortTempDir(t))
	dir := filepath.Join(t.TempDir(), "mnt")
	signals := make(chan os.Signal, 2)
	finish, err := serveControl(dir, signals)
	if err != nil {
		t.Fatal(err)
	}
	answered := make(chan error, 1)
	go func() { answered <- abortMount(dir) }()
	for range 2 {
		select {
		case sig := <-signals:
			if sig != abortRequest {
				t.Fatalf("abort arrived as %v, which a repeat would not abort", sig)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("abort did not arrive as two signals")
		}
	}
	finish(errors.New("aborted"))
	if err := <-answered; err != nil {
		t.Fatalf("abort: %v", err)
	}
}

func TestStaleControlSocketIsNotMounted(t *testing.T) {
	t.Setenv("LocalAppData", shortTempDir(t))
	dir := filepath.Join(t.TempDir(), "mnt")
	path, err := controlPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil { // left by a killed mount
		t.Fatal(err)
	}
	if err := cleanUnmount(dir); !notMounted(err) {
		t.Fatalf("cleanUnmount: %v, want not mounted", err)
	}
	finish, err := serveControl(dir, make(chan os.Signal, 2))
	if err != nil {
		t.Fatalf("mounting over a stale socket: %v", err)
	}
	finish(nil)
}

// fakeFS records what reaches the adapter.
type fakeFS struct {
	cgofuse.FileSystemBase
	mode    uint32
	chmoded uint32
	created uint32
	made    uint32
	synced  []uint64
	calls   int
	// handles, when set, gives each path its own handle; failSync fails
	// those handles' fsyncs.
	handles  map[string]uint64
	failSync map[uint64]int
	released []uint64
}

func (f *fakeFS) Fsync(path string, datasync bool, fh uint64) int {
	f.calls++
	if errc := f.failSync[fh]; errc != 0 {
		return errc
	}
	f.synced = append(f.synced, fh)
	return 0
}

func (f *fakeFS) Getattr(path string, stat *cgofuse.Stat_t, fh uint64) int {
	f.calls++
	stat.Mode = f.mode
	return 0
}

func (f *fakeFS) Chmod(path string, mode uint32) int {
	f.calls++
	f.chmoded = mode
	return 0
}

func (f *fakeFS) Create(path string, flags int, mode uint32) (int, uint64) {
	f.calls++
	f.created = mode
	return 0, 7
}

func (f *fakeFS) Mkdir(path string, mode uint32) int {
	f.calls++
	f.made = mode
	return 0
}

// Open hands every open of a file the same handle, as the engine does.
func (f *fakeFS) Open(path string, flags int) (int, uint64) {
	f.calls++
	if fh, ok := f.handles[path]; ok {
		return 0, fh
	}
	return 0, 9
}

func (f *fakeFS) Release(path string, fh uint64) int {
	f.calls++
	f.released = append(f.released, fh)
	return 0
}

func (f *fakeFS) Rename(oldpath, newpath string) int {
	f.calls++
	return 0
}

func TestPrivateFS(t *testing.T) {
	inner := &fakeFS{mode: cgofuse.S_IFREG | 0o4654}
	p := newPrivateFS(inner, false, make(chan struct{}))
	// Windows' full access reads as rwx: only read and write are taken.
	if p.Chmod("/f", 0o700); inner.chmoded != 0o4654 {
		t.Errorf("Chmod 700 stored %o, want 4654: no execute bit from Windows", inner.chmoded)
	}
	if p.Chmod("/f", 0o400); inner.chmoded != 0o4454 {
		t.Errorf("Chmod 400 stored %o, want 4454: the owner's write bit cleared only", inner.chmoded)
	}
	if p.Create("/f", cgofuse.O_RDWR|cgofuse.O_CREAT, cgofuse.S_IFREG|0o700); inner.created != cgofuse.S_IFREG|0o644 {
		t.Errorf("Create stored mode %o, want 100644", inner.created)
	}
	if p.Mkdir("/d", 0o700); inner.made != 0o755 {
		t.Errorf("Mkdir stored mode %o, want 755", inner.made)
	}
	if n := p.openWriters(); n != 1 {
		t.Errorf("after a create, %d open for writing, want 1", n)
	}
	p.Release("/f", 7)
	// A read open and a write open of one file share a handle; it is open
	// for writing until both are released.
	p.Open("/g", cgofuse.O_RDONLY)
	p.Open("/g", cgofuse.O_WRONLY)
	p.Release("/g", 9)
	if n := p.openWriters(); n != 1 {
		t.Errorf("with one of two opens released, %d open for writing, want 1", n)
	}
	p.Release("/g", 9)
	if n := p.openWriters(); n != 0 {
		t.Errorf("all released, %d open for writing", n)
	}
	// A clean unmount commits what is open for writing, and refuses opens.
	p.Open("/w", cgofuse.O_WRONLY)
	var flushed []string
	if n, problems := p.closing(func(path string) bool { flushed = append(flushed, path); return true }); n != 1 || problems != nil || len(inner.synced) != 1 || inner.synced[0] != 9 {
		t.Errorf("closing: %d, %v; synced %v", n, problems, inner.synced)
	}
	if len(flushed) != 1 || flushed[0] != "/w" {
		t.Errorf("Windows' cache was flushed for %q before the commit, want /w", flushed)
	}
	if errc, _ := p.Open("/f", cgofuse.O_RDONLY); errc != -cgofuse.EBUSY {
		t.Errorf("Open while closing: %d, want EBUSY", errc)
	}
	p.Init()
	select {
	case <-p.initialized:
	default:
		t.Error("Init did not report the mount")
	}
	p.end()
	before := inner.calls
	var stat cgofuse.Stat_t
	if errc := p.Getattr("/f", &stat, noHandle); errc != -cgofuse.EIO {
		t.Errorf("Getattr after the end: %d", errc)
	}
	if errc, _ := p.Open("/f", 0); errc != -cgofuse.EIO {
		t.Errorf("Open after the end: %d", errc)
	}
	if errc := p.Release("/f", 1); errc != -cgofuse.EIO {
		t.Errorf("Release after the end: %d", errc)
	}
	if inner.calls != before {
		t.Error("an operation after the end reached the adapter")
	}
}

// releasingFS releases the file its fsync is asked for first, as a
// program closing it during a clean unmount does.
type releasingFS struct {
	fakeFS
	p *privateFS
}

func (f *releasingFS) Fsync(path string, datasync bool, fh uint64) int {
	f.p.Release(path, fh)
	return -cgofuse.ENOENT // the engine no longer has the handle
}

func TestClosingSkipsFilesReleasedMeanwhile(t *testing.T) {
	inner := &releasingFS{}
	p := newPrivateFS(inner, false, make(chan struct{}))
	inner.p = p
	p.Open("/w", cgofuse.O_WRONLY)
	if n, problems := p.closing(func(string) bool { return true }); n != 1 || problems != nil {
		t.Fatalf("closing: %d, %v; a file released meanwhile is no failure", n, problems)
	}
}

// A clean unmount writes out Windows' cache for an open file by the name
// it has then: renamed itself, as git renames its index.lock over the
// index, or with its directory. The index it replaced, still open, now
// shares that name; an unlinked file keeps its last one.
func TestClosingFlushesRenamedFiles(t *testing.T) {
	inner := &fakeFS{handles: map[string]uint64{"/.git/index": 1, "/.git/index.lock": 2, "/gone": 3}}
	p := newPrivateFS(inner, false, make(chan struct{}))
	p.Open("/.git/index", cgofuse.O_WRONLY)
	p.Open("/.git/index.lock", cgofuse.O_WRONLY)
	p.Open("/gone", cgofuse.O_WRONLY)
	p.Unlink("/gone")
	p.Rename("/.git/index.lock", "/.git/index")
	p.Rename("/.git", "/.git2")
	p.Rename("/.git", "/other") // a string prefix of /.git2/index, not a path prefix
	var flushed []string
	_, problems := p.closing(func(path string) bool { flushed = append(flushed, path); return true })
	slices.Sort(flushed)
	if want := []string{"/.git2/index", "/.git2/index", "/gone"}; problems != nil || !slices.Equal(flushed, want) {
		t.Errorf("closing flushed %q (%v), want %q", flushed, problems, want)
	}
	if errc := p.Rename("/.git2/index", "/x"); errc != -cgofuse.EBUSY {
		t.Errorf("a rename while closing: %d, want EBUSY: the flushes go by name", errc)
	}
}

// A file whose cache WinFsp will not flush is not committed, which would
// store zeros where its cached pages were; a failed commit is reported.
// Neither stops the other files' commits.
func TestClosingCommitsWhatItCan(t *testing.T) {
	attempts, pause := flushAttempts, flushPause
	flushAttempts, flushPause = 3, 0
	t.Cleanup(func() { flushAttempts, flushPause = attempts, pause })
	inner := &fakeFS{handles: map[string]uint64{"/a": 1, "/b": 2, "/c": 3}, failSync: map[uint64]int{2: -cgofuse.EIO}}
	p := newPrivateFS(inner, false, make(chan struct{}))
	for _, name := range []string{"/a", "/b", "/c"} {
		p.Open(name, cgofuse.O_WRONLY)
	}
	tries := map[string]int{}
	n, problems := p.closing(func(path string) bool { tries[path]++; return path != "/a" })
	if n != 3 || len(problems) != 2 || !slices.Equal(inner.synced, []uint64{3}) {
		t.Fatalf("closing: %d, %v; synced %v, want only c's", n, problems, inner.synced)
	}
	if tries["/a"] != 3 {
		t.Errorf("a refused flush was tried %d times, want 3", tries["/a"])
	}
	// Released in the engine, a would be committed after all.
	p.releaseAll()
	slices.Sort(inner.released)
	if !slices.Equal(inner.released, []uint64{2, 3}) {
		t.Errorf("released %v after the volume went, want b and c only", inner.released)
	}
	text := fmt.Sprint(problems)
	if !strings.Contains(text, "caches for a,") || !strings.Contains(text, "b could not be committed") {
		t.Errorf("the problems do not name a and b: %s", text)
	}
}

func TestReadOnlyRefusesWriteOpens(t *testing.T) {
	inner := &fakeFS{}
	p := newPrivateFS(inner, true, make(chan struct{}))
	for _, flags := range []int{cgofuse.O_WRONLY, cgofuse.O_RDWR, cgofuse.O_RDONLY | cgofuse.O_TRUNC, cgofuse.O_RDONLY | cgofuse.O_APPEND} {
		if errc, _ := p.Open("/f", flags); errc != -cgofuse.EROFS {
			t.Errorf("Open(%#x) = %d, want EROFS", flags, errc)
		}
	}
	if errc, _ := p.Create("/f", cgofuse.O_RDWR|cgofuse.O_CREAT, 0o644); errc != -cgofuse.EROFS {
		t.Errorf("Create = %d, want EROFS", errc)
	}
	if inner.calls != 0 {
		t.Error("a write reached the adapter")
	}
	if errc, _ := p.Open("/f", cgofuse.O_RDONLY); errc != 0 {
		t.Errorf("a read-only open: %d", errc)
	}
}

func TestFileSecurity(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	sddl := fileSecurity(me)
	if want := "D:P(A;;FA;;;" + me.String() + ")(A;;FA;;;SY)"; sddl != want {
		t.Fatalf("fileSecurity = %s, want %s", sddl, want)
	}
	if _, err := windows.SecurityDescriptorFromString(sddl); err != nil {
		t.Fatalf("%s does not parse: %v", sddl, err)
	}
}

func TestWinFspVersion(t *testing.T) {
	for _, c := range []struct {
		version [3]uint16
		ok      bool
	}{
		{[3]uint16{2, 1, 25156}, false},
		{[3]uint16{2, 2, 26215}, false},
		{[3]uint16{2, 2, 26286}, true},
		{[3]uint16{2, 3, 1}, true},
		{[3]uint16{3, 0, 0}, true},
	} {
		if got := atLeast(c.version, minWinFsp); got != c.ok {
			t.Errorf("atLeast(%v) = %v, want %v", c.version, got, c.ok)
		}
	}
}

// With WinFsp installed (CI installs it), it is found, checked and loaded
// from its own directory.
func TestLoadWinFsp(t *testing.T) {
	if _, err := winfspInstallDir(); err != nil && os.Getenv("MOUNTABLE_TEST_WINFSP") == "" {
		t.Skip("WinFsp is not installed:", err)
	}
	if err := findAndLoadWinFsp(); err != nil {
		t.Fatal(err)
	}
}
