package icc

import (
	"container/list"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// ErrCacheResourceLimit is an explicit admission refusal, not an invalid ICC.
var ErrCacheResourceLimit = errors.New("icc: transform cache resource limit")

// CacheOptions bounds retained entries and conservative logical transform
// storage. The weight is 64 bytes per input byte plus 64 KiB per entry: it
// covers replicated sampled curves, LUT expansion, owned bytes and metadata.
// It is an admission bound, not a measurement of process heap usage.
type CacheOptions struct {
	MaxEntries      int
	MaxBytes        int64
	MaxProfileBytes int
}

// CachedTransform is immutable after construction. Transform can be used by
// concurrent tile workers; callers must not modify its exported Source field.
type CachedTransform struct {
	Transform   *Transform
	Description string
	Err         error
}
type cacheEntry struct {
	key    [32]byte
	result CachedTransform
	weight int64
}

// TransformCache shares content-addressed conversions, including failed
// conversions, in a bounded LRU. Construction is serialized, so a concurrent
// first use builds once and temporary profile expansion remains bounded.
type TransformCache struct {
	mu      sync.Mutex
	options CacheOptions
	entries map[[32]byte]*list.Element
	lru     list.List
	bytes   int64
	builds  uint64
}

func NewTransformCache(options CacheOptions) *TransformCache {
	if options.MaxEntries <= 0 {
		options.MaxEntries = 32
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 128 << 20
	}
	if options.MaxProfileBytes <= 0 {
		options.MaxProfileBytes = 1 << 20
	}
	options.MaxProfileBytes = min(options.MaxProfileBytes, MaxProfileBytes)
	return &TransformCache{options: options, entries: make(map[[32]byte]*list.Element)}
}

func (c *TransformCache) Resolve(data []byte) CachedTransform {
	if c == nil {
		return CachedTransform{Err: ErrCacheResourceLimit}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	weight := int64(len(data))*64 + 64<<10
	if len(data) > c.options.MaxProfileBytes || weight > c.options.MaxBytes {
		return CachedTransform{Err: ErrCacheResourceLimit}
	}
	key := sha256.Sum256(data)
	if entry := c.entries[key]; entry != nil {
		c.lru.MoveToFront(entry)
		return entry.Value.(cacheEntry).result
	}
	// Copy first: Parse borrows input, and closures may retain parsed values.
	owned := append([]byte(nil), data...)
	result := CachedTransform{}
	profile, err := Parse(owned)
	if err == nil {
		result.Description = strings.TrimSpace(profile.Description())
		if len(result.Description) > 48 {
			result.Description = result.Description[:48]
		}
		result.Transform, err = NewSRGBTransform(profile)
	}
	result.Err = err
	c.builds++
	for c.lru.Len() >= c.options.MaxEntries || c.bytes+weight > c.options.MaxBytes {
		last := c.lru.Back()
		if last == nil {
			break
		}
		old := last.Value.(cacheEntry)
		delete(c.entries, old.key)
		c.bytes -= old.weight
		c.lru.Remove(last)
	}
	entry := c.lru.PushFront(cacheEntry{key: key, result: result, weight: weight})
	c.entries[key] = entry
	c.bytes += weight
	return result
}

// CacheStats exposes logical bounds and constructions without exposing inputs.
type CacheStats struct {
	Entries int
	Bytes   int64
	Builds  uint64
}

func (c *TransformCache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{Entries: c.lru.Len(), Bytes: c.bytes, Builds: c.builds}
}
func (c *TransformCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[[32]byte]*list.Element)
	c.lru.Init()
	c.bytes = 0
}

func (r CachedTransform) String() string {
	if r.Err != nil {
		return fmt.Sprintf("ICC unavailable: %v", r.Err)
	}
	if r.Transform == nil {
		return "ICC unavailable"
	}
	if r.Transform.Identity() {
		return "ICC profile sRGB"
	}
	name := r.Description
	if name == "" {
		name = "embedded profile"
	}
	return "ICC " + name + " converted to sRGB"
}
