package main

// Qualification of the Windows mount against a real local cell (a SeaweedFS
// server built from the engine fork, without the gateway): read-write
// through WinFsp, names, git, another local user, and the console. CI
// starts the cell and sets MOUNTABLE_TEST_CELL to its filer's address; the
// gateway and sessions are covered by the Linux and macOS tests. The rows
// are those of the fsit design's Windows mounts §7.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"golang.org/x/sys/windows"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// cell is one filesystem root in the local cell, mounted by the tests.
type cell struct {
	t     *testing.T
	filer string // the filer's HTTP host:port
	root  string // the filesystem's path in the cell
}

// newCell is a fresh filesystem root in the cell, or skips without one.
func newCell(t *testing.T) *cell {
	t.Helper()
	filer := os.Getenv("MOUNTABLE_TEST_CELL")
	if filer == "" {
		t.Skip("no local cell (MOUNTABLE_TEST_CELL)")
	}
	requireWinFsp(t)
	return &cell{t: t, filer: filer, root: "/c/" + randomHex(6)}
}

// mount mounts the cell's root at dir with the engine options the CLI uses.
func (c *cell) mount(dir string, readOnly bool) *engine {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config := mountConfig{gateway: c.filer, root: c.root, dir: dir, readOnly: readOnly, cacheDir: engineCacheDir(c.t)}
	own, err := mountOwner(dir)
	if err != nil {
		c.t.Fatal(err)
	}
	mapper, err := meta_cache.NewUidGidMapper("1000:1000", "1000:1000")
	if err != nil {
		c.t.Fatal(err)
	}
	wfs := newWFS([]pb.ServerAddress{pb.ServerAddress(c.filer)}, grpc.WithTransportCredentials(insecure.NewCredentials()), config, own, mapper, false)
	parent, name := util.FullPath(c.root).DirAndName()
	if err := filer_pb.Mkdir(ctx, wfs, parent, name, nil); err != nil {
		c.t.Fatalf("creating the filesystem root: %v", err)
	}
	if err := wfs.StartBackgroundTasks(); err != nil {
		c.t.Fatal(err)
	}
	e, err := attach(ctx, wfs, config)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() {
		select {
		case <-e.served():
		default:
			_ = e.abort()
		}
	})
	return e
}

// stop unmounts cleanly and commits, as the lifecycle's clean stop does.
func (c *cell) stop(e *engine) {
	c.t.Helper()
	if err := e.unmount(); err != nil {
		c.t.Fatalf("unmount: %v", err)
	}
	e.flush()
}

func (c *cell) url(name string) string {
	return "http://" + c.filer + (&url.URL{Path: c.root + "/" + name}).EscapedPath()
}

// stored is a file's content in the cell, read past the mount.
func (c *cell) stored(name string) (string, bool) {
	c.t.Helper()
	resp, err := http.Get(c.url(name))
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatal(err)
	}
	return string(body), resp.StatusCode == http.StatusOK
}

// put writes a file into the cell past the mount, as another mount or the
// files API does.
func (c *cell) put(name, content string) {
	c.t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", path.Base(name))
	if err == nil {
		_, err = part.Write([]byte(content))
	}
	if err == nil {
		err = form.Close()
	}
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := http.Post(c.url(name), form.FormDataContentType(), &body)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		c.t.Fatalf("putting %s: %s", name, resp.Status)
	}
}

// remove deletes a file in the cell past the mount.
func (c *cell) remove(name string) {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodDelete, c.url(name), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		c.t.Fatalf("removing %s: %s", name, resp.Status)
	}
}

// mode is an entry's stored permission bits.
func (c *cell) mode(name string) os.FileMode {
	c.t.Helper()
	var entry *filer_pb.Entry
	err := pb.WithGrpcFilerClient(false, 0, pb.ServerAddress(c.filer), grpc.WithTransportCredentials(insecure.NewCredentials()), func(client filer_pb.SeaweedFilerClient) error {
		dir, base := util.FullPath(c.root + "/" + name).DirAndName()
		resp, err := client.LookupDirectoryEntry(context.Background(), &filer_pb.LookupDirectoryEntryRequest{Directory: dir, Name: base})
		if err == nil {
			entry = resp.Entry
		}
		return err
	})
	if err != nil {
		c.t.Fatalf("looking up %s: %v", name, err)
	}
	return os.FileMode(entry.Attributes.FileMode).Perm()
}

// eventually waits up to 15 seconds for check to hold and returns how long
// that took.
func eventually(t *testing.T, what string, check func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !check() {
		if time.Since(start) > 15*time.Second {
			t.Fatalf("%s: not within 15s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return time.Since(start)
}

// Writes: durable when FlushFileBuffers returns; committed by a clean stop
// after close, and for a file still open for writing; directories,
// renames and deletes reach the cell; modes of Windows creates.
func TestCellWrites(t *testing.T) {
	c := newCell(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	e := c.mount(dir, false)

	synced, err := os.Create(filepath.Join(dir, "synced.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synced.WriteString("flushed before close"); err != nil {
		t.Fatal(err)
	}
	if err := synced.Sync(); err != nil {
		t.Fatalf("FlushFileBuffers: %v", err)
	}
	if got, ok := c.stored("synced.txt"); !ok || got != "flushed before close" {
		t.Errorf("after FlushFileBuffers the cell holds %q (%v)", got, ok)
	}
	synced.Close()

	if err := os.WriteFile(filepath.Join(dir, "closed.txt"), []byte("written and closed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "d", "old"), []byte("renamed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "d", "old"), filepath.Join(dir, "d", "new")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "gone")); err != nil {
		t.Fatal(err)
	}
	// Saved the way editors and git save: written under a temporary name,
	// closed, renamed over the file, and the temporary name used again at
	// once. WinFsp commits a closed file at its Close, which can come after
	// the rename.
	for i := range 10 {
		tmp := filepath.Join(dir, "saved.tmp")
		if err := os.WriteFile(tmp, []byte(fmt.Sprintf("version %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "saved.txt")); err != nil {
			t.Fatal(err)
		}
	}
	held, err := os.Create(filepath.Join(dir, "held.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.WriteString("still open at the unmount"); err != nil {
		t.Fatal(err)
	}

	c.stop(e)
	held.Close() // its volume is gone

	for name, want := range map[string]string{
		"closed.txt": "written and closed",
		"d/new":      "renamed",
		"held.txt":   "still open at the unmount",
		"saved.txt":  "version 9",
	} {
		if got, ok := c.stored(name); !ok || got != want {
			t.Errorf("after the clean stop, %s holds %q (%v), want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"d/old", "gone"} {
		if _, ok := c.stored(name); ok {
			t.Errorf("%s is still stored", name)
		}
	}
	if m := c.mode("closed.txt"); m != 0o644 {
		t.Errorf("a file created from Windows is stored %o, want 644", m)
	}
	if m := c.mode("d"); m != 0o755 {
		t.Errorf("a directory created from Windows is stored %o, want 755", m)
	}
}

// A file written and closed in the mount reaches the cell, where other
// mounts read it, while the mount goes on: WinFsp commits it at its Close,
// which Windows sends once it no longer holds the file. The test records
// how long that takes.
func TestCellClosedFilesReachTheCell(t *testing.T) {
	c := newCell(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	e := c.mount(dir, false)
	defer c.stop(e)
	// Well under the engine's periodic metadata flush (two minutes), which
	// would commit the files without any Close.
	const limit = 30 * time.Second
	closed := map[string]time.Time{}
	for i := range 5 {
		name := fmt.Sprintf("closed%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		closed[name] = time.Now()
	}
	for len(closed) > 0 {
		for name, at := range closed {
			if got, ok := c.stored(name); ok && got == name {
				t.Logf("%s reached the cell %v after its close", name, time.Since(at).Round(50*time.Millisecond))
				delete(closed, name)
			} else if time.Since(at) > limit {
				t.Fatalf("%s did not reach the cell within %v of its close", name, limit)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Changes made past the mount (another mount, the files API) appear in it,
// including in a file whose data Windows has cached; the test records how
// soon.
func TestCellRemoteChanges(t *testing.T) {
	c := newCell(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	c.mount(dir, false)
	path := filepath.Join(dir, "remote.txt")
	c.put("remote.txt", "first")
	t.Logf("a remote create appeared after %v", eventually(t, "a remote create", func() bool {
		got, err := os.ReadFile(path)
		return err == nil && string(got) == "first"
	}))
	c.put("remote.txt", "second, longer")
	t.Logf("a remote change to a cached file appeared after %v", eventually(t, "a remote change to a cached file", func() bool {
		got, err := os.ReadFile(path)
		return err == nil && string(got) == "second, longer"
	}))

	// A move between directories as the Autoboard ticket sync makes one: the
	// file written at its new path, then deleted from the old, with both
	// directories listed (and so cached) before.
	c.put("todo/t.md", "ticket")
	c.put("done/other.md", "another ticket")
	listed := func(sub string) (bool, error) {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		return slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == "t.md" }), err
	}
	eventually(t, "remote creates in two directories", func() bool {
		inTodo, err := listed("todo")
		_, errDone := listed("done")
		return err == nil && errDone == nil && inTodo
	})
	c.put("done/t.md", "ticket, done")
	c.remove("todo/t.md")
	t.Logf("a remote move appeared in both listings after %v (directory listings are cached for up to 1 s)", eventually(t, "a remote move", func() bool {
		got, err := os.ReadFile(filepath.Join(dir, "done", "t.md"))
		inDone, errDone := listed("done")
		inTodo, errTodo := listed("todo")
		return err == nil && string(got) == "ticket, done" && errDone == nil && inDone && errTodo == nil && !inTodo
	}))
}

// An ACL edit from Windows never adds an execute bit or changes group and
// other.
func TestCellChmodKeepsStoredBits(t *testing.T) {
	c := newCell(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	c.mount(dir, false)
	path := filepath.Join(dir, "f")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("icacls", path, "/grant", "*"+me.String()+":(F)").CombinedOutput(); err != nil {
		t.Logf("icacls: %v: %s", err, out) // recorded; the stored bits are what matters
	}
	if m := c.mode("f"); m&0o111 != 0 || m&0o077 != 0o044 {
		t.Errorf("after an ACL edit the file is stored %o, want 644", m)
	}
}

// Names created on Linux: characters Windows forbids are shown as
// private-use characters and round-trip, a backslash is part of a name and
// never a separator, and case collisions are both listed.
func TestCellNames(t *testing.T) {
	c := newCell(t)
	c.put(`a\b`, "the one with a backslash")
	c.put("a/b", "the one in directory a")
	c.put("c:d", "a colon")
	c.put("Readme", "lower")
	c.put("README", "upper")
	dir := filepath.Join(t.TempDir(), "mnt")
	c.mount(dir, false)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	for _, want := range []string{"a\uf05cb", "a", "c\uf03ad", "Readme", "README"} {
		if !slices.Contains(names, want) {
			t.Errorf("%q is not listed; the root lists %q", want, names)
		}
	}
	for name, want := range map[string]string{
		"a\uf05cb": "the one with a backslash",
		"c\uf03ad": "a colon",
		"Readme":   "lower",
		"README":   "upper",
	} {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("reading %q: %q, %v; want %q", name, got, err, want)
		}
	}
	if err := os.Rename(filepath.Join(dir, "a\uf05cb"), filepath.Join(dir, "renamed")); err != nil {
		t.Fatalf("renaming the backslash name: %v", err)
	}
	if got, ok := c.stored("a/b"); !ok || got != "the one in directory a" {
		t.Errorf("renaming a\\b touched a/b: %q, %v", got, ok)
	}
	if got, ok := c.stored("renamed"); !ok || got != "the one with a backslash" {
		t.Errorf("the renamed file holds %q, %v", got, ok)
	}
}

// git works in the mount: init, commit, gc and fsck.
func TestCellGit(t *testing.T) {
	c := newCell(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	dir := filepath.Join(t.TempDir(), "mnt")
	e := c.mount(dir, false)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=q", "GIT_AUTHOR_EMAIL=q@example.com",
			"GIT_COMMITTER_NAME=q", "GIT_COMMITTER_EMAIL=q@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q", "-b", "main")
	for i := range 20 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), bytes.Repeat([]byte{'a' + byte(i)}, 1000*(i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		run("add", ".")
		run("commit", "-q", "-m", fmt.Sprintf("commit %d", i))
	}
	run("gc", "-q")
	run("fsck", "--full")
	run("status", "--porcelain")
	c.stop(e)

	// What the cell stored is a whole repository: mounted again, it lists
	// every file in HEAD with nothing to commit.
	dir = filepath.Join(t.TempDir(), "again")
	c.mount(dir, false)
	out, err := exec.Command(git, "-C", dir, "ls-tree", "-r", "--name-only", "HEAD").Output()
	if err != nil || strings.Count(string(out), "\n") != 20 {
		t.Errorf("the stored repository's HEAD lists %q (%v), want 20 files", out, err)
	}
	if out, err := exec.Command(git, "-C", dir, "status", "--porcelain").Output(); err != nil || len(out) != 0 {
		t.Errorf("the stored repository has changes %q (%v): its index was not stored", out, err)
	}
	if out, err := exec.Command(git, "-C", dir, "fsck", "--full").CombinedOutput(); err != nil {
		t.Errorf("git fsck of the stored repository: %v\n%s", err, out)
	}
}

// Another local user, logged on at the same time, can open nothing in the
// mount: not through a directory mount point they can otherwise reach, and
// not through the volume's device path.
func TestCellOtherUserIsRefused(t *testing.T) {
	c := newCell(t)
	user, password := "mqual"+randomHex(2), "Q1!"+randomHex(4) // net user asks before longer than 14
	if out, err := exec.Command("net", "user", user, password, "/add").CombinedOutput(); err != nil {
		t.Skipf("cannot create a local user: %v: %s", err, out)
	}
	t.Cleanup(func() { exec.Command("net", "user", user, "/delete").Run() })

	// A directory every local user can reach, unlike the test's own.
	base, err := os.MkdirTemp(os.Getenv("SystemDrive")+`\`, "mountable-qual-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	if out, err := exec.Command("icacls", base, "/grant", "*S-1-5-32-545:(OI)(CI)RX").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	control := filepath.Join(base, "control.txt")
	if err := os.WriteFile(control, []byte("anyone may read this"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "mnt")
	c.mount(dir, false)
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("only the mounting user"), 0o644); err != nil {
		t.Fatal(err)
	}

	if code := runAs(t, user, password, `type "`+control+`"`); code != 0 {
		t.Fatalf("the other user cannot read an ordinary file (exit %d): the test does not work", code)
	}
	for what, command := range map[string]string{
		"read a file":     `type "` + secret + `"`,
		"list the mount":  `dir "` + dir + `"`,
		"stat a file":     `dir "` + secret + `"`,
		"read via device": `type "` + devicePath(t, dir) + `\secret.txt"`,
	} {
		if code := runAs(t, user, password, command); code == 0 {
			t.Errorf("the other user could %s", what)
		}
	}
}

// devicePath is the mounted volume's device path, \\?\GLOBALROOT\Device\...,
// from the directory's reparse point target.
func devicePath(t *testing.T, dir string) string {
	t.Helper()
	target, err := os.Readlink(dir)
	if err != nil {
		t.Skipf("reading the mount point's target: %v", err)
	}
	target = strings.TrimPrefix(target, `\??\`)
	target = strings.TrimSuffix(target, `\`)
	if !strings.HasPrefix(target, `\Device\`) && !strings.HasPrefix(target, `Volume{`) {
		t.Logf("mount point target %q", target)
	}
	if strings.HasPrefix(target, `\Device\`) {
		return `\\?\GLOBALROOT` + target
	}
	return `\\?\` + target
}

// runAs runs cmd.exe /c command as another local user and returns its exit
// code.
func runAs(t *testing.T, user, password, command string) uint32 {
	t.Helper()
	proc := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateProcessWithLogonW")
	u, _ := windows.UTF16PtrFromString(user)
	domain, _ := windows.UTF16PtrFromString(".")
	pw, _ := windows.UTF16PtrFromString(password)
	cmdline, _ := windows.UTF16FromString(`cmd.exe /c ` + command + ` >nul 2>nul`)
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	const logonWithProfile = 1
	r, _, err := proc.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(domain)), uintptr(unsafe.Pointer(pw)),
		logonWithProfile, 0, uintptr(unsafe.Pointer(&cmdline[0])), windows.CREATE_NO_WINDOW, 0, 0,
		uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&pi)))
	if r == 0 {
		t.Skipf("cannot run a process as %s: %v", user, err)
	}
	defer windows.CloseHandle(pi.Process)
	defer windows.CloseHandle(pi.Thread)
	if event, err := windows.WaitForSingleObject(pi.Process, 60_000); err != nil || event != windows.WAIT_OBJECT_0 {
		t.Fatalf("the other user's process did not finish")
	}
	var code uint32
	if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
		t.Fatal(err)
	}
	return code
}

// Ctrl-Break in the mount's console reaches the CLI's handler, not
// WinFsp's: the lifecycle gets its signal and stops cleanly, committing
// what was written. Run just after the mount (past the first re-registration
// on top, every 100 ms), and some seconds later.
func TestCellConsoleBreak(t *testing.T) {
	c := newCell(t)
	for _, delay := range []string{"500ms", "2s", "15s"} {
		t.Run(delay, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "mnt")
			cmd := exec.Command(os.Args[0], "-test.run=^TestCellConsoleHelper$", "-test.v")
			cmd.Env = append(os.Environ(), "MOUNTABLE_TEST_CONSOLE=1", "MOUNTABLE_TEST_CONSOLE_DIR="+dir,
				"MOUNTABLE_TEST_CONSOLE_ROOT="+c.root, "MOUNTABLE_TEST_CONSOLE_DELAY="+delay,
				"LocalAppData="+shortTempDir(t))
			// A console of its own, so the break reaches it alone.
			cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP}
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "RESULT signal clean") {
				t.Fatalf("the helper: %v\n%s", err, out)
			}
			if got, ok := c.stored("written.txt"); !ok || got != "before the break" {
				t.Errorf("after the break, the cell holds %q (%v)", got, ok)
			}
		})
	}
}

// TestCellConsoleHelper is TestCellConsoleBreak's mounting process.
func TestCellConsoleHelper(t *testing.T) {
	if os.Getenv("MOUNTABLE_TEST_CONSOLE") == "" {
		t.Skip("run by TestCellConsoleBreak")
	}
	c := newCell(t)
	c.root = os.Getenv("MOUNTABLE_TEST_CONSOLE_ROOT")
	dir := os.Getenv("MOUNTABLE_TEST_CONSOLE_DIR")
	delay, err := time.ParseDuration(os.Getenv("MOUNTABLE_TEST_CONSOLE_DELAY"))
	if err != nil {
		t.Fatal(err)
	}
	signals := make(chan os.Signal, 2)
	finish, err := serveControl(dir, signals)
	if err != nil {
		t.Fatal(err)
	}
	defer finish(nil)
	e := c.mount(dir, false)
	if err := os.WriteFile(filepath.Join(dir, "written.txt"), []byte("before the break"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(delay)
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, 0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signals:
		c.stop(e)
		fmt.Println("RESULT signal clean")
	case <-e.served():
		fmt.Println("RESULT winfsp-first: WinFsp's handler removed the volume")
	case <-time.After(30 * time.Second):
		fmt.Println("RESULT nothing arrived")
	}
}
