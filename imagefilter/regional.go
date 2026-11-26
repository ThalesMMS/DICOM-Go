package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// Region identifies x-fastest samples in global volume coordinates.
type Region struct{ Origin, Size [3]int }

// RegionReader fills a complete region. It must honor ctx, fill every sample,
// and neither retain dst nor mutate the source during one filtering request.
type RegionReader interface {
	ReadRegion(context.Context, Region, []float32) error
}

// RegionWriter accepts a complete tile. It must honor ctx and not retain src.
// A failing writer may have changed its own destination; no rollback is promised.
type RegionWriter interface {
	WriteRegion(context.Context, Region, []float32) error
}

// ErrRegionalBudget identifies a request rejected before kernel/buffer allocation.
var ErrRegionalBudget = errors.New("imagefilter: regional working budget")

type RegionalBudgetError struct{ Required, Available int64 }

func (e *RegionalBudgetError) Error() string {
	return fmt.Sprintf("%v: need %d bytes, have %d", ErrRegionalBudget, e.Required, e.Available)
}
func (e *RegionalBudgetError) Unwrap() error { return ErrRegionalBudget }

// RegionalOptions bounds request-owned backing arrays, including parameter
// arrays and per-worker scratch. Reader/writer storage, runtime stacks and
// allocator overhead are external. Tiles run serially; kernel workers share
// the existing parallel budget. Zero chooses the slab default working bound.
type RegionalOptions struct {
	MaxWorkingBytes int64
	TileSize        [3]int
	MaxReadBytes    int64
}
type RegionalResult struct {
	TileSize, Halo                            [3]int
	WorkingBytes, PlannedReadBytes, ReadBytes int64
	Tiles                                     int
}

type regionalKernel struct {
	kind              int
	radius            [3]int
	workers, blockLen int
	taps              int64
	nlm               regionalNLMParams
	runtime           *nlmKernel
	filter            SlabFilter
}

func regionalCheckedProduct(values ...int64) (int64, error) {
	n := int64(1)
	for _, v := range values {
		if v < 0 || (v != 0 && n > math.MaxInt64/v) {
			return 0, fmt.Errorf("%w: regional arithmetic overflow", ErrInvalidVolume)
		}
		n *= v
	}
	return n, nil
}
func regionalParams(f SlabFilter, dims [3]int, spacing [3]float64) (regionalKernel, error) {
	k := regionalKernel{filter: f}
	var err error
	switch o := f.(type) {
	case *MedianOptions:
		if o == nil {
			return k, fmt.Errorf("%w: nil median options", ErrInvalidVolume)
		}
		return regionalParams(*o, dims, spacing)
	case *BilateralOptions:
		if o == nil {
			return k, fmt.Errorf("%w: nil bilateral options", ErrInvalidVolume)
		}
		return regionalParams(*o, dims, spacing)
	case *NonLocalMeansOptions:
		if o == nil {
			return k, fmt.Errorf("%w: nil NLM options", ErrInvalidVolume)
		}
		return regionalParams(*o, dims, spacing)
	}
	switch o := f.(type) {
	case MedianOptions:
		k.kind = 1
		k.workers = o.Workers
		k.radius, err = MedianRadiusSamples(o, dims, spacing)
		if err != nil {
			return k, err
		}
		if !regionalRadiiValid(k.radius) {
			return k, fmt.Errorf("%w: median radius overflow", ErrInvalidVolume)
		}
		limit := o.MaxKernelSamples
		if limit == 0 {
			limit = 1 << 16
		}
		n, e := regionalCheckedProduct(int64(2*k.radius[0]+1), int64(2*k.radius[1]+1), int64(2*k.radius[2]+1))
		if e != nil || limit < 0 || n > int64(limit) {
			return k, fmt.Errorf("%w: median box bound", ErrInvalidVolume)
		}
		k.blockLen = int(n)
	case BilateralOptions:
		k.kind = 2
		k.workers = o.Workers
		if !(o.RangeSigma > 0) || math.IsInf(o.RangeSigma, 0) {
			return k, fmt.Errorf("%w: range sigma", ErrInvalidVolume)
		}
		limit := o.MaxKernelSamples
		if limit == 0 {
			limit = 1 << 16
		}
		if limit < 1 {
			return k, fmt.Errorf("%w: bilateral bound", ErrInvalidVolume)
		}
		for a, s := range o.SigmaMM {
			if !(s >= 0) || math.IsInf(s, 0) {
				return k, fmt.Errorf("%w: bilateral sigma", ErrInvalidVolume)
			}
			if s > 0 && dims[a] > 1 {
				r := math.Ceil(2.5 * s / spacing[a])
				if math.IsInf(r, 0) || r > float64((math.MaxInt-1)/2) {
					return k, fmt.Errorf("%w: bilateral radius overflow", ErrInvalidVolume)
				}
				k.radius[a] = int(r)
			}
		}
		n, e := regionalCheckedProduct(int64(2*k.radius[0]+1), int64(2*k.radius[1]+1), int64(2*k.radius[2]+1))
		if e != nil || n > int64(limit) {
			return k, fmt.Errorf("%w: bilateral kernel bound", ErrInvalidVolume)
		}
		k.taps = n
	case NonLocalMeansOptions:
		k.kind = 3
		k.workers = o.Workers
		if !(o.NoiseSigma > 0) || math.IsInf(o.NoiseSigma, 0) {
			return k, fmt.Errorf("%w: noise sigma", ErrInvalidVolume)
		}
		if o.Beta == 0 {
			o.Beta = 1
		}
		if !(o.Beta > 0) || math.IsInf(o.Beta, 0) {
			return k, fmt.Errorf("%w: beta", ErrInvalidVolume)
		}
		if o.BlockStep == 0 {
			o.BlockStep = 2
		}
		if o.BlockStep < 0 {
			return k, fmt.Errorf("%w: block step", ErrInvalidVolume)
		}
		if o.MaxSearchSamples == 0 {
			o.MaxSearchSamples = 1 << 16
		}
		k.nlm.dims = dims
		k.nlm.workers = o.Workers
		k.nlm.patch, err = radiusOrDefault("patch", o.PatchRadiusMM, 1, dims, spacing)
		if err != nil {
			return k, err
		}
		k.nlm.search, err = radiusOrDefault("search", o.SearchRadiusMM, 5, dims, spacing)
		if err != nil {
			return k, err
		}
		if !regionalRadiiValid(k.nlm.patch) || !regionalRadiiValid(k.nlm.search) {
			return k, fmt.Errorf("%w: NLM radius overflow", ErrInvalidVolume)
		}
		n, e := regionalCheckedProduct(int64(2*k.nlm.search[0]+1), int64(2*k.nlm.search[1]+1), int64(2*k.nlm.search[2]+1))
		if e != nil || o.MaxSearchSamples < 1 || n > int64(o.MaxSearchSamples) {
			return k, fmt.Errorf("%w: NLM search bound", ErrInvalidVolume)
		}
		n, e = regionalCheckedProduct(int64(2*k.nlm.patch[0]+1), int64(2*k.nlm.patch[1]+1), int64(2*k.nlm.patch[2]+1))
		if e != nil || n > int64(math.MaxInt) {
			return k, fmt.Errorf("%w: NLM patch overflow", ErrInvalidVolume)
		}
		k.blockLen = int(n)
		k.nlm.blockLen = int(n)
		for a, r := range k.nlm.patch {
			k.nlm.step[a] = min(o.BlockStep, 2*r+1)
			if r > (math.MaxInt-k.nlm.search[a])/2 {
				return k, fmt.Errorf("%w: halo overflow", ErrInvalidVolume)
			}
			k.radius[a] = 2*r + k.nlm.search[a]
		}
		h2 := 2 * o.Beta * o.NoiseSigma * o.NoiseSigma
		if !(h2 > 0) || math.IsInf(h2, 0) {
			return k, fmt.Errorf("%w: noise scale", ErrInvalidVolume)
		}
		k.nlm.invH2 = 1 / h2
		k.nlm.cutoff = nlmCutoff * h2
	default:
		return k, fmt.Errorf("%w: filter is not regional median, bilateral or NLM", ErrInvalidVolume)
	}
	k.workers = workerBound(k.workers)
	return k, nil
}
func regionalValid(r Region, dims [3]int) bool {
	for a, n := range dims {
		if r.Origin[a] < 0 || r.Size[a] <= 0 || r.Origin[a] > n-r.Size[a] {
			return false
		}
	}
	return true
}
func regionalWindow(r Region, dims, halo [3]int) Region {
	var w Region
	for a, n := range dims {
		w.Origin[a] = max(0, r.Origin[a]-min(halo[a], n))
		end := r.Origin[a] + r.Size[a]
		w.Size[a] = min(n, end+min(halo[a], n-end)) - w.Origin[a]
	}
	return w
}

// PlanRegions validates and budgets before allocating any kernel or data array.
func PlanRegions(f SlabFilter, dims [3]int, spacing [3]float64, requested Region, opts RegionalOptions) (RegionalResult, error) {
	var res RegionalResult
	if _, ok := voxelCount(dims); !ok || !regionalValid(requested, dims) {
		return res, fmt.Errorf("%w: regional dimensions/request", ErrInvalidVolume)
	}
	for a, s := range spacing {
		if dims[a] > 1 && !(s > 0 && s <= math.MaxFloat64) {
			return res, fmt.Errorf("%w: spacing", ErrInvalidVolume)
		}
	}
	k, err := regionalParams(f, dims, spacing)
	if err != nil {
		return res, err
	}
	res.Halo = k.radius
	for axis, halo := range k.radius {
		if dims[axis]-1 > math.MaxInt-halo {
			return res, fmt.Errorf("%w: global coordinate plus halo overflows axis %d", ErrInvalidVolume, axis)
		}
	}
	budget := opts.MaxWorkingBytes
	if budget == 0 {
		budget = DefaultSlabWorkingBytes
	}
	if budget < 0 || opts.MaxReadBytes < 0 {
		return res, fmt.Errorf("%w: regional bounds", ErrInvalidVolume)
	}
	cost := func(tile [3]int) (int64, error) {
		var window [3]int
		for a, n := range dims {
			h := min(k.radius[a], n)
			window[a] = n
			if tile[a] <= n-2*min(h, n/2) {
				window[a] = min(n, tile[a]+2*h)
			}
		}
		ni, e := regionalCheckedProduct(int64(window[0]), int64(window[1]), int64(window[2]), 4)
		if e != nil {
			return 0, e
		}
		no, e := regionalCheckedProduct(int64(tile[0]), int64(tile[1]), int64(tile[2]), 4)
		if e != nil {
			return 0, e
		}
		scratch := int64(0)
		maxLines := tile[1] * tile[2]
		if k.kind == 3 {
			maxLines = max(maxLines, regionalExtent(dims[1], tile[1], k.nlm.patch[1]))
		}
		workers := int64(workerCount(k.workers, maxLines))
		switch k.kind {
		case 1:
			scratch, e = regionalCheckedProduct(int64(k.blockLen), 4, workers)
		case 2:
			scratch, e = regionalCheckedProduct(k.taps, 64) // upper bound append growth of 32-byte taps
		case 3:
			var centres [3]int64
			for a := range centres {
				centres[a] = int64(regionalExtent(dims[a], tile[a], k.nlm.patch[a]))
			}
			estimates, x := regionalCheckedProduct(centres[0], centres[1], centres[2], int64(k.blockLen), 4)
			if x != nil {
				return 0, x
			}
			perWorker, x := regionalCheckedProduct(workers, int64(k.blockLen), 24)
			if x != nil {
				return 0, x
			}
			offsets, x := regionalCheckedProduct(int64(2*k.nlm.patch[1]+1), int64(2*k.nlm.patch[2]+1), 16)
			if x != nil {
				return 0, x
			}
			metadata, x := regionalCheckedProduct(centres[0]+centres[1]+centres[2], 8)
			if x != nil {
				return 0, x
			}
			scratch = estimates
			for _, extra := range []int64{perWorker, offsets, metadata} {
				if scratch > math.MaxInt64-extra {
					return 0, fmt.Errorf("%w: scratch overflow", ErrInvalidVolume)
				}
				scratch += extra
			}
		}
		if e != nil {
			return 0, e
		}
		metadata, e := regionalCheckedProduct(workers, 256)
		if e != nil {
			return 0, e
		}
		total := int64(0)
		for _, part := range []int64{ni, no, scratch, 4096, metadata} {
			if total > math.MaxInt64-part {
				return 0, fmt.Errorf("%w: working overflow", ErrInvalidVolume)
			}
			total += part
		}
		return total, nil
	}
	minimum, err := cost([3]int{1, 1, 1})
	if err != nil {
		return res, err
	}
	if minimum > budget {
		return res, &RegionalBudgetError{minimum, budget}
	}
	tile := opts.TileSize
	for a, n := range requested.Size {
		if tile[a] < 0 {
			return res, fmt.Errorf("%w: tile size", ErrInvalidVolume)
		}
		if tile[a] == 0 {
			tile[a] = min(n, [3]int{64, 64, 16}[a])
		}
		tile[a] = min(tile[a], n)
	}
	for {
		n, e := cost(tile)
		if e != nil {
			return res, e
		}
		if n <= budget {
			res.WorkingBytes = n
			break
		}
		axis := 0
		for a := 1; a < 3; a++ {
			if tile[a] > tile[axis] {
				axis = a
			}
		}
		tile[axis] = (tile[axis] + 1) / 2
	}
	res.TileSize = tile
	// Account the upper read bound without enumerating/allocating tile metadata.
	tiles := int64(1)
	var win [3]int64
	for a, n := range requested.Size {
		c := int64((n-1)/tile[a] + 1)
		if tiles > math.MaxInt64/c {
			return res, fmt.Errorf("%w: tile count", ErrInvalidVolume)
		}
		tiles *= c
		win[a] = int64(regionalExtent(dims[a], tile[a], k.radius[a]))
	}
	bytes, e := regionalCheckedProduct(tiles, win[0], win[1], win[2], 4)
	if e != nil {
		return res, e
	}
	if k.kind == 1 {
		full, e := regionalCheckedProduct(int64(dims[0]), int64(dims[1]), int64(dims[2]), 4)
		if e != nil || bytes > math.MaxInt64-full {
			return res, fmt.Errorf("%w: classification read overflow", ErrInvalidVolume)
		}
		bytes += full
	}
	res.PlannedReadBytes = bytes
	if opts.MaxReadBytes > 0 && bytes > opts.MaxReadBytes {
		return res, fmt.Errorf("%w: read bound needs %d bytes", ErrRegionalBudget, bytes)
	}
	return res, nil
}
func (v *Volume) ReadRegion(ctx context.Context, r Region, dst []float32) error {
	return v.regionCopy(ctx, r, dst, false)
}
func (v *Volume) WriteRegion(ctx context.Context, r Region, src []float32) error {
	return v.regionCopy(ctx, r, src, true)
}
func (v *Volume) regionCopy(ctx context.Context, r Region, buf []float32, write bool) error {
	n, ok := voxelCount(r.Size)
	if v == nil || !ok || !regionalValid(r, v.Dims) || len(buf) != n {
		return fmt.Errorf("%w: region buffer", ErrInvalidVolume)
	}
	total, ok := voxelCount(v.Dims)
	if !ok || len(v.Data) != total {
		return fmt.Errorf("%w: volume buffer", ErrInvalidVolume)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for z := 0; z < r.Size[2]; z++ {
		for y := 0; y < r.Size[1]; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			i := v.Index(r.Origin[0], r.Origin[1]+y, r.Origin[2]+z)
			j := (z*r.Size[1] + y) * r.Size[0]
			if write {
				copy(v.Data[i:i+r.Size[0]], buf[j:j+r.Size[0]])
			} else {
				copy(buf[j:j+r.Size[0]], v.Data[i:i+r.Size[0]])
			}
		}
	}
	return nil
}

// FilterRegions filters only requested output coordinates. Complete tiles are
// committed serially after successful read/filter and a cancellation check.
// Previous committed tiles remain on failure; writer transactionality is external.
// Median makes a bounded classification pass over the full source to preserve
// the whole-volume histogram/selection choice (including signed-zero bits).
func FilterRegions(ctx context.Context, f SlabFilter, src RegionReader, dims [3]int, spacing [3]float64, requested Region, dst RegionWriter, opts RegionalOptions) (RegionalResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if src == nil || dst == nil {
		return RegionalResult{}, fmt.Errorf("%w: nil regional IO", ErrInvalidVolume)
	}
	f = regionalFreezeWorkers(f)
	res, err := PlanRegions(f, dims, spacing, requested, opts)
	if err != nil {
		return res, err
	}
	if err = ctx.Err(); err != nil {
		return res, err
	}
	k, _ := regionalParams(f, dims, spacing)
	var maxWin [3]int
	for a := range maxWin {
		maxWin[a] = regionalExtent(dims[a], res.TileSize[a], k.radius[a])
	}
	ni, _ := voxelCount(maxWin)
	no, _ := voxelCount(res.TileSize)
	input := make([]float32, ni)
	output := make([]float32, no)
	read := func(r Region, buf []float32) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := src.ReadRegion(ctx, r, buf); e != nil {
			return e
		}
		res.ReadBytes += int64(len(buf)) * 4
		for _, v := range buf {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return fmt.Errorf("%w: nonfinite regional sample", ErrInvalidVolume)
			}
		}
		return ctx.Err()
	}
	integral := true
	lo, hi := float32(math.Inf(1)), float32(math.Inf(-1))
	if k.kind == 1 { // One complete pass, not one pass per tile. Uses the existing input array.
		for z := 0; z < dims[2]; {
			dz := min(maxWin[2], dims[2]-z)
			for y := 0; y < dims[1]; {
				dy := min(maxWin[1], dims[1]-y)
				for x := 0; x < dims[0]; {
					dx := min(maxWin[0], dims[0]-x)
					r := Region{[3]int{x, y, z}, [3]int{dx, dy, dz}}
					n, _ := voxelCount(r.Size)
					if e := read(r, input[:n]); e != nil {
						return res, e
					}
					for _, v := range input[:n] {
						integral = integral && v == float32(math.Trunc(float64(v))) && v < 1<<24 && v > -(1<<24)
						lo, hi = min(lo, v), max(hi, v)
					}
					x += dx
				}
				y += dy
			}
			z += dz
		}
		integral = integral && float64(hi)-float64(lo)+1 <= medianHistogramBins
	}
	var bilateral *bilateralKernel
	if k.kind == 2 {
		bilateral, err = newBilateralKernel(dims, spacing, f.(BilateralOptions))
		if err != nil {
			return res, err
		}
	}
	linePool := scratchPool[regionalLineScratch]{}
	if k.kind == 1 {
		for i := 0; i < workerCount(k.workers, res.TileSize[1]*res.TileSize[2]); i++ {
			linePool.items = append(linePool.items, &regionalLineScratch{box: make([]float32, k.blockLen)})
		}
	}
	for z := requested.Origin[2]; z < requested.Origin[2]+requested.Size[2]; {
		tz := min(res.TileSize[2], requested.Origin[2]+requested.Size[2]-z)
		for y := requested.Origin[1]; y < requested.Origin[1]+requested.Size[1]; {
			ty := min(res.TileSize[1], requested.Origin[1]+requested.Size[1]-y)
			for x := requested.Origin[0]; x < requested.Origin[0]+requested.Size[0]; {
				tx := min(res.TileSize[0], requested.Origin[0]+requested.Size[0]-x)
				tile := Region{[3]int{x, y, z}, [3]int{tx, ty, tz}}
				w := regionalWindow(tile, dims, k.radius)
				n, _ := voxelCount(w.Size)
				if e := read(w, input[:n]); e != nil {
					return res, e
				}
				view := slabView{data: input[:n], dims: dims, regionOrigin: w.Origin, regionSize: w.Size}
				m, _ := voxelCount(tile.Size)
				part := output[:m]
				if k.kind == 3 {
					err = regionalNLM(ctx, &k, tile, view, part)
				} else {
					err = runWithScratch(ctx, ty*tz, workerCount(k.workers, ty*tz), &linePool, func(s *regionalLineScratch, line int) {
						gy, gz := y+line%ty, z+line/ty
						for ix := 0; ix < tx; ix++ {
							gx := x + ix
							if k.kind == 1 {
								j := 0
								for dx := -k.radius[0]; dx <= k.radius[0]; dx++ {
									for dz := -k.radius[2]; dz <= k.radius[2]; dz++ {
										for dy := -k.radius[1]; dy <= k.radius[1]; dy++ {
											s.box[j] = view.data[view.row(clampAxis(gy+dy, dims[1]), clampAxis(gz+dz, dims[2]))+clampAxis(gx+dx, dims[0])]
											j++
										}
									}
								}
								v := selectRank(s.box, k.blockLen/2)
								if integral {
									v = lo + float32(int(v-lo))
								}
								part[line*tx+ix] = v
							} else {
								centre := float64(view.data[view.row(gy, gz)+gx])
								sum, weights := 0.0, 0.0
								for _, t := range bilateral.taps {
									sx, sy, sz := gx+t.dx, gy+t.dy, gz+t.dz
									if sx < 0 || sy < 0 || sz < 0 || sx >= dims[0] || sy >= dims[1] || sz >= dims[2] {
										continue
									}
									v := float64(view.data[view.row(sy, sz)+sx])
									d := v - centre
									weight := t.w * math.Exp(d*d*bilateral.rangeScale)
									sum += weight * v
									weights += weight
								}
								part[line*tx+ix] = float32(sum / weights)
							}
						}
					})
				}
				if err != nil {
					return res, err
				}
				if err = ctx.Err(); err != nil {
					return res, err
				}
				if err = dst.WriteRegion(ctx, tile, part); err != nil {
					return res, err
				}
				res.Tiles++
				x += tx
			}
			y += ty
		}
		z += tz
	}
	return res, ctx.Err()
}

type regionalLineScratch struct{ box []float32 }

func regionalExtent(n, size, halo int) int {
	if halo >= (n-size+1)/2 {
		return n
	}
	return size + 2*halo
}
func regionalCentres(dst []int, n, r, step, start, size int) []int {
	lo := max(0, start-r)
	end := start + size - 1
	hi := end + min(r, n-1-end)
	first := lo
	if rem := lo % step; rem != 0 {
		inc := step - rem
		if inc > n-1-lo {
			first = n - 1
		} else {
			first = lo + inc
		}
	}
	out := dst[:0]
	capacity := regionalExtent(n, size, r)
	if cap(out) < capacity {
		out = make([]int, 0, capacity)
	}
	for c := first; c < n-1 && c <= hi; {
		out = append(out, c)
		if step > n-1-c {
			break
		}
		c += step
	}
	if n-1 >= lo && n-1 <= hi {
		out = append(out, n-1)
	}
	return out
}

func regionalNLM(ctx context.Context, p *regionalKernel, tile Region, in slabView, out []float32) error {
	k := p.runtime
	if k == nil {
		k = &nlmKernel{dims: p.nlm.dims, patch: p.nlm.patch, search: p.nlm.search, step: p.nlm.step, blockLen: p.nlm.blockLen, invH2: p.nlm.invH2, cutoff: p.nlm.cutoff, workers: p.workers}
		p.runtime = k
	}
	for a := range k.centres {
		k.centres[a] = regionalCentres(k.centres[a], k.dims[a], k.patch[a], k.step[a], tile.Origin[a], tile.Size[a])
	}
	nbx, nby, nbz := len(k.centres[0]), len(k.centres[1]), len(k.centres[2])
	planeLen := nbx * nby * k.blockLen
	if len(k.planes) == 0 {
		k.planes = make([][]float32, 1)
	}
	if cap(k.planes[0]) < planeLen*nbz {
		k.planes[0] = make([]float32, planeLen*nbz)
	}
	planes := k.planes[0][:planeLen*nbz]
	k.rowOffsets = k.rowOffsets[:0]
	for dz := -k.patch[2]; dz <= k.patch[2]; dz++ {
		for dy := -k.patch[1]; dy <= k.patch[1]; dy++ {
			k.rowOffsets = append(k.rowOffsets, (dz*in.regionSize[1]+dy)*in.regionSize[0])
		}
	}
	for iz, cz := range k.centres[2] {
		plane := planes[iz*planeLen : (iz+1)*planeLen]
		if err := runWithScratch(ctx, nby, workerCount(k.workers, nby), &k.scratch, func(s *nlmScratch, iy int) {
			k.blockRow(in, s, plane[iy*nbx*k.blockLen:(iy+1)*nbx*k.blockLen], k.centres[1][iy], cz)
		}); err != nil {
			return err
		}
	}
	bx, by := 2*k.patch[0]+1, 2*k.patch[1]+1
	err := parallelLines(ctx, tile.Size[1]*tile.Size[2], workerCount(k.workers, tile.Size[1]*tile.Size[2]), func(line int) {
		y := tile.Origin[1] + line%tile.Size[1]
		z := tile.Origin[2] + line/tile.Size[1]
		for ix := 0; ix < tile.Size[0]; ix++ {
			x := tile.Origin[0] + ix
			sum := 0.0
			count := 0
			for iz, cz := range k.centres[2] {
				if regionalAbs(z-cz) > k.patch[2] {
					continue
				}
				for iy, cy := range k.centres[1] {
					if regionalAbs(y-cy) > k.patch[1] {
						continue
					}
					for jx, cx := range k.centres[0] {
						if regionalAbs(x-cx) > k.patch[0] {
							continue
						}
						offset := ((iz*nby+iy)*nbx+jx)*k.blockLen + ((z-cz+k.patch[2])*by+y-cy+k.patch[1])*bx + x - cx + k.patch[0]
						sum += float64(planes[offset])
						count++
					}
				}
			}
			out[line*tile.Size[0]+ix] = float32(sum / float64(count))
		}
	})
	return err
}

func regionalAbs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func regionalFreezeWorkers(f SlabFilter) SlabFilter {
	switch o := f.(type) {
	case *MedianOptions:
		if o != nil {
			return regionalFreezeWorkers(*o)
		}
		return f
	case *BilateralOptions:
		if o != nil {
			return regionalFreezeWorkers(*o)
		}
		return f
	case *NonLocalMeansOptions:
		if o != nil {
			return regionalFreezeWorkers(*o)
		}
		return f
	}
	switch o := f.(type) {
	case MedianOptions:
		o.Workers = workerBound(o.Workers)
		return o
	case BilateralOptions:
		o.Workers = workerBound(o.Workers)
		return o
	case NonLocalMeansOptions:
		o.Workers = workerBound(o.Workers)
		return o
	}
	return f
}

type regionalNLMParams struct {
	dims, patch, search, step [3]int
	blockLen, workers         int
	invH2, cutoff             float64
}

func regionalRadiiValid(r [3]int) bool {
	for _, v := range r {
		if v < 0 || v > (math.MaxInt-1)/2 {
			return false
		}
	}
	return true
}
