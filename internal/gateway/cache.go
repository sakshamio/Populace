package gateway

import (
	"container/list"
	"crypto/sha256"
	"sync"
)

// Cache is a byte-bounded LRU over request body -> response body.
//
// Worth having because this workload is unusually repetitive: the same
// (archetype, event) pair is asked for whenever a scenario is replayed, a
// persona is added to an existing archetype, or a run is repeated with a
// different seed elsewhere in the pipeline. A hit costs nothing where a miss
// costs seconds of GPU time, so the hit rate is the single most useful number
// on the health endpoint.
//
// Keyed on the exact request bytes. That is deliberately conservative -- a
// semantically identical request with keys in a different order misses -- but
// the alternative is canonicalising JSON and hoping the canonicaliser agrees
// with the model server about what changes an answer. Callers that want hits
// should send stable request bodies, which a generated pipeline does anyway.
type Cache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	ll       *list.List
	items    map[[32]byte]*list.Element
	hits     int64
	misses   int64
}

type entry struct {
	key  [32]byte
	val  []byte
	size int64
}

func NewCache(maxBytes int64) *Cache {
	return &Cache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[[32]byte]*list.Element),
	}
}

func (c *Cache) Get(req []byte) ([]byte, bool) {
	k := sha256.Sum256(req)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		c.misses++
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return el.Value.(*entry).val, true
}

func (c *Cache) Put(req, resp []byte) {
	if int64(len(resp)) > c.maxBytes {
		return // a single entry that cannot coexist with anything else
	}
	k := sha256.Sum256(req)
	// Copy: the caller's buffer may be reused, and a cache that hands out
	// aliases of mutable memory is a bug that shows up as random corruption.
	v := make([]byte, len(resp))
	copy(v, resp)

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		e := el.Value.(*entry)
		c.bytes += int64(len(v)) - e.size
		e.val, e.size = v, int64(len(v))
		c.ll.MoveToFront(el)
	} else {
		e := &entry{key: k, val: v, size: int64(len(v))}
		c.items[k] = c.ll.PushFront(e)
		c.bytes += e.size
	}
	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := c.ll.Remove(back).(*entry)
		delete(c.items, e.key)
		c.bytes -= e.size
	}
}

func (c *Cache) Stats() (hits, misses, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.bytes
}
