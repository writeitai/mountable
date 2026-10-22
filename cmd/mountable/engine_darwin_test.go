package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// The exact option list of the design (§2), with ro for read-only.
func TestNFSOptions(t *testing.T) {
	want := "vers=3,proto=ticotsord,port=/s/nfs.sock,mountport=/s/nfs.sock," +
		"opaque_auth,noasync,nonfc,noresvport," +
		"locallocks,noquota,noacl," +
		"actimeo=1,nonegnamecache," +
		"rsize=1048576,wsize=1048576,dsize=65536," +
		"hard,nodev,nosuid,intr,mutejukebox,dumbtimer,timeo=300,deadtimeout=30"
	if got := nfsOptions("/s/nfs.sock", false); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got := nfsOptions("/s/nfs.sock", true); got != want+",ro" {
		t.Fatalf("read-only: %s", got)
	}
}

// nfsstat -m as macOS 26.7.1 printed it for a mount by this CLI.
const nfsstatOutput = `/Users/jpuc/mnt from </Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock>:/
  -- Original mount options:
     General mount flags: 0x19 ro,nodev,nosuid
     NFS parameters: vers=3,ticotsord,port=/Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock,mountport=/Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock,hard,intr,noresvport,nonegnamecache,noacl,locallocks,noquota,rsize=1048576,wsize=1048576,dsize=65536,dumbtimer,timeo=300,acregmin=1,acregmax=1,acdirmin=1,acdirmax=1,acrootdirmin=1,acrootdirmax=1,deadtimeout=30,mutejukebox,nonfc
     File system locations:
       / @ </Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock>
  -- Current mount parameters:
     General mount flags: 0x4000019 ro,nodev,nosuid,multilabel
     NFS parameters: vers=3,ticotsord,port=/Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock,mountport=/Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock,nomntudp,hard,intr,noresvport,nonegnamecache,callumnt,locallocks,noquota,rsize=1048576,wsize=1048576,readahead=16,dsize=65536,rdirplus,dumbtimer,timeo=300,maxgroups=16,acregmin=1,acregmax=1,acdirmin=1,acdirmax=1,acrootdirmin=1,acrootdirmax=1,deadtimeout=30,mutejukebox,nonfc,sec=sys,accesscache=3
     File system locations:
       / @ </Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock>
     Status flags: 0x0
`

func TestCheckMountParameters(t *testing.T) {
	if err := checkMountParameters(nfsstatOutput); err != nil {
		t.Fatal(err)
	}
	// The current parameters decide, not the options asked for.
	for from, to := range map[string]string{
		"nomntudp,hard,": "nomntudp,soft,",
		"vers=3,ticotsord,port=/Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock,mountport=/Users/jpuc/Library/Caches/mountable/mount-1/nfs/nfs.sock,nomntudp": "vers=3,tcp,port=1,mountport=1,nomntudp",
		"rsize=1048576,wsize=1048576,readahead": "rsize=32768,wsize=1048576,readahead",
		"readahead=16,dsize=65536":              "readahead=16,dsize=8192",
	} {
		changed := strings.Replace(nfsstatOutput, from, to, 1)
		if changed == nfsstatOutput {
			t.Fatalf("fixture does not contain %q", from)
		}
		if err := checkMountParameters(changed); err == nil {
			t.Errorf("accepted %s", to)
		}
	}
	if err := checkMountParameters(""); err == nil {
		t.Error("accepted no output")
	}
}

func TestCheckMountFlags(t *testing.T) {
	m := &unix.Statfs_t{Flags: unix.MNT_RDONLY | unix.MNT_NOSUID | unix.MNT_NODEV}
	copy(m.Fstypename[:], "nfs")
	if err := checkMountFlags(m, true); err != nil {
		t.Fatal(err)
	}
	for name, flags := range map[string]uint32{
		"async":        unix.MNT_RDONLY | unix.MNT_NOSUID | unix.MNT_NODEV | unix.MNT_ASYNC,
		"read-write":   unix.MNT_NOSUID | unix.MNT_NODEV,
		"setuid":       unix.MNT_RDONLY | unix.MNT_NODEV,
		"device files": unix.MNT_RDONLY | unix.MNT_NOSUID,
	} {
		bad := *m
		bad.Flags = flags
		if err := checkMountFlags(&bad, true); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestMountableSocket(t *testing.T) {
	entry := func(fstype, from string, owner int) *unix.Statfs_t {
		m := &unix.Statfs_t{Owner: uint32(owner)}
		copy(m.Fstypename[:], fstype)
		copy(m.Mntfromname[:], from)
		return m
	}
	me := os.Getuid()
	if socket, ok := mountableSocket(entry("nfs", "</Users/u/Library/Caches/mountable/mount-1/nfs/nfs.sock>:/", me)); !ok || socket != "/Users/u/Library/Caches/mountable/mount-1/nfs/nfs.sock" {
		t.Fatalf("%q %v", socket, ok)
	}
	if _, ok := mountableSocket(entry("nfs", fmt.Sprintf("</tmp/mountable-%d/nfs-1/nfs.sock>:/", me), me)); !ok {
		t.Fatal("the /tmp socket directory is not recognised")
	}
	for _, m := range []*unix.Statfs_t{
		entry("nfs", "</Users/u/Library/Caches/mountable/mount-1/nfs/nfs.sock>:/", me+1),
		entry("nfs", "server:/export", me),
		entry("nfs", "</var/run/other.sock>:/", me),
		entry("apfs", "/dev/disk3s1", me),
	} {
		if _, ok := mountableSocket(m); ok {
			t.Errorf("%s from %s taken for a Mountable mount", cString(m.Fstypename[:]), cString(m.Mntfromname[:]))
		}
	}
}

// The socket's directory is private and the socket 0600.
func TestSocketPermissions(t *testing.T) {
	cache, err := os.MkdirTemp("", "mnt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cache)
	dir, err := makeSocketDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(cache, "nfs") {
		t.Fatalf("socket directory %s", dir)
	}
	l, err := listenSocket(filepath.Join(dir, socketName))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, socketName): 0o600} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s: mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

// A socket path too long for sockaddr_un moves under /tmp/mountable-<uid>.
func TestLongSocketPathsMoveToTmp(t *testing.T) {
	base := fmt.Sprintf("/tmp/mountable-%d", os.Getuid())
	if _, err := os.Lstat(base); os.IsNotExist(err) {
		defer os.Remove(base) // created by this test
	}
	parent := filepath.Join(t.TempDir(), "mountable")
	cache := filepath.Join(parent, strings.Repeat("x", 100))
	for _, dir := range []string{parent, cache} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := makeSocketDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if filepath.Dir(dir) != base || !socketPathFits(filepath.Join(dir, socketName)) {
		t.Fatalf("socket directory %s", dir)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", info.Mode(), err)
	}
	if socketPathFits("/a,b/nfs.sock") || socketPathFits("/<a>/nfs.sock") || socketPathFits("/"+strings.Repeat("x", 103)) {
		t.Fatal("an unusable socket path fits")
	}
}

func TestCheckPrivate(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(open); err == nil {
		t.Fatal("a 0755 directory passed")
	}
	link := filepath.Join(dir, "link")
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivate(link); err == nil {
		t.Fatal("a symlink passed")
	}
	if err := checkPrivate(private); err != nil {
		t.Fatal(err)
	}
}

// A reused cache directory open to others is refused before the socket goes
// into it.
func TestSocketDirNeedsPrivateParents(t *testing.T) {
	base := filepath.Join(t.TempDir(), "mountable")
	if err := os.Mkdir(base, 0o755); err != nil {
		t.Fatal(err)
	}
	cache, err := os.MkdirTemp(base, "mount-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := makeSocketDir(cache); err == nil {
		t.Fatal("a socket went under a 0755 directory")
	}
}

// Unmounting a directory nothing is mounted on, as after macOS dropped a
// dead mount, says so; nothing was aborted, so nothing may be lost.
func TestUnmountOfADirectoryNotMounted(t *testing.T) {
	err := unmount(t.TempDir())
	if err == nil || !strings.HasSuffix(err.Error(), "is not mounted") {
		t.Fatalf("unmount: %v", err)
	}
}
