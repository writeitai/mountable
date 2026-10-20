// Derived from go-nfs (github.com/willscott/go-nfs at
// 6274d051a57fe87e8cb9cda60e38824db1ab08a7: conn.go, server.go and
// errors.go), Apache License 2.0; see LICENSE and NOTICE in this directory.
// Changed: one fixed pair of programs instead of a handler registry, the
// program and version checked on every call, multi-fragment records
// reassembled within a size bound, calls decoded without reflection,
// AUTH_SYS credentials parsed for every call, replies encoded big-endian
// throughout (go-nfs wrote some error bodies little-endian), a bounded number
// of connections and of calls in progress, a duplicate-request cache, and
// connections tracked so Close ends them all.

package nfs

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const (
	// maxConnections bounds the client connections served at once: the
	// kernel uses one, and reconnects after a reset.
	maxConnections = 16
	// maxPending bounds the calls read but not yet answered; past it, a
	// call is answered "retry later" at once.
	maxPending = 256
	// maxRecord bounds one request record: a 1 MiB WRITE and its header.
	maxRecord = 1<<20 + 4096
	// maxFragments bounds the fragments of one record; the kernel sends one.
	maxFragments = 64
)

// Server serves one export's NFS and MOUNT programs on stream connections.
type Server struct {
	svc     *service
	pending chan struct{}
	waiting chan struct{} // copies waiting for their first copy's reply
	drc     *replyCache

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closed   bool
	readers  sync.WaitGroup
}

// Serve accepts connections on l until Close. It returns nil once closed.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		l.Close()
		return nil
	}
	s.listener = l
	s.mu.Unlock()
	for {
		nc, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		s.mu.Lock()
		if s.closed || len(s.conns) >= maxConnections {
			s.mu.Unlock()
			nc.Close()
			continue
		}
		s.conns[nc] = struct{}{}
		s.readers.Add(1)
		s.mu.Unlock()
		c := &conn{Conn: nc, s: s}
		go c.serve()
	}
}

// Close stops accepting, ends the export (calls waiting for a worker are
// answered at once and those running are cancelled), closes every
// connection and waits for their readers, then waits for the work still
// running, at most rpcDeadline: every call is answered within it, and only
// engine work that ignores cancellation runs longer.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.Unlock()
	s.svc.end()
	s.mu.Lock()
	for nc := range s.conns {
		nc.Close()
	}
	s.mu.Unlock()
	s.readers.Wait()
	s.svc.wait(rpcDeadline)
}

// spawn runs fn on its own goroutine, counted as work Close waits for.
func (s *Server) spawn(fn func()) {
	s.svc.work.Add(1)
	go func() {
		defer s.svc.work.Done()
		fn()
	}()
}

type conn struct {
	net.Conn
	s  *Server
	mu sync.Mutex // one reply written at a time
}

func (c *conn) serve() {
	defer func() {
		c.s.mu.Lock()
		delete(c.s.conns, c.Conn)
		c.s.mu.Unlock()
		c.Close()
		c.s.readers.Done()
	}()
	r := bufio.NewReader(c)
	for {
		record, err := readRecord(r)
		if err != nil {
			return
		}
		cl, rejected, err := parseCall(record)
		if err != nil {
			return // not a call this server can even answer
		}
		cl.received = time.Now()
		if rejected != nil {
			c.send(cl.xid, *rejected)
			continue
		}
		c.dispatch(cl)
	}
}

// dispatch answers a call without holding up the reading of the next one.
func (c *conn) dispatch(cl *call) {
	svc := c.s.svc
	if r, ok := checkProgram(cl); !ok {
		c.send(cl.xid, r)
		return
	}
	if r, ok := svc.immediate(cl); ok {
		c.send(cl.xid, r)
		return
	}
	if r, ok := svc.admit(cl); !ok {
		c.send(cl.xid, r)
		return
	}
	if !nonIdempotent(cl.proc) {
		if !c.admitted() {
			c.send(cl.xid, svc.retryLater(cl)) // changed nothing
			return
		}
		c.s.spawn(func() {
			defer c.release()
			c.send(cl.xid, svc.handle(cl))
		})
		return
	}
	// A copy of a call already running or answered is recognised before
	// anything else: told to retry later, the client would resend it under
	// a new XID and it would run twice.
	key := cl.key()
	if entry, state := c.s.drc.lookup(key); state != drcNew {
		c.duplicate(cl, entry, state)
		return
	}
	if !c.admitted() {
		c.send(cl.xid, svc.retryLater(cl)) // a new call, changed nothing
		return
	}
	entry, state := c.s.drc.begin(key, c)
	if state != drcNew {
		c.release()
		c.duplicate(cl, entry, state) // the first copy arrived meanwhile
		return
	}
	c.s.spawn(func() {
		defer c.release()
		r := svc.handle(cl)
		c.s.drc.finish(entry, r)
		c.send(cl.xid, r)
	})
}

// admitted takes a pending slot for a new call, if one is free.
func (c *conn) admitted() bool {
	select {
	case c.s.pending <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *conn) release() { <-c.s.pending }

// duplicate answers a copy of a call the cache already holds: with its
// reply if it is finished; with nothing if the first copy is still running
// on this connection, which it answers; and with the first copy's reply
// once it is done if the client has reconnected meanwhile. Copies waiting
// are bounded apart from new calls, and past the bound one is dropped,
// never told to retry later.
func (c *conn) duplicate(cl *call, entry *cachedReply, state int) {
	switch {
	case state == drcDone:
		c.send(cl.xid, entry.r)
	case state == drcFull:
		c.send(cl.xid, c.s.svc.retryLater(cl)) // no entry was made: a new call
	case entry.conn == c:
	default:
		select {
		case c.s.waiting <- struct{}{}:
		default:
			return
		}
		c.s.spawn(func() {
			defer func() { <-c.s.waiting }()
			wait := time.NewTimer(time.Until(cl.received.Add(rpcDeadline)))
			defer wait.Stop()
			select {
			case <-entry.done:
				c.send(cl.xid, entry.r)
			case <-wait.C:
			}
		})
	}
}

// send writes one reply record. A failed write closes the connection; the
// client retransmits on its next one.
func (c *conn) send(xid uint32, r reply) {
	body := encodeReply(xid, r)
	record := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(body)), 1<<31|uint32(len(body)))
	record = append(record, body...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.Write(record); err != nil {
		c.Close()
	}
}

// readRecord reads one RPC record (RFC 5531 §11): fragments up to the one
// marked last, at most maxFragments of them and maxRecord bytes in all. An
// empty fragment that is not the last is refused: it would never end.
func readRecord(r io.Reader) ([]byte, error) {
	var record []byte
	for fragments := 1; ; fragments++ {
		var mark [4]byte
		if _, err := io.ReadFull(r, mark[:]); err != nil {
			return nil, err
		}
		header := binary.BigEndian.Uint32(mark[:])
		last := header&(1<<31) != 0
		n := int(header &^ (1 << 31))
		if len(record)+n > maxRecord || n == 0 && !last || fragments > maxFragments {
			return nil, errGarbage
		}
		start := len(record)
		record = append(record, make([]byte, n)...)
		if _, err := io.ReadFull(r, record[start:]); err != nil {
			return nil, err
		}
		if last {
			return record, nil
		}
	}
}

// call is one decoded RPC call.
type call struct {
	xid              uint32
	prog, vers, proc uint32
	cred             credential
	args             []byte
	received         time.Time
}

// credential is the call's credential. Only AUTH_SYS carries identities.
type credential struct {
	flavor   uint32
	uid, gid uint32
	gids     []uint32
}

// key identifies a call for the duplicate-request cache: its XID and a
// digest of everything else it says, credential and groups included, so a
// reused XID with other contents is a new call.
func (c *call) key() replyKey {
	e := &encoder{}
	for _, v := range []uint32{c.prog, c.vers, c.proc, c.cred.flavor, c.cred.uid, c.cred.gid, uint32(len(c.cred.gids))} {
		e.uint32(v)
	}
	for _, g := range c.cred.gids {
		e.uint32(g)
	}
	h := sha256.New()
	h.Write(e.buf)
	h.Write(c.args)
	k := replyKey{xid: c.xid}
	h.Sum(k.digest[:0])
	return k
}

// parseCall decodes a call message. A call the server must refuse outright
// (another RPC version, a malformed credential) comes back with its
// rejection; err means the record is not a call at all.
func parseCall(record []byte) (*call, *reply, error) {
	d := &decoder{buf: record}
	c := &call{xid: d.uint32()}
	msgType := d.uint32()
	if d.err != nil || msgType != msgCall {
		return nil, nil, errGarbage
	}
	if v := d.uint32(); d.err == nil && v != rpcVersion {
		return c, &reply{denied: true, stat: rejectRPCMismatch, body: mismatch(rpcVersion, rpcVersion)}, nil
	}
	c.prog, c.vers, c.proc = d.uint32(), d.uint32(), d.uint32()
	c.cred.flavor = d.uint32()
	credBody := d.opaque(maxAuthBytes)
	d.uint32() // verifier flavor
	d.opaque(maxAuthBytes)
	if d.err != nil {
		return nil, nil, errGarbage
	}
	if c.cred.flavor == authSys {
		cd := &decoder{buf: credBody}
		cd.uint32()    // stamp
		cd.opaque(255) // machine name
		c.cred.uid, c.cred.gid = cd.uint32(), cd.uint32()
		n := cd.uint32()
		if cd.err == nil && n > 16 {
			cd.err = errGarbage
		}
		for i := uint32(0); cd.err == nil && i < n; i++ {
			c.cred.gids = append(c.cred.gids, cd.uint32())
		}
		if cd.err != nil || len(cd.buf) != 0 {
			return c, &reply{denied: true, stat: rejectAuthError, body: be32(authBadCred)}, nil
		}
	}
	c.args = d.buf
	return c, nil, nil
}

// checkProgram answers a call to anything but NFS v3 and MOUNT v3.
func checkProgram(c *call) (reply, bool) {
	switch {
	case c.prog != programNFS && c.prog != programMount:
		return reply{stat: acceptProgUnavail}, false
	case c.vers != version3:
		return reply{stat: acceptProgMismatch, body: mismatch(version3, version3)}, false
	}
	return reply{}, true
}

// reply is the part of an RPC reply after the XID and message type.
type reply struct {
	denied bool   // MSG_DENIED rather than MSG_ACCEPTED
	stat   uint32 // accept_stat, or reject_stat when denied
	body   []byte // procedure results, mismatch_info or auth_stat
	// retry marks an NFS "retry later" answer, which is never cached: the
	// call changed nothing and its retransmission runs again.
	retry bool
}

func encodeReply(xid uint32, r reply) []byte {
	e := &encoder{buf: make([]byte, 0, 24+len(r.body))}
	e.uint32(xid)
	e.uint32(msgReply)
	if r.denied {
		e.uint32(replyDenied)
	} else {
		e.uint32(replyAccepted)
		e.uint32(authNone) // verifier
		e.uint32(0)
	}
	e.uint32(r.stat)
	e.buf = append(e.buf, r.body...)
	return e.buf
}

func mismatch(low, high uint32) []byte {
	e := &encoder{}
	e.uint32(low)
	e.uint32(high)
	return e.buf
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
