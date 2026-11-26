package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"unsafe"
)

var (
	ErrLocalChainBudget     = errors.New("imagefilter: local chain budget")
	ErrLocalChainSuperseded = errors.New("imagefilter: local chain superseded")
	ErrLocalChainClosed     = errors.New("imagefilter: local chain closed")
)

type LocalChainBudgetError struct{ Required, Available int64 }

func (e *LocalChainBudgetError) Error() string {
	return fmt.Sprintf("%v: need %d bytes, have %d", ErrLocalChainBudget, e.Required, e.Available)
}
func (e *LocalChainBudgetError) Unwrap() error { return ErrLocalChainBudget }

// LocalChainIdentity identifies an immutable source binding. SourceID and
// GeometryID are caller-owned identities (for example hashes), never inferred
// from pointers. Dims/SpacingMM are also part of the cache identity. A changed
// source must be rebound with ReplaceSource; data backing an active binding
// must stay immutable until its requests finish. Generation identifies the
// acquired snapshot, not a derived stage.
type LocalChainIdentity struct {
	SourceID, GeometryID [32]byte
	Generation           uint64
	Dims                 [3]int
	SpacingMM            [3]float64
}

type LocalChainSource struct {
	Identity LocalChainIdentity
	Reader   RegionReader
}

// LocalChainOptions has one logical array budget for cache, all intermediate
// regions, detached result, and the regional kernels' complete reservations.
// Runtime/allocator overhead and caller-owned source/results are external.
// Zero working bytes selects 256 MiB; CacheBytes=0 disables cache. Cache entries
// are independently bounded (8 by default, at most 128). TileSize partitions
// each stage through the existing regional driver. MaxReadBytes=0 disables the
// extra I/O bound; the source window is already bounded by working admission.
type LocalChainOptions struct {
	MaxWorkingBytes, CacheBytes, MaxReadBytes int64
	MaxCacheEntries                           int
	TileSize                                  [3]int
}

type LocalChainStagePlan struct {
	Filter        SlabFilter // frozen value; only bilateral or NLM
	Input, Output Region
	Regional      RegionalResult
	OutputBytes   int64
}
type LocalChainPlan struct {
	Identity                              LocalChainIdentity
	Stages                                int
	Stage                                 [3]LocalChainStagePlan
	Source, Requested                     Region
	CompositeHalo                         [3]int
	WorkingBytes, SourceReadBytes         int64
	CachePayloadLimit, CacheMetadataBytes int64
}

type LocalChainStats struct {
	Computed                           [3]bool
	CacheHitStage                      int // -1 means no reused stage
	SourceReadBytes, InternalReadBytes int64
	CacheBytesAfter, Evictions         int64
}

// LocalChainResult is an owned requested region, x-fastest. Publication is
// all-or-nothing: any read, kernel, cancellation or supersession error returns
// nil. No caller writer is invoked, and incomplete intermediate stages never
// enter the cache. Returned Data never aliases source or cached arrays.
type LocalChainResult struct {
	Region Region
	Data   []float32
	Plan   LocalChainPlan
	Stats  LocalChainStats
}

type localChainStage struct {
	filter  SlabFilter
	version uint64
}
type localChainKey struct {
	identity LocalChainIdentity
	region   Region
	prefix   [3]uint64
	stage    int
}
type localChainEntry struct {
	key   localChainKey
	data  []float32
	stamp uint64
}

// LocalChain composes exactly two or three existing local bilateral/NLM
// kernels. It is a small opt-in CPU preprocessing profile, not a graph/plugin
// framework or renderer. Requests share a context-aware serial admission gate,
// bounding aggregate active workspace. Configuration changes cancel active
// computation and invalidate affected stage descendants; immutable prefix
// regions remain reusable. Cache matches exact regions, not containing regions.
type LocalChain struct {
	mu       sync.Mutex
	gate     chan struct{}
	source   LocalChainSource
	stages   [3]localChainStage
	n        int
	opts     LocalChainOptions
	revision uint64
	active   context.CancelFunc
	closed   bool
	cache    []localChainEntry
	bytes    int64
	clock    uint64
	evicted  int64
}

func localChainAdd(parts ...int64) (int64, error) {
	var total int64
	for _, p := range parts {
		if p < 0 || total > math.MaxInt64-p {
			return 0, fmt.Errorf("%w: chain byte overflow", ErrInvalidVolume)
		}
		total += p
	}
	return total, nil
}
func localChainRegionBytes(r Region) (int64, error) {
	return regionalCheckedProduct(int64(r.Size[0]), int64(r.Size[1]), int64(r.Size[2]), 4)
}
func localChainIdentityValid(s LocalChainSource) error {
	if s.Reader == nil {
		return fmt.Errorf("%w: nil chain source", ErrInvalidVolume)
	}
	if _, ok := voxelCount(s.Identity.Dims); !ok {
		return fmt.Errorf("%w: chain dimensions", ErrInvalidVolume)
	}
	for a, spacing := range s.Identity.SpacingMM {
		if math.IsNaN(spacing) || math.IsInf(spacing, 0) || spacing < 0 || (s.Identity.Dims[a] > 1 && spacing == 0) {
			return fmt.Errorf("%w: chain spacing", ErrInvalidVolume)
		}
	}
	return nil
}
func localChainFilter(f SlabFilter, id LocalChainIdentity) (SlabFilter, error) {
	switch o := f.(type) {
	case BilateralOptions:
		f = o
	case *BilateralOptions:
		if o == nil {
			return nil, fmt.Errorf("%w: nil chain bilateral", ErrInvalidVolume)
		}
		f = *o
	case NonLocalMeansOptions:
		f = o
	case *NonLocalMeansOptions:
		if o == nil {
			return nil, fmt.Errorf("%w: nil chain NLM", ErrInvalidVolume)
		}
		f = *o
	default:
		return nil, fmt.Errorf("%w: chain admits only bilateral/NLM; global filters need an external materialized barrier", ErrInvalidVolume)
	}
	f = regionalFreezeWorkers(f)
	// Canonicalize documented scalar zero defaults before snapshot comparison:
	// making a scalar default explicit must not invalidate or cancel work.
	// Radius vectors keep their caller representation, including all-zero
	// defaults; changing that representation may invalidate an equal kernel.
	switch o := f.(type) {
	case BilateralOptions:
		if o.MaxKernelSamples == 0 {
			o.MaxKernelSamples = 1 << 16
		}
		f = o
	case NonLocalMeansOptions:
		if o.Beta == 0 {
			o.Beta = 1
		}
		if o.BlockStep == 0 {
			o.BlockStep = 2
		}
		if o.MaxSearchSamples == 0 {
			o.MaxSearchSamples = 1 << 16
		}
		f = o
	}
	_, err := regionalParams(f, id.Dims, id.SpacingMM)
	return f, err
}

func NewLocalChain(source LocalChainSource, filters []SlabFilter, opts LocalChainOptions) (*LocalChain, error) {
	if err := localChainIdentityValid(source); err != nil {
		return nil, err
	}
	if len(filters) < 2 || len(filters) > 3 || opts.MaxWorkingBytes < 0 || opts.CacheBytes < 0 || opts.MaxReadBytes < 0 || opts.MaxCacheEntries < 0 || opts.MaxCacheEntries > 128 {
		return nil, fmt.Errorf("%w: chain stage count/bounds", ErrInvalidVolume)
	}
	for _, t := range opts.TileSize {
		if t < 0 {
			return nil, fmt.Errorf("%w: chain tile size", ErrInvalidVolume)
		}
	}
	if opts.MaxWorkingBytes == 0 {
		opts.MaxWorkingBytes = DefaultSlabWorkingBytes
	}
	if opts.CacheBytes == 0 {
		opts.MaxCacheEntries = 0
	} else if opts.MaxCacheEntries == 0 {
		opts.MaxCacheEntries = 8
	}
	metadata := int64(opts.MaxCacheEntries) * int64(unsafe.Sizeof(localChainEntry{}))
	required, err := localChainAdd(opts.CacheBytes, metadata, 4096)
	if err != nil {
		return nil, err
	}
	if required > opts.MaxWorkingBytes {
		return nil, &LocalChainBudgetError{required, opts.MaxWorkingBytes}
	}
	c := &LocalChain{source: source, n: len(filters), opts: opts, revision: 1, gate: make(chan struct{}, 1)}
	for i, f := range filters {
		frozen, err := localChainFilter(f, source.Identity)
		if err != nil {
			return nil, err
		}
		c.stages[i] = localChainStage{frozen, 1}
	}
	c.cache = make([]localChainEntry, 0, opts.MaxCacheEntries)
	return c, nil
}

func (c *LocalChain) planLocked(requested Region) (LocalChainPlan, error) {
	p := LocalChainPlan{Identity: c.source.Identity, Stages: c.n, Requested: requested,
		CachePayloadLimit: c.opts.CacheBytes, CacheMetadataBytes: int64(c.opts.MaxCacheEntries) * int64(unsafe.Sizeof(localChainEntry{}))}
	if !regionalValid(requested, p.Identity.Dims) {
		return p, fmt.Errorf("%w: chain requested region", ErrInvalidVolume)
	}
	region := requested
	outputBytes, err := localChainRegionBytes(requested)
	if err != nil {
		return p, err
	}
	total, err := localChainAdd(c.opts.CacheBytes, p.CacheMetadataBytes, 4096, outputBytes)
	if err != nil {
		return p, err
	}
	for i := c.n - 1; i >= 0; i-- {
		f := c.stages[i].filter
		k, e := regionalParams(f, p.Identity.Dims, p.Identity.SpacingMM)
		if e != nil {
			return p, e
		}
		for a, halo := range k.radius {
			if p.CompositeHalo[a] > math.MaxInt-halo {
				return p, fmt.Errorf("%w: composite halo overflow", ErrInvalidVolume)
			}
			p.CompositeHalo[a] += halo
		}
		input := regionalWindow(region, p.Identity.Dims, k.radius)
		regional, e := PlanRegions(f, p.Identity.Dims, p.Identity.SpacingMM, region,
			RegionalOptions{MaxWorkingBytes: c.opts.MaxWorkingBytes, TileSize: c.opts.TileSize})
		if e != nil {
			return p, e
		}
		bytes, e := localChainRegionBytes(region)
		if e != nil {
			return p, e
		}
		p.Stage[i] = LocalChainStagePlan{f, input, region, regional, bytes}
		// Conservative sum: all stage outputs plus all regional reservations
		// coexist logically even though kernels execute sequentially. Cache-hit
		// references are also charged as stage output, covering concurrent clear.
		total, e = localChainAdd(total, bytes, regional.WorkingBytes)
		if e != nil {
			return p, e
		}
		region = input
	}
	p.Source = region
	p.SourceReadBytes, err = localChainRegionBytes(region)
	if err != nil {
		return p, err
	}
	p.WorkingBytes, err = localChainAdd(total, p.SourceReadBytes)
	if err != nil {
		return p, err
	}
	if p.WorkingBytes > c.opts.MaxWorkingBytes {
		return p, &LocalChainBudgetError{p.WorkingBytes, c.opts.MaxWorkingBytes}
	}
	if c.opts.MaxReadBytes > 0 && p.SourceReadBytes > c.opts.MaxReadBytes {
		return p, &LocalChainBudgetError{p.SourceReadBytes, c.opts.MaxReadBytes}
	}
	return p, nil
}

// Plan freezes all effective parameters and propagates per-axis clipped halos
// backward. It reserves before any source I/O or request data allocation.
func (c *LocalChain) Plan(requested Region) (LocalChainPlan, error) {
	if c == nil {
		return LocalChainPlan{}, ErrLocalChainClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return LocalChainPlan{}, ErrLocalChainClosed
	}
	return c.planLocked(requested)
}

func (c *LocalChain) keyLocked(stage int, region Region) localChainKey {
	k := localChainKey{identity: c.source.Identity, stage: stage, region: region}
	for i := 0; i <= stage; i++ {
		k.prefix[i] = c.stages[i].version
	}
	return k
}
func (c *LocalChain) removeLocked(i int, eviction bool) {
	c.bytes -= int64(len(c.cache[i].data)) * 4
	copy(c.cache[i:], c.cache[i+1:])
	c.cache[len(c.cache)-1] = localChainEntry{}
	c.cache = c.cache[:len(c.cache)-1]
	if eviction {
		c.evicted++
	}
}
func (c *LocalChain) clearLocked(first int) {
	for i := len(c.cache) - 1; i >= 0; i-- {
		if c.cache[i].key.stage >= first {
			c.removeLocked(i, false)
		}
	}
}
func (c *LocalChain) changedLocked() {
	c.revision++
	if c.active != nil {
		c.active()
	}
}

// SetStage replaces one parameter snapshot. Only this stage and descendants
// are invalidated. Identical snapshots after worker/scalar-default normalization
// are a no-op. Radius vectors preserve the caller's millimetre representation;
// zero-default versus explicit equivalent radii can supersede active work and
// invalidate descendants. Enlarged halos may
// miss an otherwise valid exact-region prefix entry and require recomputation.
func (c *LocalChain) SetStage(index int, filter SlabFilter) error {
	if c == nil {
		return ErrLocalChainClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrLocalChainClosed
	}
	if index < 0 || index >= c.n {
		return fmt.Errorf("%w: chain stage index", ErrInvalidVolume)
	}
	f, err := localChainFilter(filter, c.source.Identity)
	if err != nil {
		return err
	}
	if f == c.stages[index].filter {
		return nil
	}
	c.stages[index].filter = f
	c.stages[index].version++
	c.clearLocked(index)
	c.changedLocked()
	return nil
}

// ReplaceSource invalidates every dependent entry, even when the caller reuses
// the same identity. The previous binding must remain stable while its canceled
// request joins. Rebinding does not transfer or close caller-owned handles.
func (c *LocalChain) ReplaceSource(source LocalChainSource) error {
	if c == nil {
		return ErrLocalChainClosed
	}
	if err := localChainIdentityValid(source); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrLocalChainClosed
	}
	for i := 0; i < c.n; i++ {
		if _, err := localChainFilter(c.stages[i].filter, source.Identity); err != nil {
			return err
		}
	}
	c.source = source
	c.clearLocked(0)
	c.changedLocked()
	return nil
}

func (c *LocalChain) ClearCache() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.clearLocked(0)
	c.mu.Unlock()
}
func (c *LocalChain) Close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.closed = true
	c.clearLocked(0)
	c.changedLocked()
	// Active requests already hold their immutable local snapshots. Release
	// caller handles and idle cache metadata even if the closed handle lives on;
	// the active request releases its own source reference when it joins.
	c.source = LocalChainSource{}
	c.stages = [3]localChainStage{}
	c.cache = nil
	c.mu.Unlock()
}
func (c *LocalChain) CacheBytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

func (c *LocalChain) cachePutLocked(key localChainKey, data []float32) {
	bytes := int64(len(data)) * 4
	if bytes > c.opts.CacheBytes || c.opts.MaxCacheEntries == 0 {
		return
	}
	for i := range c.cache {
		if c.cache[i].key == key {
			return
		}
	}
	for c.bytes > c.opts.CacheBytes-bytes || len(c.cache) >= c.opts.MaxCacheEntries {
		oldest := 0
		for i := range c.cache {
			if c.cache[i].stamp < c.cache[oldest].stamp {
				oldest = i
			}
		}
		c.removeLocked(oldest, true)
	}
	c.clock++
	c.cache = append(c.cache, localChainEntry{key, data, c.clock})
	c.bytes += bytes
}

// Run evaluates one requested output region. Concurrent requests wait without
// holding workspace and can cancel while queued. SetStage/ReplaceSource/Close
// fence both cache insertion and return publication of the active request.
func (c *LocalChain) Run(ctx context.Context, requested Region) (*LocalChainResult, error) {
	if c == nil {
		return nil, ErrLocalChainClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.gate }()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrLocalChainClosed
	}
	p, err := c.planLocked(requested)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	revision, source, opts := c.revision, c.source, c.opts
	activeCtx, cancel := context.WithCancel(ctx)
	c.active = cancel
	var keys [3]localChainKey
	for i := 0; i < p.Stages; i++ {
		keys[i] = c.keyLocked(i, p.Stage[i].Output)
	}
	stats := LocalChainStats{CacheHitStage: -1}
	start := 0
	var input *localChainBuffer
	for i := p.Stages - 1; i >= 0 && input == nil; i-- {
		for j := range c.cache {
			if c.cache[j].key == keys[i] {
				c.clock++
				c.cache[j].stamp = c.clock
				input = &localChainBuffer{p.Stage[i].Output, c.cache[j].data}
				stats.CacheHitStage = i
				start = i + 1
				break
			}
		}
	}
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		c.active = nil
		c.mu.Unlock()
	}()
	finishError := func(cause error) (*LocalChainResult, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed {
			return nil, ErrLocalChainClosed
		}
		if revision != c.revision {
			return nil, ErrLocalChainSuperseded
		}
		return nil, cause
	}
	if err = activeCtx.Err(); err != nil {
		return finishError(err)
	}
	if input == nil {
		count, _ := voxelCount(p.Source.Size)
		input = &localChainBuffer{p.Source, make([]float32, count)}
		if err = source.Reader.ReadRegion(activeCtx, p.Source, input.data); err != nil {
			return finishError(err)
		}
		stats.SourceReadBytes = p.SourceReadBytes
		for i, v := range input.data {
			if i%4096 == 0 {
				if err = activeCtx.Err(); err != nil {
					return finishError(err)
				}
			}
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return finishError(fmt.Errorf("%w: nonfinite chain source", ErrInvalidVolume))
			}
		}
	}
	var pending [3]*localChainBuffer
	for i := start; i < p.Stages; i++ {
		stage := p.Stage[i]
		count, _ := voxelCount(stage.Output.Size)
		out := &localChainBuffer{stage.Output, make([]float32, count)}
		res, e := FilterRegions(activeCtx, stage.Filter, input, p.Identity.Dims, p.Identity.SpacingMM,
			stage.Output, out, RegionalOptions{MaxWorkingBytes: opts.MaxWorkingBytes, TileSize: stage.Regional.TileSize})
		if e != nil {
			return finishError(e)
		}
		stats.Computed[i] = true
		stats.InternalReadBytes += res.ReadBytes
		pending[i] = out
		input = out
	}
	// The result copy keeps caller data detached even on a full final-stage hit.
	data := make([]float32, len(input.data))
	for i := 0; i < len(data); {
		if err = activeCtx.Err(); err != nil {
			return finishError(err)
		}
		end := i + min(4096, len(data)-i)
		copy(data[i:end], input.data[i:end])
		i = end
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrLocalChainClosed
	}
	if revision != c.revision {
		return nil, ErrLocalChainSuperseded
	}
	if err = activeCtx.Err(); err != nil {
		return nil, err
	}
	beforeEvictions := c.evicted
	for i, out := range pending {
		if out != nil {
			c.cachePutLocked(keys[i], out.data)
		}
	}
	stats.CacheBytesAfter, stats.Evictions = c.bytes, c.evicted-beforeEvictions
	return &LocalChainResult{requested, data, p, stats}, nil
}

// Internal readers/writers use compact strides but retain global coordinates.
// They reject any missing halo rather than inventing ROI boundaries or restarting
// NLM's global centre grid. Their arrays are owned by the chain admission plan.
type localChainBuffer struct {
	region Region
	data   []float32
}

func (b *localChainBuffer) copyRegion(ctx context.Context, r Region, data []float32, write bool) error {
	n, ok := voxelCount(r.Size)
	if !ok || len(data) != n {
		return fmt.Errorf("%w: chain region buffer", ErrInvalidVolume)
	}
	for a := 0; a < 3; a++ {
		if r.Origin[a] < b.region.Origin[a] || r.Size[a] > b.region.Size[a] || r.Origin[a]-b.region.Origin[a] > b.region.Size[a]-r.Size[a] {
			return fmt.Errorf("%w: chain missing halo", ErrInvalidVolume)
		}
	}
	for z := 0; z < r.Size[2]; z++ {
		for y := 0; y < r.Size[1]; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			bi := ((z+r.Origin[2]-b.region.Origin[2])*b.region.Size[1]+y+r.Origin[1]-b.region.Origin[1])*b.region.Size[0] + r.Origin[0] - b.region.Origin[0]
			ri := (z*r.Size[1] + y) * r.Size[0]
			if write {
				copy(b.data[bi:bi+r.Size[0]], data[ri:ri+r.Size[0]])
			} else {
				copy(data[ri:ri+r.Size[0]], b.data[bi:bi+r.Size[0]])
			}
		}
	}
	return nil
}
func (b *localChainBuffer) ReadRegion(ctx context.Context, r Region, dst []float32) error {
	return b.copyRegion(ctx, r, dst, false)
}
func (b *localChainBuffer) WriteRegion(ctx context.Context, r Region, src []float32) error {
	return b.copyRegion(ctx, r, src, true)
}
