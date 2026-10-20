package nfs

import (
	"container/list"
	"sync"
)

// drcSize bounds the duplicate-request cache. The kernel resends a call
// within its 30-second timeout, so a small cache spans many of them.
const drcSize = 1024

type replyKey struct {
	xid    uint32
	digest [32]byte
}

// replyCache remembers the replies to non-idempotent calls by XID and
// request digest, so a call the client resends, on the same connection or
// after reconnecting, gets the original reply and is not applied twice.
// Only finished entries are ever evicted; with every entry unfinished, a new
// call is turned away ("retry later") before it changes anything. "Retry
// later" replies are not kept: such a call changed nothing.
type replyCache struct {
	mu      sync.Mutex
	entries map[replyKey]*list.Element
	order   *list.List // of *cachedReply, oldest first
}

type cachedReply struct {
	key      replyKey
	conn     *conn         // where the first copy arrived
	done     chan struct{} // closed by finish
	finished bool
	r        reply
}

const (
	drcNew = iota
	drcInProgress
	drcDone
	drcFull
)

func newReplyCache() *replyCache {
	return &replyCache{entries: map[replyKey]*list.Element{}, order: list.New()}
}

// begin decides, in one step, what becomes of a call with key k: a copy
// of one the cache holds (drcDone or drcInProgress, returning its entry),
// a new call admitted (drcNew: admit took its slot, the entry is made, and
// finish must follow), or a new call turned away (drcFull: no slot, or no
// room for an entry; nothing was taken). Deciding under one lock is what
// keeps two simultaneous copies from both being taken for new.
func (c *replyCache) begin(k replyKey, from *conn, admit func() bool) (*cachedReply, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		e := el.Value.(*cachedReply)
		if e.finished {
			return e, drcDone
		}
		return e, drcInProgress
	}
	if len(c.entries) >= drcSize && !c.evictFinished() || !admit() {
		return nil, drcFull
	}
	e := &cachedReply{key: k, conn: from, done: make(chan struct{})}
	c.entries[k] = c.order.PushBack(e)
	return e, drcNew
}

// evictFinished drops the oldest finished entry.
func (c *replyCache) evictFinished() bool {
	for el := c.order.Front(); el != nil; el = el.Next() {
		if e := el.Value.(*cachedReply); e.finished {
			c.order.Remove(el)
			delete(c.entries, e.key)
			return true
		}
	}
	return false
}

// finish records the reply to a call begin admitted, and hands it to any
// copy waiting on done.
func (c *replyCache) finish(e *cachedReply, r reply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.r, e.finished = r, true
	if el, ok := c.entries[e.key]; ok && r.retry {
		c.order.Remove(el)
		delete(c.entries, e.key)
	}
	close(e.done)
}
