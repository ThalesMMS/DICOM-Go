package render

import (
	"container/list"
	"context"
	"sync"
)

const (
	defaultSourceConvolutionCacheBytes   = 64 << 20
	defaultSourceConvolutionCacheEntries = 16
)

type SourceConvolutionCacheStats struct {
	RetainedBytes int64
	Entries       int
	Computations  uint64
}

// SourceConvolutionCache retains immutable pre-VOI results by source/frame
// generation and kernel. Window, level, and VOI selection are independent.
// Callers must replace Frame.CacheIdentity whenever acquired pixels or the
// retained frame interpretation (layout, modality, padding, colour) change.
type SourceConvolutionCache struct {
	mu           sync.Mutex
	generation   uint64
	maxBytes     int64
	bytes        int64
	computations uint64
	entries      map[sourceConvolutionKey]*list.Element
	lru          list.List
	work         chan struct{}
}

type sourceConvolutionKey struct {
	identity *FrameIdentity
	frame    int
	kernel   SourceKernel3x3
}

type sourceConvolutionEntry struct {
	key      sourceConvolutionKey
	prepared *ConvolvedFrame
}

func NewSourceConvolutionCache(maxBytes int64) *SourceConvolutionCache {
	if maxBytes <= 0 {
		maxBytes = defaultSourceConvolutionCacheBytes
	}
	return &SourceConvolutionCache{maxBytes: maxBytes}
}

// Lookup promotes a resident result without opening the source. Only the
// immutable frame identity token and physical frame index are needed.
func (c *SourceConvolutionCache) Lookup(frame *Frame, kernel SourceKernel3x3) (*ConvolvedFrame, bool) {
	if c == nil || frame == nil || frame.CacheIdentity == nil {
		return nil, false
	}
	key := sourceConvolutionKey{identity: frame.CacheIdentity, frame: frame.FrameIndex, kernel: kernel}
	c.mu.Lock()
	defer c.mu.Unlock()
	prepared := c.lookupLocked(key)
	return prepared, prepared != nil
}

func (c *SourceConvolutionCache) lookupLocked(key sourceConvolutionKey) *ConvolvedFrame {
	if entry := c.entries[key]; entry != nil {
		c.lru.MoveToFront(entry)
		return entry.Value.(sourceConvolutionEntry).prepared
	}
	return nil
}

// PrepareContext serializes cold convolution for a cache. A waiting window
// change reuses the completed result even when its window differs. Frames
// without immutable source tokens can be prepared without cache retention.
func (c *SourceConvolutionCache) PrepareContext(ctx context.Context, frame *Frame, kernel SourceKernel3x3) (*ConvolvedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || frame == nil || frame.CacheIdentity == nil {
		return PrepareFrameConvolution3x3Context(ctx, frame, kernel)
	}

	key := sourceConvolutionKey{identity: frame.CacheIdentity, frame: frame.FrameIndex, kernel: kernel}
	c.mu.Lock()
	generation := c.generation
	if c.work == nil {
		c.work = make(chan struct{}, 1)
		c.entries = make(map[sourceConvolutionKey]*list.Element)
		if c.maxBytes <= 0 {
			c.maxBytes = defaultSourceConvolutionCacheBytes
		}
	}
	prepared := c.lookupLocked(key)
	work := c.work
	maxBytes := c.maxBytes
	c.mu.Unlock()
	if prepared != nil {
		return prepared, nil
	}
	if err := checkSourceConvolutionSize(frame.Metadata); err != nil {
		return nil, err
	}
	bytesPerPixel := int64(8)
	var lutBytes int64
	if frame.Metadata.SamplesPerPixel == 3 {
		bytesPerPixel = 4
	} else if frame.DefaultWindow.LUT != nil {
		lutBytes = int64(len(frame.DefaultWindow.LUT.Entries)) * 2
	}
	if int64(frame.Metadata.Columns)*int64(frame.Metadata.Rows)*bytesPerPixel+lutBytes > maxBytes {
		return nil, ErrSourceConvolutionLimit
	}
	select {
	case work <- struct{}{}:
		defer func() { <-work }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	prepared = c.lookupLocked(key)
	if prepared == nil {
		c.computations++
	}
	c.mu.Unlock()
	if prepared != nil {
		return prepared, nil
	}

	prepared, err := PrepareFrameConvolution3x3Context(ctx, frame, kernel)
	if err != nil {
		return nil, err
	}
	bytes := prepared.MemoryBytes()
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if generation == c.generation && bytes <= c.maxBytes {
		for c.lru.Len() >= defaultSourceConvolutionCacheEntries || bytes > c.maxBytes-c.bytes {
			old := c.lru.Back()
			entry := old.Value.(sourceConvolutionEntry)
			c.bytes -= entry.prepared.MemoryBytes()
			delete(c.entries, entry.key)
			c.lru.Remove(old)
		}
		c.entries[key] = c.lru.PushFront(sourceConvolutionEntry{key: key, prepared: prepared})
		c.bytes += bytes
	}
	return prepared, nil
}

// Clear drops residency and prevents work that was queued or in flight before
// the clear from repopulating this generation.
func (c *SourceConvolutionCache) Clear() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	freed := c.bytes
	c.generation++
	clear(c.entries)
	c.lru.Init()
	c.bytes = 0
	return freed
}

func (c *SourceConvolutionCache) Stats() SourceConvolutionCacheStats {
	if c == nil {
		return SourceConvolutionCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return SourceConvolutionCacheStats{RetainedBytes: c.bytes, Entries: c.lru.Len(), Computations: c.computations}
}
