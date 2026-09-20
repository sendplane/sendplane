package render

import (
	"container/list"
	"sync"
)

// lruCache is a small size-capped LRU. It is used for parsed templates, which
// are read far more often than they are written, and must be safe for
// concurrent use because one Renderer serves every sender goroutine.
type lruCache[K comparable, V any] struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List
	items map[K]*list.Element
}

type lruEntry[K comparable, V any] struct {
	key K
	val V
}

func newLRU[K comparable, V any](capacity int) *lruCache[K, V] {
	if capacity < 1 {
		capacity = 1
	}
	return &lruCache[K, V]{
		cap:   capacity,
		ll:    list.New(),
		items: make(map[K]*list.Element, capacity),
	}
}

func (c *lruCache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruEntry[K, V]).val, true
	}
	var zero V
	return zero, false
}

func (c *lruCache[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*lruEntry[K, V]).val = v
		return
	}
	c.items[k] = c.ll.PushFront(&lruEntry[K, V]{key: k, val: v})
	for c.ll.Len() > c.cap {
		back := c.ll.Back()
		if back == nil {
			return
		}
		c.ll.Remove(back)
		delete(c.items, back.Value.(*lruEntry[K, V]).key)
	}
}

func (c *lruCache[K, V]) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
