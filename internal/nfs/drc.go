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

// lookup finds k without making an entry: drcNew means there is none.
func (c *replyCache) lookup(k replyKey) (*cachedReply, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[k]
	if !ok {
		return nil, drcNew
	}
	if e := el.Value.(*cachedReply); e.finished {
		return e, drcDone
	}
	return el.Value.(*cachedReply), drcInProgress
}

// begin looks k up. drcNew returns the entry the call now holds, and finish
// must follow; drcDone and drcInProgress return the existing entry, whose
// reply is final once done is closed; drcFull means no entry can be made.
func (c *replyCache) begin(k replyKey, from *conn) (*cachedReply, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[k]; ok {
		e := el.Value.(*cachedReply)
		if e.finished {
			return e, drcDone
		}
		return e, drcInProgress
	}
	if len(c.entries) >= drcSize && !c.evictFinished() {
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
