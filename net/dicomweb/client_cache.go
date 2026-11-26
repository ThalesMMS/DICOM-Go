package dicomweb

import (
	"bytes"
	"container/list"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ResponseCache is an optional in-memory cache of successful buffered GET
// responses (QIDO-RS searches, WADO-RS metadata and buffered object
// retrieval), keyed by URL, Accept header, and redirect origin requirements,
// and bounded in bytes with least-recently-used eviction. Streaming
// retrievals, Verify, STOW-RS, and responses marked Cache-Control: no-store
// are never cached.
//
// The key does not include credentials: share one cache only between clients
// authorized for the same data. A ResponseCache is safe for concurrent use.
type ResponseCache struct {
	mu       sync.Mutex
	maxBytes int64
	ttl      time.Duration
	now      func() time.Time
	size     int64
	order    *list.List
	entries  map[responseCacheKey]*list.Element
	hits     uint64
	misses   uint64
}

// ResponseCacheStats reports the occupancy and effectiveness of a cache.
type ResponseCacheStats struct {
	Entries int
	Bytes   int64
	Hits    uint64
	Misses  uint64
}

type responseCacheKey struct {
	url         string
	accept      string
	originBound bool
}

type bufferedGET struct {
	Response
	// Operational resolution context stays private to the buffered read.
	redirectedURL string
}

type responseCacheEntry struct {
	key      responseCacheKey
	response bufferedGET
	size     int64
	expires  time.Time
}

// NewResponseCache returns a cache holding at most maxBytes of responses.
// A positive ttl expires entries that old; zero keeps them until evicted.
// A non-positive maxBytes returns nil, which disables caching.
func NewResponseCache(maxBytes int64, ttl time.Duration) *ResponseCache {
	if maxBytes <= 0 {
		return nil
	}
	if ttl < 0 {
		ttl = 0
	}
	return &ResponseCache{
		maxBytes: maxBytes, ttl: ttl, now: time.Now,
		order: list.New(), entries: map[responseCacheKey]*list.Element{},
	}
}

// Stats returns the current entry count, bytes, hits and misses.
func (c *ResponseCache) Stats() ResponseCacheStats {
	if c == nil {
		return ResponseCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return ResponseCacheStats{Entries: len(c.entries), Bytes: c.size, Hits: c.hits, Misses: c.misses}
}

// Purge removes every entry.
func (c *ResponseCache) Purge() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.entries = map[responseCacheKey]*list.Element{}
	c.size = 0
}

func (c *ResponseCache) get(key responseCacheKey) (bufferedGET, bool) {
	if c == nil {
		return bufferedGET{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		c.misses++
		return bufferedGET{}, false
	}
	entry := element.Value.(*responseCacheEntry)
	if !entry.expires.IsZero() && !c.now().Before(entry.expires) {
		c.remove(element)
		c.misses++
		return bufferedGET{}, false
	}
	c.order.MoveToFront(element)
	c.hits++
	return entry.response.clone(), true
}

func (c *ResponseCache) put(key responseCacheKey, response bufferedGET) {
	if c == nil || response.StatusCode < 200 || response.StatusCode >= 300 || response.StatusCode == http.StatusPartialContent {
		return
	}
	for _, value := range response.Header.Values("Cache-Control") {
		if strings.Contains(strings.ToLower(value), "no-store") {
			return
		}
	}
	size := int64(len(response.Body) + len(key.url) + len(key.accept) + len(response.URL) + len(response.redirectedURL))
	for name, values := range response.Header {
		size += int64(len(name))
		for _, value := range values {
			size += int64(len(value))
		}
	}
	if size > c.maxBytes {
		return
	}
	entry := &responseCacheEntry{key: key, response: response.clone(), size: size}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ttl > 0 {
		entry.expires = c.now().Add(c.ttl)
	}
	if existing, ok := c.entries[key]; ok {
		c.remove(existing)
	}
	c.entries[key] = c.order.PushFront(entry)
	c.size += size
	for c.size > c.maxBytes {
		c.remove(c.order.Back())
	}
}

func (c *ResponseCache) remove(element *list.Element) {
	entry := c.order.Remove(element).(*responseCacheEntry)
	delete(c.entries, entry.key)
	c.size -= entry.size
}

func (response bufferedGET) clone() bufferedGET {
	response.Header = response.Header.Clone()
	response.Body = bytes.Clone(response.Body)
	return response
}
