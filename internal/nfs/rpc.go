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
)

// Server serves one export's NFS and MOUNT programs on stream connections.
type Server struct {
	svc     *service
	pending chan struct{}
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

// Close stops accepting, closes every connection and waits for their
// readers to stop. Calls still running answer into closed connections.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	if s.listener != nil {
		s.listener.Close()
	}
	for nc := range s.conns {
		nc.Close()
	}
	s.mu.Unlock()
	s.readers.Wait()
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
	if r, ok := checkProgram(cl); !ok {
		c.send(cl.xid, r)
		return
	}
	// MOUNT and NULL calls never reach the engine: answered at once.
	if _, known := procedures[cl.proc]; cl.prog == programMount || cl.proc == procNull || !known {
		c.send(cl.xid, c.s.svc.handle(cl))
		return
	}
	key, cached := replyKey{}, nonIdempotent(cl.proc)
	if cached {
		key = cl.key()
		switch r, state := c.s.drc.begin(key); state {
		case drcDone:
			c.send(cl.xid, r)
			return
		case drcInProgress:
			return // the first copy answers
		}
	}
	select {
	case c.s.pending <- struct{}{}:
	default:
		r := c.s.svc.retryLater(cl)
		if cached {
			c.s.drc.finish(key, r)
		}
		c.send(cl.xid, r)
		return
	}
	go func() {
		defer func() { <-c.s.pending }()
		r := c.s.svc.handle(cl)
		if cached {
			c.s.drc.finish(key, r)
		}
		c.send(cl.xid, r)
	}()
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
// marked last, at most maxRecord bytes in all.
func readRecord(r io.Reader) ([]byte, error) {
	var record []byte
	for {
		var mark [4]byte
		if _, err := io.ReadFull(r, mark[:]); err != nil {
			return nil, err
		}
		header := binary.BigEndian.Uint32(mark[:])
		n := int(header &^ (1 << 31))
		if len(record)+n > maxRecord {
			return nil, errGarbage
		}
		start := len(record)
		record = append(record, make([]byte, n)...)
		if _, err := io.ReadFull(r, record[start:]); err != nil {
			return nil, err
		}
		if header&(1<<31) != 0 {
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
// digest of everything else it says, so a reused XID with other contents
// is a new call.
func (c *call) key() replyKey {
	h := sha256.New()
	var head [24]byte
	binary.BigEndian.PutUint32(head[0:], c.prog)
	binary.BigEndian.PutUint32(head[4:], c.vers)
	binary.BigEndian.PutUint32(head[8:], c.proc)
	binary.BigEndian.PutUint32(head[12:], c.cred.flavor)
	binary.BigEndian.PutUint32(head[16:], c.cred.uid)
	binary.BigEndian.PutUint32(head[20:], c.cred.gid)
	h.Write(head[:])
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
