package nfs

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests talk to the server over a real socket in XDR built and parsed
// here, independently of the server's own encoder and decoder.

type wire []byte

func (w wire) u32(v uint32) wire { return binary.BigEndian.AppendUint32(w, v) }
func (w wire) u64(v uint64) wire { return binary.BigEndian.AppendUint64(w, v) }
func (w wire) opaque(b []byte) wire {
	w = w.u32(uint32(len(b)))
	w = append(w, b...)
	for len(w)%4 != 0 {
		w = append(w, 0)
	}
	return w
}
func (w wire) str(s string) wire { return w.opaque([]byte(s)) }

// authSysCred is an AUTH_SYS credential body for uid, in group 20.
func authSysCred(uid uint32) wire {
	return wire{}.u32(0).str("mac").u32(uid).u32(20).u32(1).u32(20)
}

// callMessage is an RPC call with an AUTH_SYS credential and a null
// verifier.
func callMessage(xid, prog, vers, proc, uid uint32, args wire) wire {
	w := wire{}.u32(xid).u32(0).u32(2).u32(prog).u32(vers).u32(proc)
	w = w.u32(1).opaque(authSysCred(uid)).u32(0).u32(0)
	return append(w, args...)
}

// replyReader parses a reply.
type replyReader struct {
	t *testing.T
	b []byte
}

func (r *replyReader) u32() uint32 {
	r.t.Helper()
	if len(r.b) < 4 {
		r.t.Fatalf("reply too short")
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v
}

func (r *replyReader) u64() uint64 { return uint64(r.u32())<<32 | uint64(r.u32()) }

func (r *replyReader) opaque() []byte {
	n := int(r.u32())
	if len(r.b) < n+pad(n) {
		r.t.Fatalf("opaque of %d bytes past the reply", n)
	}
	b := r.b[:n]
	r.b = r.b[n+pad(n):]
	return b
}

// attr reads fattr3.
type fattr struct {
	ftype, mode, nlink, uid, gid uint32
	size, used, fsid, fileid     uint64
	mtimeSec, mtimeNsec          uint32
}

func (r *replyReader) fattr() fattr {
	a := fattr{ftype: r.u32(), mode: r.u32(), nlink: r.u32(), uid: r.u32(), gid: r.u32()}
	a.size, a.used = r.u64(), r.u64()
	r.u32() // rdev
	r.u32()
	a.fsid, a.fileid = r.u64(), r.u64()
	r.u64() // atime
	a.mtimeSec, a.mtimeNsec = r.u32(), r.u32()
	r.u64() // ctime
	return a
}

// postOpAttr reads post_op_attr; ok is whether attributes follow.
func (r *replyReader) postOpAttr() (fattr, bool) {
	if r.u32() == 0 {
		return fattr{}, false
	}
	return r.fattr(), true
}

type testClient struct {
	t    *testing.T
	conn net.Conn
	xid  uint32
}

// startServer serves engine to the mounting user 501 over a Unix socket.
func startServer(t *testing.T, engine Engine) (*Server, *testClient) {
	t.Helper()
	srv, err := NewServer(Config{Engine: engine, UID: 501})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "nfs") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(srv.Close)
	return srv, dial(t, l.Addr().String())
}

func dial(t *testing.T, addr string) *testClient {
	t.Helper()
	conn, err := net.Dial("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return &testClient{t: t, conn: conn, xid: 100}
}

// send writes one record.
func (c *testClient) send(msg wire) {
	c.t.Helper()
	record := wire{}.u32(1<<31 | uint32(len(msg)))
	if _, err := c.conn.Write(append(record, msg...)); err != nil {
		c.t.Fatal(err)
	}
}

// receive reads one reply record.
func (c *testClient) receive() []byte {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var mark [4]byte
	if _, err := io.ReadFull(c.conn, mark[:]); err != nil {
		c.t.Fatal(err)
	}
	n := binary.BigEndian.Uint32(mark[:])
	if n&(1<<31) == 0 {
		c.t.Fatal("reply split into fragments")
	}
	body := make([]byte, n&^(1<<31))
	if _, err := io.ReadFull(c.conn, body); err != nil {
		c.t.Fatal(err)
	}
	return body
}

// raw makes one call and returns the whole reply.
func (c *testClient) raw(prog, vers, proc, uid uint32, args wire) []byte {
	c.t.Helper()
	c.xid++
	c.send(callMessage(c.xid, prog, vers, proc, uid, args))
	return c.receive()
}

// call makes an NFS call as the mounting user and returns the reply after
// a successful accept_stat.
func (c *testClient) call(proc uint32, args wire) *replyReader {
	c.t.Helper()
	return c.callAs(501, proc, args)
}

func (c *testClient) callAs(uid, proc uint32, args wire) *replyReader {
	c.t.Helper()
	return c.accepted(c.raw(programNFS, 3, proc, uid, args))
}

// accepted checks the reply header (this XID, accepted, null verifier,
// SUCCESS) and returns what follows.
func (c *testClient) accepted(reply []byte) *replyReader {
	c.t.Helper()
	want := wire{}.u32(c.xid).u32(1).u32(0).u32(0).u32(0).u32(0)
	if !bytes.HasPrefix(reply, want) {
		c.t.Fatalf("reply header % x, want % x", reply[:min(len(reply), 24)], want)
	}
	return &replyReader{t: c.t, b: reply[len(want):]}
}

// root mounts "/" and returns the root handle.
func (c *testClient) root() []byte {
	c.t.Helper()
	r := c.accepted(c.raw(programMount, 3, mountProcMnt, 501, wire{}.str("/")))
	if status := r.u32(); status != 0 {
		c.t.Fatalf("MNT status %d", status)
	}
	fh := r.opaque()
	if n := r.u32(); n != 1 || r.u32() != 1 {
		c.t.Fatal("MNT must offer exactly AUTH_SYS")
	}
	return fh
}

// lookup returns the handle and attributes of name in dir, failing the test
// on an error.
func (c *testClient) lookup(dir []byte, name string) ([]byte, fattr) {
	c.t.Helper()
	r := c.call(procLookup, wire{}.opaque(dir).str(name))
	if status := r.u32(); status != 0 {
		c.t.Fatalf("LOOKUP %s: status %d", name, status)
	}
	fh := r.opaque()
	a, ok := r.postOpAttr()
	if !ok {
		c.t.Fatal("LOOKUP without attributes")
	}
	return fh, a
}

func (c *testClient) status(proc uint32, args wire) uint32 {
	c.t.Helper()
	return c.call(proc, args).u32()
}
