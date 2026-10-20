package nfs

import (
	"container/list"
	"crypto/rand"
	"encoding/binary"
	"sync"

	"github.com/seaweedfs/go-fuse/v2/fuse"
)

// maxEnumerations bounds the directories with a listing in progress. Each
// holds a WFS directory handle, which keeps the batch it last read.
const maxEnumerations = 64

// enumeration is the listing state of one directory: a WFS directory handle,
// which pages through the directory by name, and the verifier its cookies
// carry. A cookie is WFS's offset after the entry.
//
// There is one per directory, not one per listing, because the macOS client
// keeps one verifier per directory and sends the latest with every cookie.
// It treats NFS3ERR_BAD_COOKIE as the end of the directory (nfs_vnops.c,
// rdar://102465951), so a listing is never invalidated because the
// directory changed: WFS resumes by name, as readdir(3) on a changing
// directory allows. BAD_COOKIE answers only a verifier this process does
// not hold, such as one whose enumeration was evicted.
type enumeration struct {
	mu    sync.Mutex // one page at a time
	dir   uint64
	verf  uint64
	fh    uint64 // the WFS directory handle
	entry *list.Element
	// users counts the pages reading through it; an evicted enumeration's
	// handle is released once the last of them is done.
	users   int
	evicted bool
}

type enumerations struct {
	engine Engine
	mu     sync.Mutex
	byDir  map[uint64]*enumeration
	byVerf map[uint64]*enumeration
	lru    *list.List // of *enumeration, most recently used first
	prefix uint64     // random high half of every verifier
	last   uint32
}

func newEnumerations(engine Engine) *enumerations {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return &enumerations{
		engine: engine,
		byDir:  map[uint64]*enumeration{},
		byVerf: map[uint64]*enumeration{},
		lru:    list.New(),
		prefix: binary.BigEndian.Uint64(b[:]) &^ 0xffffffff,
	}
}

// open returns dir's enumeration for a listing from the start, creating it
// if needed. done must follow.
func (t *enumerations) open(cancel <-chan struct{}, dir uint64, h fuse.InHeader) (*enumeration, fuse.Status) {
	t.mu.Lock()
	if e := t.byDir[dir]; e != nil {
		t.use(e)
		t.mu.Unlock()
		return e, fuse.OK
	}
	t.mu.Unlock()
	var out fuse.OpenOut
	h.NodeId = dir
	if st := t.engine.OpenDir(cancel, &fuse.OpenIn{InHeader: h}, &out); st != fuse.OK {
		return nil, st
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.byDir[dir]; e != nil { // opened concurrently
		t.engine.ReleaseDir(&fuse.ReleaseIn{Fh: out.Fh})
		t.use(e)
		return e, fuse.OK
	}
	t.last++
	e := &enumeration{dir: dir, verf: t.prefix | uint64(t.last), fh: out.Fh}
	e.entry = t.lru.PushFront(e)
	t.byDir[dir], t.byVerf[e.verf] = e, e
	t.use(e)
	for t.lru.Len() > maxEnumerations {
		old := t.lru.Remove(t.lru.Back()).(*enumeration)
		delete(t.byDir, old.dir)
		delete(t.byVerf, old.verf)
		old.evicted = true
		if old.users == 0 {
			t.engine.ReleaseDir(&fuse.ReleaseIn{Fh: old.fh})
		}
	}
	return e, fuse.OK
}

// resume returns the enumeration a cookie's verifier names, if it is dir's.
// done must follow.
func (t *enumerations) resume(dir, verf uint64) (*enumeration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.byVerf[verf]
	if e == nil || e.dir != dir {
		return nil, false
	}
	t.use(e)
	return e, true
}

func (t *enumerations) use(e *enumeration) {
	e.users++
	t.lru.MoveToFront(e.entry)
}

// done ends a page's use of an enumeration.
func (t *enumerations) done(e *enumeration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.users--; e.users == 0 && e.evicted {
		t.engine.ReleaseDir(&fuse.ReleaseIn{Fh: e.fh})
	}
}

// listed is one entry a page carries.
type listed struct {
	name     string
	ino, off uint64
	plus     bool // WFS took a lookup reference for it
	obj      *object
}

// dirSink collects one page from WFS within the reply's two budgets:
// bytes, the space left for entries in the whole reply (maxcount), and
// dirBytes, what READDIRPLUS's dircount allows for names, fileids and
// cookies alone.
type dirSink struct {
	plus            bool
	bytes, dirBytes int
	entries         []listed
	full            bool
}

// entrySize is an entry's encoded size in the reply, and its share of
// dircount.
func entrySize(name string, plus bool) (size, dirSize int) {
	dirSize = 8 + 4 + len(name) + pad(len(name)) + 8 // fileid, name, cookie
	size = 4 + dirSize                               // value_follows
	if plus {
		size += 4 + fattrSize + 4 + 4 + handleSize // post_op_attr, post_op_fh3
	}
	return size, dirSize
}

func (s *dirSink) take(entry fuse.DirEntry, plus bool) bool {
	size, dirSize := entrySize(entry.Name, s.plus)
	if size > s.bytes || s.plus && dirSize > s.dirBytes {
		s.full = true
		return false
	}
	s.bytes -= size
	s.dirBytes -= dirSize
	dot := entry.Name == "." || entry.Name == ".."
	s.entries = append(s.entries, listed{name: entry.Name, ino: entry.Ino, off: entry.Off, plus: plus && !dot})
	return true
}

func (s *dirSink) AddEntry(entry fuse.DirEntry) bool { return s.take(entry, false) }

func (s *dirSink) AddEntryPlus(entry fuse.DirEntry) *fuse.EntryOut {
	if !s.take(entry, true) {
		return nil
	}
	return &fuse.EntryOut{} // the page reads attributes through GetAttr
}

// TakesLookupRef: every entry of a READDIRPLUS page carries a handle, and a
// handle holds its inode.
func (s *dirSink) TakesLookupRef() bool { return true }

func (s *service) readDir(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	cookie, verf := d.uint64(), d.uint64()
	count := d.uint32()
	if d.err != nil {
		return garbage()
	}
	return s.list(o, fh, cookie, verf, int(count), int(count), false)
}

func (s *service) readDirPlus(o *op) reply {
	d := &decoder{buf: o.args}
	fh := d.opaque(64)
	cookie, verf := d.uint64(), d.uint64()
	dirCount, maxCount := d.uint32(), d.uint32()
	if d.err != nil {
		return garbage()
	}
	return s.list(o, fh, cookie, verf, int(dirCount), int(maxCount), true)
}

// pageOverhead is a listing reply without entries: status, the directory's
// post_op_attr, the verifier, the end of the list and eof.
const pageOverhead = 4 + 4 + fattrSize + 8 + 4 + 4

// list answers READDIR and READDIRPLUS: one page from cookie, within both
// budgets, read through the directory's enumeration.
func (s *service) list(o *op, fh []byte, cookie, verf uint64, dirCount, maxCount int, plus bool) reply {
	dir, status := s.resolve(o, fh)
	if status != nfsOK {
		return s.failure(o.call, status)
	}
	fail := func(status nfsStatus) reply {
		e := &encoder{}
		e.uint32(uint32(status))
		putPostOpAttr(e, &dir.attr, s.fsid)
		return reply{body: e.buf}
	}
	switch {
	case !isDir(&dir.attr):
		return fail(nfsErrNotDir)
	case !allowed(o.cred, &dir.attr, permRead):
		return fail(nfsErrAccess)
	}
	// The first page must hold "." and "..": WFS adds them whether or not
	// they fit.
	sink := &dirSink{plus: plus, bytes: min(maxCount, maxTransfer) - pageOverhead, dirBytes: dirCount}
	if cookie < 2 {
		dot, dotDir := entrySize(".", plus)
		dotdot, dotdotDir := entrySize("..", plus)
		if sink.bytes < dot+dotdot || plus && sink.dirBytes < dotDir+dotdotDir {
			return fail(nfsErrTooSmall)
		}
	}
	h := header(dir.inode, o.cred)
	var en *enumeration
	if cookie == 0 {
		var st fuse.Status
		if en, st = s.dirs.open(o.cancel, dir.inode, h); st != fuse.OK {
			return fail(statusOf(st))
		}
	} else if en, _ = s.dirs.resume(dir.inode, verf); en == nil {
		return fail(nfsErrBadCook)
	}
	en.mu.Lock()
	eof, st := s.page(en, h, cookie, sink, plus)
	en.mu.Unlock()
	s.dirs.done(en)
	if st != fuse.OK {
		s.drop(sink.entries)
		return fail(statusOf(st))
	}
	if len(sink.entries) == 0 && !eof {
		return fail(nfsErrTooSmall)
	}
	for i := range sink.entries {
		s.fill(o, dir, &sink.entries[i], plus)
	}

	e := &encoder{}
	e.uint32(uint32(nfsOK))
	putPostOpAttr(e, &dir.attr, s.fsid)
	e.uint64(en.verf)
	for _, entry := range sink.entries {
		e.bool(true)
		e.uint64(entry.ino)
		e.string(entry.name)
		e.uint64(entry.off)
		if plus {
			if entry.obj == nil {
				e.bool(false)
				e.bool(false)
				continue
			}
			putPostOpAttr(e, &entry.obj.attr, s.fsid)
			e.bool(true)
			e.opaque(s.fh(*entry.obj))
		}
	}
	e.bool(false)
	e.bool(eof)
	return reply{body: e.buf}
}

// page reads entries from cookie into sink until it is full or the
// directory ends.
func (s *service) page(en *enumeration, h fuse.InHeader, cookie uint64, sink *dirSink, plus bool) (eof bool, st fuse.Status) {
	offset := cookie
	for {
		before := len(sink.entries)
		if st := s.engine.ReadDirectoryInto(&fuse.ReadIn{InHeader: h, Fh: en.fh, Offset: offset}, sink, plus); st != fuse.OK {
			return false, st
		}
		switch {
		case sink.full:
			return false, fuse.OK
		case len(sink.entries) == before:
			return true, fuse.OK
		}
		offset = sink.entries[len(sink.entries)-1].off
	}
}

// fill gives an entry its fileid and, on a READDIRPLUS page, its handle and
// attributes. A child WFS just referenced gets a handle that keeps the
// reference; "." is the directory and ".." its parent.
func (s *service) fill(o *op, dir object, entry *listed, plus bool) {
	var obj object
	status := nfsOK
	switch {
	case entry.name == ".":
		obj = dir
	case entry.name == "..":
		obj, status = s.parent(o, dir)
	case entry.plus:
		obj, status = s.found(o, entry.ino)
	default:
		return // READDIR: WFS's fileid as listed
	}
	if status != nfsOK {
		if entry.ino == 0 {
			entry.ino = dir.inode // a fileid of zero would hide the entry
		}
		return
	}
	entry.ino = obj.inode
	if plus {
		entry.obj = &obj
	}
}

// drop gives back the lookup references a page that is not sent took.
func (s *service) drop(entries []listed) {
	for _, entry := range entries {
		if entry.plus {
			s.engine.Forget(entry.ino, 1)
		}
	}
}
