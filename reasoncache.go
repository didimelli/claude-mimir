package main

import (
	"container/list"
	"sync"
	"time"
)

// Mimir's reasoning never reaches Claude Code, so its history has no thinking to replay. The gateway keeps the
// reasoning of each reply that called tools, keyed by the tool call ids Claude Code echoes back as tool_use ids, and
// puts it back as reasoning_content on the next request. In memory only: a restart loses it, which costs a re-think.
const (
	reasonMaxEntries = 2000
	reasonMaxBytes   = 200 << 10 // larger reasoning is not kept
	reasonTTL        = 2 * time.Hour
)

type reasonCache struct {
	mu       sync.Mutex
	max      int
	maxBytes int
	ttl      time.Duration
	now      func() time.Time
	order    *list.List // of *reasonEntry, most recently used first
	items    map[string]*list.Element
}

type reasonEntry struct {
	id, text string
	at       time.Time
}

func newReasonCache(max, maxBytes int, ttl time.Duration) *reasonCache {
	return &reasonCache{max: max, maxBytes: maxBytes, ttl: ttl, now: time.Now, order: list.New(), items: map[string]*list.Element{}}
}

// put stores text under every id and reports whether it did.
func (c *reasonCache) put(ids []string, text string) bool {
	if c == nil || text == "" || len(ids) == 0 || len(text) > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, id := range ids {
		if e, ok := c.items[id]; ok {
			c.order.Remove(e)
		}
		c.items[id] = c.order.PushFront(&reasonEntry{id: id, text: text, at: now})
	}
	for c.order.Len() > c.max {
		c.drop(c.order.Back())
	}
	return true
}

func (c *reasonCache) get(id string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[id]
	if !ok {
		return "", false
	}
	en := e.Value.(*reasonEntry)
	if c.now().Sub(en.at) > c.ttl {
		c.drop(e)
		return "", false
	}
	c.order.MoveToFront(e)
	return en.text, true
}

func (c *reasonCache) drop(e *list.Element) {
	delete(c.items, e.Value.(*reasonEntry).id)
	c.order.Remove(e)
}
