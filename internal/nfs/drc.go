package nfs

import "sync"

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
// "Retry later" replies are never kept: such a call changed nothing.
type replyCache struct {
	mu      sync.Mutex
	entries map[replyKey]*cachedReply
	order   []replyKey // oldest first
}

type cachedReply struct {
	done bool
	r    reply
}

const (
	drcNew = iota
	drcInProgress
	drcDone
)

func newReplyCache() *replyCache {
	return &replyCache{entries: map[replyKey]*cachedReply{}}
}

// begin looks k up. drcNew means the call is now in progress, and finish
// must follow; drcDone returns the reply to send again; drcInProgress means
// the first copy is still running and answers for both.
func (c *replyCache) begin(k replyKey) (reply, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[k]; ok {
		if e.done {
			return e.r, drcDone
		}
		return reply{}, drcInProgress
	}
	c.entries[k] = &cachedReply{}
	c.order = append(c.order, k)
	for len(c.order) > drcSize {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	return reply{}, drcNew
}

// finish records the reply to a call begin admitted.
func (c *replyCache) finish(k replyKey, r reply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	if !ok {
		return // evicted meanwhile
	}
	if r.retry {
		delete(c.entries, k)
		for i, o := range c.order {
			if o == k {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		return
	}
	e.done, e.r = true, r
}
