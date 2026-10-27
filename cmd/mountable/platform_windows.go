package main

// How a Windows mount point is checked, how the console's Ctrl-C and close
// reach the mount, and how `mountable unmount` reaches it: Windows has no
// call one process can use to unmount another's WinFsp volume, so each
// mount listens on a control socket (design §5).

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// controlTimeout bounds a control request that should answer at once.
	controlTimeout = 5 * time.Second
	// unmountTimeout is how long `mountable unmount` waits for the mount
	// to end: its own shutdown budget, then its abort.
	unmountTimeout = shutdownTimeout + abortTimeout + controlTimeout
	// consoleCloseBudget is how long the console handler holds off
	// Windows' end of the process after a close, logoff or shutdown;
	// Windows ends it about five seconds after the event.
	consoleCloseBudget = 4500 * time.Millisecond
	// maxSocketPath is the longest path an AF_UNIX address holds, with its
	// terminating zero.
	maxSocketPath = 107
)

// errNotMounted is a mount point no mount is listening for.
var errNotMounted = errors.New("not mounted")

// mountOwner: a Windows mount point has no owner to read; entries show as
// the mounting user's (WinFsp's uid=-1), and the filesystem owner's ids map
// to themselves.
func mountOwner(string) (owner, error) {
	return owner{uid: ownerID, gid: ownerID, localUID: ownerID, localGID: ownerID, ctime: time.Now()}, nil
}

// isMountPoint keeps canonicalDir from following a reparse point at the
// mount point itself: a mounted directory is one, whose target is WinFsp's
// volume (\Device\Volume{…}), which is no path to resolve.
func isMountPoint(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(p)
	return err == nil && attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// prepareMountPoint checks that WinFsp is there before a ticket is used,
// and says where the mount will not be seen.
func prepareMountPoint(string) error {
	if err := loadWinFsp(); err != nil {
		return err
	}
	if windows.GetCurrentProcessToken().IsElevated() {
		diagnose("this prompt is elevated: the mount is visible to elevated programs only, not to Explorer or an ordinary prompt")
	}
	if serviceContext() {
		diagnose("running as a service account or SYSTEM: WinFsp defines the drive for every logon session")
	}
	return nil
}

func inSessionZero() bool {
	var session uint32
	return windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) == nil && session == 0
}

// serviceContext is WinFsp's own test (FspServiceContextCheck) for when it
// defines a drive for every logon session: session 0, and SYSTEM or a
// member of SERVICE. An SSH session also runs in session 0, as neither.
func serviceContext() bool {
	if !inSessionZero() {
		return false
	}
	user, err := currentUserSID()
	if err != nil {
		return false
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err == nil && user.Equals(system) {
		return true
	}
	service, err := windows.CreateWellKnownSid(windows.WinServiceSid)
	if err != nil {
		return false
	}
	member, err := windows.Token(0).IsMember(service)
	return err == nil && member
}

// checkBeforeSession checks WinFsp and the mount point before `mountable
// mount FS_ID DIR` creates a session, which a machine without WinFsp would
// otherwise leave behind.
func checkBeforeSession(dir string) error {
	dir, err := canonicalDir(dir)
	if err != nil {
		return err
	}
	if err := loadWinFsp(); err != nil {
		return err
	}
	return checkMountDir(dir)
}

// checkMountDir checks the mount point before the ticket is exchanged: a
// free drive letter, or a directory that does not exist yet in an existing
// one on NTFS or ReFS, which hold the reparse point WinFsp mounts on.
// Network names and device paths are refused: WinFsp would make a network
// name visible to every logon session.
func checkMountDir(dir string) error {
	refuse := func(message, hint string) error {
		return &cliError{Code: "mount_failed", Message: message, Hint: hint}
	}
	if strings.HasPrefix(dir, `\\`) || strings.HasPrefix(dir, `//`) {
		return refuse(dir+": network names and device paths are not supported as mount points", `mount on a drive letter (X:) or a new directory`)
	}
	if letter, ok := driveLetter(dir); ok {
		drives, err := windows.GetLogicalDrives()
		if err != nil {
			return err
		}
		if drives&(1<<(letter[0]-'A')) != 0 {
			return refuse("drive "+letter+" is in use", "pick a free drive letter")
		}
		return nil
	}
	_, err := os.Lstat(dir)
	if err == nil {
		return refuse("mount point "+dir+" exists", "on Windows the mount creates the directory itself: give a path that does not exist yet, or a free drive letter (X:)")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return refuse(fmt.Sprintf("mount point %s: %v", dir, err), "")
	}
	parent := filepath.Dir(dir)
	if err := isDirectory(parent); err != nil {
		return refuse(fmt.Sprintf("the parent of %s: %v", dir, err), "create it first: mkdir "+parent)
	}
	if name, err := filesystemName(parent); err != nil || (name != "NTFS" && name != "ReFS") {
		return refuse(fmt.Sprintf("%s is not on NTFS or ReFS (%s)", parent, name+errString(err)), "mount on a drive letter (X:) or in a directory on an NTFS disk")
	}
	return nil
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// filesystemName is the filesystem of the volume path is on: "NTFS", ...
func filesystemName(path string) (string, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	volume := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(p, &volume[0], uint32(len(volume))); err != nil {
		return "", err
	}
	name := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumeInformation(&volume[0], nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(name), nil
}

// mountedPath is the path the mounted event reports: "X:\" for a drive,
// since "X:" alone means the drive's current directory.
func mountedPath(dir string) string { return mountRoot(dir) }

// controlPath is the mount point's control socket:
// %LOCALAPPDATA%\mountable\mounts\<hash>.sock (12 bytes of SHA-256, in
// hex), in directories only this
// user can open. canonicalDir has resolved the parent's links, case and
// short names; Windows names are case-insensitive, so the hash is of the
// upper-cased mount point.
func controlPath(dir string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	mounts := filepath.Join(base, "mountable", "mounts")
	if err := os.MkdirAll(mounts, 0o700); err != nil {
		return "", err
	}
	for _, d := range []string{filepath.Dir(mounts), mounts} {
		if err := checkPrivate(d); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(strings.ToUpper(dir)))
	path := filepath.Join(mounts, hex.EncodeToString(sum[:12])+".sock")
	if len(path) > maxSocketPath {
		return "", fmt.Errorf("the control socket's path %s is longer than Windows allows (%d bytes)", path, maxSocketPath)
	}
	return path, nil
}

// dialControl connects to the mount at dir. A socket nobody listens on
// (its mount died) is removed, and dir is then not mounted.
func dialControl(dir string) (net.Conn, error) {
	path, err := controlPath(dir)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", path, controlTimeout)
	if err == nil {
		return conn, nil
	}
	if _, statErr := os.Lstat(path); errors.Is(statErr, fs.ErrNotExist) {
		return nil, errNotMounted
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, windows.WSAECONNREFUSED) {
		os.Remove(path)
		return nil, errNotMounted
	}
	return nil, err
}

// mountProcess is a connection to a mount and a handle to the process
// serving it, taken from the kernel when connecting, so it cannot name
// another process later.
type mountProcess struct {
	conn    net.Conn
	process windows.Handle
}

// sioAFUnixGetPeerPID is SIO_AF_UNIX_GETPEERPID: the PID of an AF_UNIX
// socket's peer.
const sioAFUnixGetPeerPID = windows.IOC_OUT | windows.IOC_VENDOR | 256

func connectMount(dir string) (*mountProcess, error) {
	conn, err := dialControl(dir)
	if err != nil {
		return nil, err
	}
	process, err := peerProcess(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &mountProcess{conn: conn, process: process}, nil
}

// peerProcess opens the process at the other end of conn, checking that it
// is a mountable binary.
func peerProcess(conn net.Conn) (windows.Handle, error) {
	unix, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a Unix socket")
	}
	raw, err := unix.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid, n uint32
	var ioctlErr error
	if err := raw.Control(func(fd uintptr) {
		ioctlErr = windows.WSAIoctl(windows.Handle(fd), sioAFUnixGetPeerPID, nil, 0, (*byte)(unsafe.Pointer(&pid)), 4, &n, nil, 0)
	}); err != nil {
		return 0, err
	}
	if ioctlErr != nil {
		return 0, fmt.Errorf("finding the mounting process: %w", ioctlErr)
	}
	// Without the right to end it, which a mount started elevated does not
	// grant an ordinary prompt; end asks for that when it is needed. Held
	// open, the handle keeps the process ID from being reused.
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return 0, fmt.Errorf("opening the mounting process %d: %w", pid, err)
	}
	name := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(name))
	if err := windows.QueryFullProcessImageName(h, 0, &name[0], &size); err != nil {
		windows.CloseHandle(h)
		return 0, err
	}
	if image := filepath.Base(windows.UTF16ToString(name[:size])); !mountableImage(image) {
		windows.CloseHandle(h)
		return 0, fmt.Errorf("the control socket is served by %s, not mountable", image)
	}
	return h, nil
}

// mountableImage reports whether a process image is the CLI: under one of
// the names it ships as, or this binary's own.
func mountableImage(image string) bool {
	names := []string{"mountable.exe", "mountable-windows-amd64.exe", "mountable-windows-arm64.exe",
		"mountable-win32-x64.exe", "mountable-win32-arm64.exe"}
	if self, err := os.Executable(); err == nil {
		names = append(names, filepath.Base(self))
	}
	for _, n := range names {
		if strings.EqualFold(image, n) {
			return true
		}
	}
	return false
}

// ask sends one control line and reads the one-line answer within timeout.
func (m *mountProcess) ask(line string, timeout time.Duration) (string, error) {
	_ = m.conn.SetDeadline(time.Now().Add(timeout))
	if _, err := fmt.Fprintln(m.conn, line); err != nil {
		return "", err
	}
	answer, err := bufio.NewReader(m.conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("no answer from the mount: %w", err)
	}
	return strings.TrimSpace(answer), nil
}

func (m *mountProcess) close() {
	m.conn.Close()
	windows.CloseHandle(m.process)
}

// end ends the mounting process and waits for it to exit, which removes
// its volume. The handle held since connecting keeps its process ID from
// naming another process.
func (m *mountProcess) end() error {
	pid, err := windows.GetProcessId(m.process)
	if err != nil {
		return err
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return fmt.Errorf("ending the mounting process %d (a mount started from an elevated prompt is ended from one): %w", pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("ending the mounting process: %w", err)
	}
	if event, err := windows.WaitForSingleObject(m.process, uint32(abortTimeout/time.Millisecond)); err != nil || event != windows.WAIT_OBJECT_0 {
		return errors.New("the mounting process did not exit")
	}
	return nil
}

// The control socket and the console both end the mount by signalling the
// lifecycle; finished is closed once it has ended.
var console struct {
	mu       sync.Mutex
	signals  chan<- os.Signal
	finished <-chan struct{}
}

// serveControl listens on dir's control socket, refusing a mount point
// another mount already serves. An `unmount` request arrives as one
// signal, ending the mount the ordinary way; `abort` as two, the second of
// which aborts; both are answered once the mount has ended. The returned
// finish records how the mount ended, answers the waiting requests and
// removes the socket.
func serveControl(dir string, signals chan<- os.Signal) (func(error), error) {
	if conn, err := dialControl(dir); err == nil {
		conn.Close()
		return nil, &cliError{Code: "mount_failed", Message: dir + " is already mounted", Hint: "unmount it first: mountable unmount " + dir}
	} else if !errors.Is(err, errNotMounted) {
		return nil, err
	}
	path, err := controlPath(dir)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	c := &control{listener: listener, signals: signals, finished: make(chan struct{})}
	console.mu.Lock()
	console.signals, console.finished = signals, c.finished
	console.mu.Unlock()
	go c.serve()
	return c.finish, nil
}

type control struct {
	listener net.Listener
	signals  chan<- os.Signal
	finished chan struct{}
	result   error
	once     sync.Once

	mu      sync.Mutex
	closed  bool // finish has begun: no more answers are counted
	answers sync.WaitGroup
}

func (c *control) serve() {
	for {
		conn, err := c.listener.Accept()
		if err != nil {
			return
		}
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			conn.Close()
			return
		}
		c.answers.Add(1)
		c.mu.Unlock()
		go c.handle(conn)
	}
}

func (c *control) handle(conn net.Conn) {
	defer c.answers.Done()
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(controlTimeout))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch strings.TrimSpace(line) {
	case "unmount":
		sendSignal(c.signals, syscall.SIGTERM, 1)
	case "abort":
		sendSignal(c.signals, syscall.SIGTERM, 2)
	default:
		fmt.Fprintln(conn, "error: unknown request")
		return
	}
	<-c.finished
	// A mount interrupted while it started has ended as asked.
	if c.result != nil && !errors.Is(c.result, errInterrupted) {
		fmt.Fprintln(conn, "error: "+oneLine(c.result))
		return
	}
	fmt.Fprintln(conn, "ok")
}

// sendSignal delivers n requests to end the mount; one already waiting is
// enough.
func sendSignal(signals chan<- os.Signal, sig os.Signal, n int) {
	for range n {
		select {
		case signals <- sig:
		default:
		}
	}
}

func (c *control) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.listener.Close() // also removes the socket file
		c.result = err
		close(c.finished)
		waited := make(chan struct{})
		go func() {
			c.answers.Wait()
			close(waited)
		}()
		select {
		case <-waited:
		case <-time.After(time.Second):
		}
	})
}

// Console control events (wincon.h).
const (
	ctrlC        = 0
	ctrlBreak    = 1
	ctrlClose    = 2
	ctrlLogoff   = 5
	ctrlShutdown = 6
)

var (
	setConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")
	// Two callbacks for the one handler, so that one of them is always
	// registered while the other moves on top (keepConsole).
	consoleCallbacks = [2]uintptr{windows.NewCallback(consoleHandler), windows.NewCallback(consoleHandler)}
)

// keepConsole keeps this process's console handler first until done is
// closed. WinFsp registers its own once per process, from a thread it
// starts after the mount has started, at a moment this process cannot
// see; that one would delete the volume on Ctrl-C or a close, without the
// lifecycle's clean stop. Windows asks the handler registered last first,
// so this one is registered again on top, often at first and then every
// few seconds: the other of the two callbacks is added before the current
// one is removed, so there is never a moment without one on top.
func keepConsole(done <-chan struct{}) {
	current := 0
	if r, _, err := setConsoleCtrlHandler.Call(consoleCallbacks[current], 1); r == 0 {
		diagnose("taking the console's Ctrl-C: %v", err)
		return
	}
	go func() {
		start := time.Now()
		for {
			interval := 3 * time.Second
			if time.Since(start) < 10*time.Second {
				interval = 100 * time.Millisecond
			}
			select {
			case <-done:
				return
			case <-time.After(interval):
			}
			next := 1 - current
			if r, _, _ := setConsoleCtrlHandler.Call(consoleCallbacks[next], 1); r == 0 {
				continue
			}
			setConsoleCtrlHandler.Call(consoleCallbacks[current], 0)
			current = next
		}
	}()
}

// consoleHandler turns Ctrl-C and Break into SIGINT and a close, logoff or
// shutdown into SIGTERM. Windows ends the process once the handler returns
// from one of the latter, so it waits for the mount to end first, within
// what Windows allows. A service ignores logoffs, which are other users'.
func consoleHandler(event uint32) uintptr {
	console.mu.Lock()
	signals, finished := console.signals, console.finished
	console.mu.Unlock()
	if signals == nil {
		return 0
	}
	switch event {
	case ctrlC, ctrlBreak:
		sendSignal(signals, syscall.SIGINT, 1)
		return 1
	case ctrlLogoff:
		if serviceContext() {
			return 1
		}
		fallthrough
	case ctrlClose, ctrlShutdown:
		sendSignal(signals, syscall.SIGTERM, 1)
		select {
		case <-finished:
		case <-time.After(consoleCloseBudget):
		}
		return 1
	}
	return 0
}

// clearStaleMount: a dead mount's volume is gone with its process; only its
// socket can be left, which dialControl removes.
func clearStaleMount(string) (bool, error) { return false, nil }

// unmounting holds, per mount point, the process cleanUnmount asked, for
// abortMount if it does not end cleanly.
var unmounting sync.Map

// cleanUnmount asks the mount at dir to unmount and waits until it has
// ended.
func cleanUnmount(dir string) error {
	m, err := connectMount(dir)
	if err != nil {
		return err
	}
	answer, err := m.ask("unmount", unmountTimeout)
	m.conn.Close()
	if err == nil && answer == "ok" {
		windows.CloseHandle(m.process)
		return nil
	}
	unmounting.Store(dir, m)
	if err != nil {
		return err
	}
	return errors.New(strings.TrimPrefix(answer, "error: "))
}

func notMounted(err error) bool { return errors.Is(err, errNotMounted) }

// abortMount ends the mount at dir at once: it asks the mount to abort,
// and if that does not answer, ends the mounting process.
func abortMount(dir string) error {
	var m *mountProcess
	if v, ok := unmounting.LoadAndDelete(dir); ok {
		m = v.(*mountProcess)
		defer windows.CloseHandle(m.process)
	}
	asked, err := connectMount(dir)
	if notMounted(err) {
		return nil
	}
	if err == nil {
		defer asked.close()
		// Any answer means the mount has ended; what it reports is why.
		if _, err := asked.ask("abort", abortTimeout+controlTimeout); err == nil {
			return nil
		}
		if m == nil {
			m = asked
		}
	}
	if m == nil {
		return fmt.Errorf("the mount does not answer: %w", err)
	}
	return m.end()
}
