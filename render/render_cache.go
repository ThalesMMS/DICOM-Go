package render

import (
	"bytes"
	"container/list"
	"fmt"
	"image"
	"image/png"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

const defaultRenderCacheBytes int64 = 128 << 20

type RenderCache struct {
	mu         sync.Mutex
	generation uint64

	maxDecodedBytes int64
	decodedBytes    int64
	decodedEntries  map[frameCacheKey]*list.Element
	decodedLRU      *list.List

	maxWindowBytes int64
	windowBytes    int64
	windowEntries  map[windowCacheKey]*list.Element
	windowLRU      *list.List

	pinnedDecoded map[frameCacheKey]bool
	pinnedWindow  map[windowCacheKey]bool
	// decoding holds the decode in progress of each frame, so concurrent
	// callers (a render, a read-ahead, a volume build) wait for one decode
	// instead of each decoding the frame (#1612).
	decoding  map[frameCacheKey]*decodeCall
	hits      uint64
	misses    uint64
	evictions uint64

	decodeFrame func(*Frame) (*decodedFrame, error)
}

type decodeCall struct {
	done  chan struct{}
	frame *decodedFrame
	err   error
}

type decodedCacheEntry struct {
	key   frameCacheKey
	frame *decodedFrame
	bytes int64
	// windowMisses counts windows rendered from this decoded plane;
	// levelsTried records that the level index was attempted (#1222).
	windowMisses int
	levelsTried  bool
}

// levelIndexWindowMisses is the windowed render of a decoded plane that first
// builds its level index. Scrolling renders each plane at one window and never
// pays for the index; a W/L drag reaches the second window at once.
const levelIndexWindowMisses = 2

type windowCacheKey struct {
	slice      frameCacheKey
	centerBits uint64
	widthBits  uint64
	function   int
	lut        *display.LUT
}

type windowCacheEntry struct {
	key   windowCacheKey
	image image.Image
	err   error
	bytes int64
}

func NewRenderCache(maxBytes int64) *RenderCache {
	if maxBytes <= 0 {
		maxBytes = defaultRenderCacheBytes
	}
	cache := &RenderCache{
		maxDecodedBytes: maxBytes,
		decodedEntries:  map[frameCacheKey]*list.Element{},
		decodedLRU:      list.New(),
		maxWindowBytes:  maxBytes,
		windowEntries:   map[windowCacheKey]*list.Element{},
		windowLRU:       list.New(),
		pinnedDecoded:   map[frameCacheKey]bool{},
		pinnedWindow:    map[windowCacheKey]bool{},
		decoding:        map[frameCacheKey]*decodeCall{},
	}
	cache.decodeFrame = decodeCacheFrame
	return cache
}

func (c *RenderCache) RenderFrame(slice *Frame, window WindowLevel) (image.Image, error) {
	if slice == nil {
		return blankImage(512, 512), fmt.Errorf("render: nil slice")
	}
	if slice.DecodeErr != nil {
		return blankImage(int(slice.Metadata.Columns), int(slice.Metadata.Rows)), slice.DecodeErr
	}
	generation := c.cacheGeneration()
	window = normalizedRenderWindow(slice, window)
	key := windowCacheKey{
		slice:      frameCacheIdentity(slice),
		centerBits: cacheFloatBits(window.Center),
		widthBits:  cacheFloatBits(window.Width),
		function:   int(window.Function),
		lut:        window.LUT,
	}
	if img, err, ok := c.cachedWindowImage(key); ok {
		c.recordResult(true)
		return img, err
	}
	c.recordResult(false)
	decoded, err := c.decodedAtGeneration(slice, generation)
	if err != nil {
		return blankImage(int(slice.Metadata.Columns), int(slice.Metadata.Rows)), err
	}
	decoded = c.levelIndexedFrame(slice, decoded)
	img, err := renderDecodedFrame(decoded, window)
	if img == nil {
		img = blankImage(int(slice.Metadata.Columns), int(slice.Metadata.Rows))
	}
	c.storeWindowImage(key, img, err, generation)
	return img, err
}

// ContainsFrame reports whether the fully windowed frame is resident. It also
// promotes a hit in the LRU, allowing an interactive viewer to pin its visible
// frame while bounded background prefetch continues.
func (c *RenderCache) ContainsFrame(slice *Frame, window WindowLevel) bool {
	if c == nil || slice == nil {
		return false
	}
	window = normalizedRenderWindow(slice, window)
	key := windowCacheKey{
		slice:      frameCacheIdentity(slice),
		centerBits: cacheFloatBits(window.Center),
		widthBits:  cacheFloatBits(window.Width),
		function:   int(window.Function),
		lut:        window.LUT,
	}
	_, _, ok := c.cachedWindowImage(key)
	return ok
}

// PinnedFrame identifies one active frame/window pair that pressure eviction
// must retain.
type PinnedFrame struct {
	Frame  *Frame
	Window WindowLevel
}

// CacheStats reports PHI-free cache occupancy and counters.
type CacheStats struct {
	DecodedBytes int64
	WindowBytes  int64
	PinnedBytes  int64
	Hits         uint64
	Misses       uint64
	Evictions    uint64
}

// SetPinnedFrames replaces the active-frame set. Pinned entries may exceed the
// local limit, but every non-pinned entry remains evictable.
func (c *RenderCache) SetPinnedFrames(frames []PinnedFrame) {
	if c == nil {
		return
	}
	decoded := make(map[frameCacheKey]bool, len(frames))
	windowed := make(map[windowCacheKey]bool, len(frames))
	for _, pinned := range frames {
		if pinned.Frame == nil {
			continue
		}
		decoded[frameCacheIdentity(pinned.Frame)] = true
		window := normalizedRenderWindow(pinned.Frame, pinned.Window)
		windowed[windowCacheKey{
			slice:      frameCacheIdentity(pinned.Frame),
			centerBits: cacheFloatBits(window.Center),
			widthBits:  cacheFloatBits(window.Width),
			function:   int(window.Function),
			lut:        window.LUT,
		}] = true
	}
	c.mu.Lock()
	c.pinnedDecoded = decoded
	c.pinnedWindow = windowed
	c.evictDecodedToLimitLocked()
	c.evictWindowToLimitLocked()
	c.mu.Unlock()
}

// EvictNonPinned removes all non-visible decoded and windowed entries.
func (c *RenderCache) EvictNonPinned() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	before := c.decodedBytes + c.windowBytes
	for elem := c.decodedLRU.Back(); elem != nil; {
		previous := elem.Prev()
		entry := elem.Value.(*decodedCacheEntry)
		if !c.pinnedDecoded[entry.key] {
			c.removeDecodedLocked(elem)
		}
		elem = previous
	}
	for elem := c.windowLRU.Back(); elem != nil; {
		previous := elem.Prev()
		entry := elem.Value.(*windowCacheEntry)
		if !c.pinnedWindow[entry.key] {
			c.removeWindowLocked(elem)
		}
		elem = previous
	}
	freed := before - c.decodedBytes - c.windowBytes
	c.mu.Unlock()
	return freed
}

// Stats returns a consistent cache snapshot.
func (c *RenderCache) Stats() CacheStats {
	if c == nil {
		return CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := CacheStats{
		DecodedBytes: c.decodedBytes,
		WindowBytes:  c.windowBytes,
		Hits:         c.hits,
		Misses:       c.misses,
		Evictions:    c.evictions,
	}
	for _, elem := range c.decodedEntries {
		entry := elem.Value.(*decodedCacheEntry)
		if c.pinnedDecoded[entry.key] {
			stats.PinnedBytes += entry.bytes
		}
	}
	for _, elem := range c.windowEntries {
		entry := elem.Value.(*windowCacheEntry)
		if c.pinnedWindow[entry.key] {
			stats.PinnedBytes += entry.bytes
		}
	}
	return stats
}

func (c *RenderCache) RenderFramePNG(slice *Frame, window WindowLevel) ([]byte, error) {
	img, err := c.RenderFrame(slice, window)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (c *RenderCache) RenderThumbnail(series *Stack) image.Image {
	if series == nil {
		return blankImage(128, 128)
	}
	img, err := c.RenderFrame(series.FirstDisplayFrame(), series.DefaultWindow)
	if err != nil || img == nil {
		return blankImage(128, 128)
	}
	return img
}

func (c *RenderCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	c.decodedBytes = 0
	c.decodedEntries = map[frameCacheKey]*list.Element{}
	c.decodedLRU.Init()
	c.windowBytes = 0
	c.windowEntries = map[windowCacheKey]*list.Element{}
	c.windowLRU.Init()
	c.pinnedDecoded = map[frameCacheKey]bool{}
	c.pinnedWindow = map[windowCacheKey]bool{}
}

func (c *RenderCache) cachedWindowImage(key windowCacheKey) (image.Image, error, bool) {
	if c == nil {
		return nil, nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem := c.windowEntries[key]
	if elem == nil {
		return nil, nil, false
	}
	c.windowLRU.MoveToFront(elem)
	entry := elem.Value.(*windowCacheEntry)
	return entry.image, entry.err, true
}

func (c *RenderCache) cacheGeneration() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

func (c *RenderCache) decoded(slice *Frame) (*decodedFrame, error) {
	return c.decodedAtGeneration(slice, c.cacheGeneration())
}

func (c *RenderCache) decodedAtGeneration(slice *Frame, generation uint64) (*decodedFrame, error) {
	if c == nil {
		return decodeCacheFrame(slice)
	}
	key := frameCacheIdentity(slice)
	c.mu.Lock()
	if elem := c.decodedEntries[key]; elem != nil {
		c.decodedLRU.MoveToFront(elem)
		entry := elem.Value.(*decodedCacheEntry)
		c.mu.Unlock()
		return entry.frame, nil
	}
	if call := c.decoding[key]; call != nil {
		c.mu.Unlock()
		<-call.done
		return call.frame, call.err
	}
	if c.decoding == nil {
		c.decoding = map[frameCacheKey]*decodeCall{}
	}
	call := &decodeCall{done: make(chan struct{})}
	c.decoding[key] = call
	c.mu.Unlock()

	call.frame, call.err = c.decodeFrame(slice)
	if call.err == nil {
		c.storeDecodedFrame(slice, call.frame, generation)
	}
	c.mu.Lock()
	delete(c.decoding, key)
	c.mu.Unlock()
	close(call.done)
	if call.err != nil {
		return nil, call.err
	}
	return call.frame, nil
}

// levelIndexedFrame counts one more window rendered from the cached decoded
// plane of slice and, from the second window on, replaces the cached plane
// with a copy that carries the level index (indexDecodedLevels), so every
// later window maps one value per level instead of one per pixel. The copy
// shares the modality values; renders already holding the previous plane keep
// using it unchanged. Planes that are not cached, or whose index was already
// attempted, are returned as they are.
func (c *RenderCache) levelIndexedFrame(slice *Frame, decoded *decodedFrame) *decodedFrame {
	if c == nil || decoded == nil || decoded.grayscale == nil || decoded.levels != nil {
		return decoded
	}
	c.mu.Lock()
	elem := c.decodedEntries[frameCacheIdentity(slice)]
	if elem == nil {
		c.mu.Unlock()
		return decoded
	}
	entry := elem.Value.(*decodedCacheEntry)
	if entry.frame != decoded || entry.levelsTried {
		current := entry.frame
		c.mu.Unlock()
		if current != nil && current.levels != nil {
			return current
		}
		return decoded
	}
	entry.windowMisses++
	if entry.windowMisses < levelIndexWindowMisses {
		c.mu.Unlock()
		return decoded
	}
	entry.levelsTried = true
	c.mu.Unlock()

	indexed := *decoded
	indexDecodedLevels(&indexed, slice)
	if indexed.levels == nil {
		return decoded
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current := c.decodedEntries[frameCacheIdentity(slice)]; current == elem && entry.frame == decoded {
		bytes := indexed.cacheBytes()
		c.decodedBytes += bytes - entry.bytes
		entry.frame = &indexed
		entry.bytes = bytes
		c.evictDecodedToLimitLocked()
	}
	return &indexed
}

func (c *RenderCache) storeDecodedFrame(slice *Frame, decoded *decodedFrame, generation uint64) {
	if c == nil || slice == nil || decoded == nil {
		return
	}
	bytes := decoded.cacheBytes()
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	if bytes > c.maxDecodedBytes && !c.pinnedDecoded[frameCacheIdentity(slice)] {
		return
	}
	if elem := c.decodedEntries[frameCacheIdentity(slice)]; elem != nil {
		entry := elem.Value.(*decodedCacheEntry)
		c.decodedBytes -= entry.bytes
		entry.frame = decoded
		entry.bytes = bytes
		entry.windowMisses = 0
		entry.levelsTried = false
		c.decodedBytes += bytes
		c.decodedLRU.MoveToFront(elem)
	} else {
		elem = c.decodedLRU.PushFront(&decodedCacheEntry{key: frameCacheIdentity(slice), frame: decoded, bytes: bytes})
		c.decodedEntries[frameCacheIdentity(slice)] = elem
		c.decodedBytes += bytes
	}
	c.evictDecodedToLimitLocked()
}

func (c *RenderCache) storeWindowImage(key windowCacheKey, img image.Image, err error, generation uint64) {
	if c == nil || img == nil {
		return
	}
	bytes := imageCacheBytes(img)
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	if bytes > c.maxWindowBytes && !c.pinnedWindow[key] {
		return
	}
	if elem := c.windowEntries[key]; elem != nil {
		entry := elem.Value.(*windowCacheEntry)
		c.windowBytes -= entry.bytes
		entry.image = img
		entry.err = err
		entry.bytes = bytes
		c.windowBytes += bytes
		c.windowLRU.MoveToFront(elem)
	} else {
		elem = c.windowLRU.PushFront(&windowCacheEntry{key: key, image: img, err: err, bytes: bytes})
		c.windowEntries[key] = elem
		c.windowBytes += bytes
	}
	c.evictWindowToLimitLocked()
}

func (c *RenderCache) evictDecodedToLimitLocked() {
	for c.decodedBytes > c.maxDecodedBytes {
		elem := c.decodedLRU.Back()
		for elem != nil && c.pinnedDecoded[elem.Value.(*decodedCacheEntry).key] {
			elem = elem.Prev()
		}
		if elem == nil {
			return
		}
		c.removeDecodedLocked(elem)
	}
}

func (c *RenderCache) evictWindowToLimitLocked() {
	for c.windowBytes > c.maxWindowBytes {
		elem := c.windowLRU.Back()
		for elem != nil && c.pinnedWindow[elem.Value.(*windowCacheEntry).key] {
			elem = elem.Prev()
		}
		if elem == nil {
			return
		}
		c.removeWindowLocked(elem)
	}
}

func (c *RenderCache) removeDecodedLocked(elem *list.Element) {
	entry := elem.Value.(*decodedCacheEntry)
	delete(c.decodedEntries, entry.key)
	c.decodedBytes -= entry.bytes
	c.decodedLRU.Remove(elem)
	c.evictions++
}

func (c *RenderCache) removeWindowLocked(elem *list.Element) {
	entry := elem.Value.(*windowCacheEntry)
	delete(c.windowEntries, entry.key)
	c.windowBytes -= entry.bytes
	c.windowLRU.Remove(elem)
	c.evictions++
}

func (c *RenderCache) recordResult(hit bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if hit {
		c.hits++
	} else {
		c.misses++
	}
	c.mu.Unlock()
}

func normalizedRenderWindow(slice *Frame, window WindowLevel) WindowLevel {
	if window.LUT != nil {
		return window
	}
	if window.Width <= 0 || math.IsNaN(window.Width) || math.IsInf(window.Width, 0) {
		if slice != nil {
			window = slice.DefaultWindow
		}
	}
	if window.Width <= 0 {
		window = WindowLevel{Center: defaultWindowCenter, Width: defaultWindowWidth}
	}
	return window
}

func cacheFloatBits(value float64) uint64 {
	if value == 0 {
		return 0
	}
	return math.Float64bits(value)
}

func imageCacheBytes(img image.Image) int64 {
	switch typed := img.(type) {
	case *image.Gray:
		return int64(len(typed.Pix))
	case *image.RGBA:
		return int64(len(typed.Pix))
	default:
		bounds := img.Bounds()
		return int64(bounds.Dx() * bounds.Dy() * 4)
	}
}

// Only one pointer is set. Tokens retain no source frame/pixels; nil-token
// callers preserve the original pointer-identity lifetime contract.
type frameCacheKey struct {
	frame    *Frame
	identity *FrameIdentity
}

func frameCacheIdentity(frame *Frame) frameCacheKey {
	if frame.CacheIdentity != nil {
		return frameCacheKey{identity: frame.CacheIdentity}
	}
	return frameCacheKey{frame: frame}
}

// LookupFrame returns an already windowed image atomically without loading or
// decoding source pixels. It promotes residency and records a hit only when
// found. A miss has no side effects. Inputs and returned images are immutable.
func (c *RenderCache) LookupFrame(frame *Frame, window WindowLevel) (image.Image, error, bool) {
	if c == nil || frame == nil {
		return nil, nil, false
	}
	window = normalizedRenderWindow(frame, window)
	key := windowCacheKey{slice: frameCacheIdentity(frame), centerBits: cacheFloatBits(window.Center), widthBits: cacheFloatBits(window.Width), function: int(window.Function), lut: window.LUT}
	img, err, ok := c.cachedWindowImage(key)
	if ok {
		c.recordResult(true)
	}
	return img, err, ok
}
