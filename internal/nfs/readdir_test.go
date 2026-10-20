package nfs

import (
	"fmt"
	"syscall"
	"testing"
)

type page struct {
	verf    uint64
	entries []pageEntry
	eof     bool
	size    int
}

type pageEntry struct {
	fileid, cookie uint64
	name           string
	attr           *fattr
	fh             []byte
}

// readDirPlus reads one READDIRPLUS page.
func (c *testClient) readDirPlus(dir []byte, cookie, verf uint64, dirCount, maxCount uint32) (uint32, page) {
	c.t.Helper()
	r := c.call(procReadDirPlus, wire{}.opaque(dir).u64(cookie).u64(verf).u32(dirCount).u32(maxCount))
	size := len(r.b)
	status := r.u32()
	if _, ok := r.postOpAttr(); !ok {
		c.t.Fatal("no directory attributes")
	}
	if status != 0 {
		return status, page{}
	}
	p := page{verf: r.u64(), size: size}
	for r.u32() == 1 {
		e := pageEntry{fileid: r.u64(), name: string(r.opaque()), cookie: r.u64()}
		if a, ok := r.postOpAttr(); ok {
			e.attr = &a
		}
		if r.u32() == 1 {
			e.fh = r.opaque()
		}
		p.entries = append(p.entries, e)
	}
	p.eof = r.u32() == 1
	if len(r.b) != 0 {
		c.t.Fatalf("%d bytes after the page", len(r.b))
	}
	return status, p
}

func wideEngine(n int) *fakeEngine {
	f := newFakeEngine()
	f.add("wide", 5000, syscall.S_IFDIR|0o755, "")
	for i := range n {
		f.add(fmt.Sprintf("wide/file-%03d", i), uint64(6000+i), syscall.S_IFREG|0o644, "x")
	}
	return f
}

// A directory read in small pages yields every entry once, in cookie order,
// each with attributes and a handle, within both budgets.
func TestReadDirPlusPages(t *testing.T) {
	f := wideEngine(50)
	_, c := startServer(t, f)
	root := c.root()
	dir, dirAttr := c.lookup(root, "wide")
	const maxCount = 2048
	var cookie, verf uint64
	seen := map[string]bool{}
	var pages int
	for {
		status, p := c.readDirPlus(dir, cookie, verf, 1024, maxCount)
		if status != 0 {
			t.Fatalf("page %d: status %d", pages, status)
		}
		if p.size > maxCount {
			t.Fatalf("page of %d bytes, maxcount %d", p.size, maxCount)
		}
		if pages > 0 && p.verf != verf {
			t.Fatal("the verifier changed between pages")
		}
		pages++
		verf = p.verf
		for _, e := range p.entries {
			if e.cookie <= cookie {
				t.Fatalf("cookie %d after %d", e.cookie, cookie)
			}
			cookie = e.cookie
			if seen[e.name] {
				t.Fatalf("%s listed twice", e.name)
			}
			seen[e.name] = true
			if e.attr == nil || len(e.fh) != 48 || e.fileid == 0 || e.attr.fileid != e.fileid {
				t.Fatalf("entry %+v", e)
			}
			switch e.name {
			case ".":
				if e.fileid != dirAttr.fileid {
					t.Fatal(`"." is not the directory`)
				}
			case "..":
				if e.fileid != rootInode {
					t.Fatal(`".." is not the root`)
				}
			}
		}
		if p.eof {
			break
		}
		if len(p.entries) == 0 {
			t.Fatal("an empty page before the end")
		}
	}
	if len(seen) != 52 || pages < 3 {
		t.Fatalf("%d entries in %d pages", len(seen), pages)
	}
	// Every child holds one reference however its handle was handed out.
	fh, _ := c.lookup(dir, "file-007")
	if n := f.refs("wide/file-007"); n != 1 {
		t.Fatalf("%d references", n)
	}
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != 0 {
		t.Fatalf("GETATTR of a listed handle: %d", s)
	}
}

// dircount bounds a page even when maxcount would allow more.
func TestReadDirPlusHonoursDirCount(t *testing.T) {
	_, c := startServer(t, wideEngine(50))
	dir, _ := c.lookup(c.root(), "wide")
	_, p := c.readDirPlus(dir, 0, 0, 200, 1<<20)
	used := 0
	for _, e := range p.entries {
		used += 8 + 4 + len(e.name) + pad(len(e.name)) + 8
	}
	if used > 200 || p.eof {
		t.Fatalf("%d bytes of directory information, eof %v", used, p.eof)
	}
}

func TestReadDirPlusCookies(t *testing.T) {
	_, c := startServer(t, wideEngine(5))
	dir, _ := c.lookup(c.root(), "wide")
	_, first := c.readDirPlus(dir, 0, 0, 1024, 1024)
	if first.eof {
		t.Fatal("the first page holds everything")
	}
	last := first.entries[len(first.entries)-1].cookie
	if s, _ := c.readDirPlus(dir, last, first.verf^1, 1024, 1024); s != uint32(nfsErrBadCook) {
		t.Fatalf("an unknown verifier: %d", s)
	}
	// Starting over needs no verifier and keeps the directory's.
	_, again := c.readDirPlus(dir, 0, 0, 1024, 1024)
	if again.verf != first.verf {
		t.Fatal("a fresh listing got another verifier")
	}
	_, rest := c.readDirPlus(dir, last, first.verf, 1024, 1<<20)
	if !rest.eof || rest.entries[0].cookie <= last {
		t.Fatalf("resumed page %+v", rest)
	}
}

func TestReadDirPlusTooSmall(t *testing.T) {
	_, c := startServer(t, wideEngine(5))
	dir, _ := c.lookup(c.root(), "wide")
	if s, _ := c.readDirPlus(dir, 0, 0, 1024, 200); s != uint32(nfsErrTooSmall) {
		t.Fatalf("status %d", s)
	}
}

func TestReadDir(t *testing.T) {
	_, c := startServer(t, sampleEngine())
	root := c.root()
	var names []string
	var cookie, verf uint64
	for {
		r := c.call(procReadDir, wire{}.opaque(root).u64(cookie).u64(verf).u32(256))
		if s := r.u32(); s != 0 {
			t.Fatalf("status %d", s)
		}
		r.postOpAttr()
		verf = r.u64()
		for r.u32() == 1 {
			fileid, name := r.u64(), string(r.opaque())
			cookie = r.u64()
			if fileid == 0 {
				t.Fatalf("%s has fileid 0", name)
			}
			names = append(names, name)
		}
		if r.u32() == 1 {
			break
		}
	}
	want := []string{".", "..", "a.txt", "dir", "link", "secret"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("listed %v, want %v", names, want)
	}
}

// Listing needs read permission on the directory.
func TestReadDirChecksPermission(t *testing.T) {
	f := sampleEngine()
	f.add("closed", 1100, syscall.S_IFDIR|0o300, "")
	_, c := startServer(t, f)
	dir, _ := c.lookup(c.root(), "closed")
	if s, _ := c.readDirPlus(dir, 0, 0, 1024, 4096); s != uint32(nfsErrAccess) {
		t.Fatalf("status %d", s)
	}
}

func TestEnumerationsAreBounded(t *testing.T) {
	f := newFakeEngine()
	for i := range maxEnumerations + 5 {
		f.add(fmt.Sprintf("d%d", i), uint64(100+i), syscall.S_IFDIR|0o755, "")
	}
	_, c := startServer(t, f)
	root := c.root()
	for i := range maxEnumerations + 5 {
		dir, _ := c.lookup(root, fmt.Sprintf("d%d", i))
		c.readDirPlus(dir, 0, 0, 1024, 4096)
	}
	f.mu.Lock()
	open := len(f.dirs)
	f.mu.Unlock()
	if open != maxEnumerations {
		t.Fatalf("%d directory handles open", open)
	}
}

// An enumeration evicted while a page reads through it keeps its WFS
// directory handle until that page is done.
func TestEvictionWaitsForThePage(t *testing.T) {
	f := newFakeEngine()
	dirs := newEnumerations(f)
	busy, _ := dirs.open(nil, 1000, header(1000, credential{}))
	for i := range maxEnumerations {
		e, _ := dirs.open(nil, uint64(2000+i), header(0, credential{}))
		dirs.done(e)
	}
	f.mu.Lock()
	stillOpen := f.dirs[busy.fh]
	f.mu.Unlock()
	if !stillOpen {
		t.Fatal("released while in use")
	}
	if _, ok := dirs.resume(1000, busy.verf); ok {
		t.Fatal("an evicted enumeration resumed")
	}
	dirs.done(busy)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirs[busy.fh] || len(f.dirs) != maxEnumerations {
		t.Fatalf("%d handles open, the evicted one %v", len(f.dirs), f.dirs[busy.fh])
	}
}
