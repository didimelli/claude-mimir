package main

import (
	"container/list"
	"sync"
	"time"
)

// Mimir's reasoning never reaches Claude Code, so its history has no thinking to replay. The gateway keeps the
// reasoning of each reply that called tools, keyed by model and the tool call ids Claude Code echoes back as tool_use
// ids, and puts it back as reasoning_content on the next request. In memory only: a restart loses it, which costs a
// re-think.
const (
	reasonBudget   = 64 << 20  // total bytes kept; least recently used entries go first
	reasonMaxBytes = 200 << 10 // larger reasoning is not kept
	reasonTTL      = 2 * time.Hour
)

type reasonCache struct {
	mu       sync.Mutex
	budget   int // bytes of keys and texts; ids of one reply share a string but each counts it, an upper bound
	size     int
	maxBytes int
	ttl      time.Duration
	now      func() time.Time
	order    *list.List // of *reasonEntry, most recently used first
	items    map[string]*list.Element
}

type reasonEntry struct {
	key, text string
	at        time.Time
}

func newReasonCache(budget, maxBytes int, ttl time.Duration) *reasonCache {
	return &reasonCache{budget: budget, maxBytes: maxBytes, ttl: ttl, now: time.Now, order: list.New(), items: map[string]*list.Element{}}
}

// tool call ids are unique only per model: a switch mid-session must not hand one model another's reasoning
func reasonKey(model, id string) string { return model + "\x00" + id }

// put stores text under every id of model and reports whether it did.
func (c *reasonCache) put(model string, ids []string, text string) bool {
	if c == nil || text == "" || len(ids) == 0 || len(text) > c.maxBytes {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, id := range ids {
		key := reasonKey(model, id)
		if e, ok := c.items[key]; ok {
			c.drop(e)
		}
		c.items[key] = c.order.PushFront(&reasonEntry{key: key, text: text, at: now})
		c.size += len(key) + len(text)
	}
	for c.size > c.budget {
		c.drop(c.order.Back())
	}
	return true
}

func (c *reasonCache) get(model, id string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[reasonKey(model, id)]
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

func (c *reasonCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *reasonCache) drop(e *list.Element) {
	en := e.Value.(*reasonEntry)
	delete(c.items, en.key)
	c.size -= len(en.key) + len(en.text)
	c.order.Remove(e)
}
