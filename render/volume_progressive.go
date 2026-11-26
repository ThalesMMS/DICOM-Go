package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// Progressive volume decoding (#1230). PrepareSnapshotProgressiveContext
// decodes the same canonical generation as PrepareSnapshotContext, in an order
// that lets a viewer show the volume before every slice has arrived: the even
// slices first, from which a 2× preview is built (each preview voxel averages
// a 2×2 block of one even slice, the in-plane half of BinShrinkVolumeContext's
// 2× level, at twice the slice spacing), then the odd slices in increasing z,
// reporting each z-band of slices that becomes complete at full resolution so
// a GPU backend can upload it while the rest decodes. The generation is
// installed only when every slice is decoded; its payload and descriptor are
// the ones PrepareSnapshotContext installs, so frames rendered from it are
// unchanged. PrepareSnapshotProgressiveWithOptionsContext can omit the derived
// preview while preserving the original full-resolution bands and generation.
// Written from the documented behaviour of progressive volume
// loaders (Isis DICOM Viewer's ProgressiveVolumeDataset and
// ChunkedVolumeUploader); no code is copied.

// VolumeDecodePhase is the kind of a progressive decode event.
type VolumeDecodePhase uint8

const (
	// VolumeDecodePreview: the even slices are decoded and Preview holds the
	// 2× preview volume.
	VolumeDecodePreview VolumeDecodePhase = iota + 1
	// VolumeDecodeBand: slices [BandStart, BandEnd) are decoded at full
	// resolution; Band reads them during the callback.
	VolumeDecodeBand
	// VolumeDecodeComplete: every slice is decoded and the canonical
	// generation is installed; measurements may use the volume.
	VolumeDecodeComplete
	// VolumeDecodeEvenSlices reports unchanged even slices for device preview
	// preparation, only when ReportEvenSlices is explicitly selected.
	VolumeDecodeEvenSlices
)

// progressiveBandSlices is the smallest z-band reported before the end.
const progressiveBandSlices = 8

// VolumeDecodeProgress is one progressive decode event. Events arrive on the
// decoding goroutine, in order: at most one preview, bands covering every
// slice once in increasing z, then exactly one completion on success.
type VolumeDecodeProgress struct {
	Phase         VolumeDecodePhase
	DecodedSlices int
	TotalSlices   int
	// Descriptor is the descriptor the generation carries; VolumeGeneration
	// is zero until the completion event.
	Descriptor VolumeDescriptor
	// Preview (VolumeDecodePreview) is a new volume owned by the observer,
	// which must Close it.
	Preview *Volume
	// BandStart and BandEnd (VolumeDecodeBand) are the z-range of the band.
	BandStart, BandEnd int
	// Band reads the band's samples during the callback. Call Retain inside
	// the callback before handing the reader to another goroutine.
	Band       VolumeBandReader
	EvenSlices VolumeBandReader
}

// VolumeBandReader reads one decoded z-band of a progressive decode.
type VolumeBandReader struct {
	volume      *Volume
	cols, rows  int
	start, end  int
	step        int
	descriptor  VolumeDescriptor
	float32Data []float32
	packed      *packed16VolumePayload
}

// RetainedVolumeBandReader is an immutable view of one completed decode band.
// It captures geometry and backing-slice headers in value, without copying or
// resampling samples. The decoder writes each slice once, and publishes a band
// only after all of its slices have finished. Cancellation/retry uses a new
// allocation; closing the source volume never overwrites the retained backing.
// A retained reader remains valid after callback return, cancellation, retry or
// Volume.Close. Dropping every reader releases its reference for garbage
// collection. Retaining one band keeps the entire decode allocation alive;
// readers from the same decode share that allocation, rather than duplicating it.
type RetainedVolumeBandReader struct {
	cols, rows, start, end int
	step                   int
	descriptor             VolumeDescriptor
	float32Data            []float32
	signed                 []int16
	unsigned               []uint16
}

// Retain captures a read-only band before its decoding callback returns.
func (b VolumeBandReader) Retain() (RetainedVolumeBandReader, error) {
	cols, rows := b.dimensions()
	count, valid := checkedVoxelCount(cols, rows, b.end)
	if !valid || b.start < 0 || b.start >= b.end {
		return RetainedVolumeBandReader{}, fmt.Errorf("%w: invalid decode band", ErrInvalidVolumeSnapshot)
	}
	if b.step < 0 {
		return RetainedVolumeBandReader{}, fmt.Errorf("%w: invalid decode band stride", ErrInvalidVolumeSnapshot)
	}
	r := RetainedVolumeBandReader{cols: cols, rows: rows, start: b.start, end: b.end, step: b.step, descriptor: b.descriptor}
	if b.packed != nil {
		if b.packed.signed != nil && len(b.packed.signed) >= count && b.packed.unsigned == nil {
			r.signed = b.packed.signed
		} else if b.packed.unsigned != nil && len(b.packed.unsigned) >= count && b.packed.signed == nil {
			r.unsigned = b.packed.unsigned
		} else {
			return RetainedVolumeBandReader{}, fmt.Errorf("%w: incomplete stored decode band", ErrInvalidVolumeSnapshot)
		}
	} else if len(b.float32Data) >= count {
		r.float32Data = b.float32Data
	} else {
		return RetainedVolumeBandReader{}, fmt.Errorf("%w: incomplete modality decode band", ErrInvalidVolumeSnapshot)
	}
	return r, nil
}

func (b VolumeBandReader) dimensions() (cols, rows int) {
	if b.cols > 0 && b.rows > 0 {
		return b.cols, b.rows
	}
	if b.volume != nil {
		return b.volume.Cols, b.volume.Rows
	}
	return 0, 0
}

func (r RetainedVolumeBandReader) reader() VolumeBandReader {
	b := VolumeBandReader{cols: r.cols, rows: r.rows, start: r.start, end: r.end, step: r.step, descriptor: r.descriptor, float32Data: r.float32Data}
	if r.signed != nil || r.unsigned != nil {
		b.packed = &packed16VolumePayload{signed: r.signed, unsigned: r.unsigned}
	}
	return b
}

// WriteModalityF32To streams the retained band's original modality samples.
func (r RetainedVolumeBandReader) WriteModalityF32To(dst io.Writer) error {
	return r.reader().WriteModalityF32To(dst)
}

// WriteStored16To streams the retained band's original 16-bit stored samples.
func (r RetainedVolumeBandReader) WriteStored16To(dst io.Writer) error {
	return r.reader().WriteStored16To(dst)
}

// WriteModalityF32To streams the band as tightly packed little-endian
// float32 modality rows, as VolumeSnapshot.WriteModalityF32Context does for
// the whole volume.
func (b VolumeBandReader) WriteModalityF32To(dst io.Writer) error {
	cols, rows := b.dimensions()
	if cols <= 0 || rows <= 0 || dst == nil {
		return fmt.Errorf("%w: no decode band", ErrInvalidVolumeSnapshot)
	}
	if b.packed == nil && hostLittleEndian() {
		if done, err := b.writeSlicesDirect(dst, 4); done {
			return err
		}
	}
	row := make([]byte, 4*cols)
	for z := b.start; z < b.end; z += max(1, b.step) {
		for y := 0; y < rows; y++ {
			base := (z*rows + y) * cols
			for x := 0; x < cols; x++ {
				var value float32
				if b.packed != nil {
					value = float32(b.packed.modality(base+x, b.descriptor))
				} else {
					value = b.float32Data[base+x]
				}
				binary.LittleEndian.PutUint32(row[4*x:], math.Float32bits(value))
			}
			if err := writeExact(dst, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// WriteStored16To streams a 16-bit stored band as tightly packed
// little-endian rows, as VolumeSnapshot.WriteStored16Context does.
func (b VolumeBandReader) WriteStored16To(dst io.Writer) error {
	cols, rows := b.dimensions()
	if cols <= 0 || rows <= 0 || dst == nil || b.packed == nil {
		return fmt.Errorf("%w: band holds no 16-bit stored samples", ErrInvalidVolumeSnapshot)
	}
	if hostLittleEndian() {
		if done, err := b.writeSlicesDirect(dst, 2); done {
			return err
		}
	}
	row := make([]byte, 2*cols)
	for z := b.start; z < b.end; z += max(1, b.step) {
		for y := 0; y < rows; y++ {
			base := (z*rows + y) * cols
			for x := 0; x < cols; x++ {
				var value uint16
				if b.packed.signed != nil {
					value = uint16(b.packed.signed[base+x])
				} else {
					value = b.packed.unsigned[base+x]
				}
				binary.LittleEndian.PutUint16(row[2*x:], value)
			}
			if err := writeExact(dst, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSlicesDirect writes each slice of the band as one little-endian
// write of its native samples, which are already in the stream's layout on
// a little-endian host: float32 modality samples (sampleBytes 4) or 16-bit
// stored samples (sampleBytes 2). A slice is contiguous, so a band that
// skips slices (EvenSlices) still writes whole slices, without the per-sample
// conversion of the row loop (#1626). done is false, with nothing written,
// when the band's backing does not hold every slice; the row loop then
// reports it as before. dst consumes each slice before returning and must
// not modify it.
func (b VolumeBandReader) writeSlicesDirect(dst io.Writer, sampleBytes int) (done bool, err error) {
	cols, rows := b.dimensions()
	sliceSamples, ok := checkedVoxelCount(cols, rows, 1)
	if !ok || b.start < 0 || b.end <= b.start {
		return false, nil
	}
	var base unsafe.Pointer
	var samples int
	switch {
	case sampleBytes == 4 && b.float32Data != nil:
		base, samples = unsafe.Pointer(unsafe.SliceData(b.float32Data)), len(b.float32Data)
	case sampleBytes == 2 && b.packed != nil && b.packed.signed != nil:
		base, samples = unsafe.Pointer(unsafe.SliceData(b.packed.signed)), len(b.packed.signed)
	case sampleBytes == 2 && b.packed != nil && b.packed.unsigned != nil:
		base, samples = unsafe.Pointer(unsafe.SliceData(b.packed.unsigned)), len(b.packed.unsigned)
	default:
		return false, nil
	}
	step := max(1, b.step)
	last := b.start + (b.end-1-b.start)/step*step
	if needed, ok := checkedVoxelCount(cols, rows, last+1); !ok || needed > samples {
		return false, nil
	}
	sliceBytes := sliceSamples * sampleBytes
	for z := b.start; z < b.end; z += step {
		slice := unsafe.Slice((*byte)(unsafe.Add(base, z*sliceBytes)), sliceBytes)
		if err := writeExact(dst, slice); err != nil {
			return true, err
		}
	}
	return true, nil
}

// hostLittleEndian reports whether native 16- and 32-bit samples are stored
// little-endian.
func hostLittleEndian() bool {
	var probe uint16 = 1
	return *(*byte)(unsafe.Pointer(&probe)) == 1
}

func (p *packed16VolumePayload) modality(index int, descriptor VolumeDescriptor) float64 {
	if p.signed != nil {
		return float64(p.signed[index])*descriptor.RescaleSlope + descriptor.RescaleIntercept
	}
	return float64(p.unsigned[index])*descriptor.RescaleSlope + descriptor.RescaleIntercept
}

// PrepareSnapshotProgressiveContext is PrepareSnapshotContext reporting
// progressive decode events to observe (see VolumeDecodeProgress). A volume
// whose snapshot is already installed, or is being built by another caller,
// reports only the completion; one that needs resampling to a regular grid
// decodes in one pass and reports only the completion as well. observe may be
// nil.
func (v *Volume) PrepareSnapshotProgressiveContext(ctx context.Context, observe func(VolumeDecodeProgress)) (VolumePreparationStats, error) {
	return v.PrepareSnapshotProgressiveWithOptionsContext(ctx, VolumeDecodeOptions{}, observe)
}

// VolumeDecodeOptions controls optional derived data during progressive loading.
// Its zero value preserves the preview of PrepareSnapshotProgressiveContext.
type VolumeDecodeOptions struct {
	// SkipPreview prevents construction of the averaged, reduced-resolution
	// volume. Full-resolution decoded bands and completion are still reported.
	// It does not disable regular-grid preparation for irregular library volumes.
	SkipPreview bool
	// ReportEvenSlices publishes unchanged, retained-readable even z samples
	// before odd-slice decoding. It performs no spatial filtering or averaging.
	ReportEvenSlices bool
}

// PrepareSnapshotProgressiveWithOptionsContext reports progressive loading with
// optional derived-volume preview construction. SkipPreview supports callers
// that must upload stored samples without computing a CPU-derived 3D preview.
// It preserves the callback lifetime of Band and the full-range band sequence;
// callers must consume each Band or call Retain before returning from observe. As with
// the default API, an existing snapshot or irregular-grid preparation reports
// only completion. This option does not prevent regular-grid resampling.
func (v *Volume) PrepareSnapshotProgressiveWithOptionsContext(ctx context.Context, options VolumeDecodeOptions, observe func(VolumeDecodeProgress)) (VolumePreparationStats, error) {
	if v == nil {
		return VolumePreparationStats{}, fmt.Errorf("render: nil volume")
	}
	if observe == nil {
		return v.PrepareSnapshotContext(ctx)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if v.geometry.RequiresResampling {
		stats, err := v.PrepareSnapshotContext(ctx)
		if err == nil {
			err = v.reportComplete(observe)
		}
		return stats, err
	}
	stats, err := v.prepareDirectSnapshotObserved(ctx, observe, options)
	if err == nil {
		err = v.reportComplete(observe)
	}
	return stats, err
}

// SnapshotReady reports whether the canonical generation is installed: every
// slice is decoded, so measurements and full-resolution rendering can use the
// volume without waiting.
func (v *Volume) SnapshotReady() bool {
	if v == nil || v.closed.Load() {
		return false
	}
	target := v
	if v.geometry.RequiresResampling {
		if !v.regularizedReady.Load() {
			return false
		}
		target = v.regularized
	}
	target.snapshotMu.Lock()
	defer target.snapshotMu.Unlock()
	return target.generation != 0 && target.snapshotErr == nil
}

func (v *Volume) reportComplete(observe func(VolumeDecodeProgress)) error {
	lease, err := v.AcquireSnapshot()
	if err != nil {
		return err
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		return err
	}
	descriptor, err := snapshot.Descriptor()
	if err != nil {
		return err
	}
	depth := int(descriptor.Dimensions[2])
	observe(VolumeDecodeProgress{
		Phase: VolumeDecodeComplete, DecodedSlices: depth, TotalSlices: depth, Descriptor: descriptor,
	})
	return nil
}

// progressiveDecoder decodes v's slices even first and reports the events.
type progressiveDecoder struct {
	skipPreview      bool
	reportEvenSlices bool
	volume           *Volume
	observe          func(VolumeDecodeProgress)
	descriptor       VolumeDescriptor
	float32          []float32
	packed           *packed16VolumePayload
	ready            []bool
	decoded          int
	prefix           int // slices [0, prefix) are decoded
	reported         int // slices [0, reported) were reported in bands
}

// progressiveSliceOrder is the even slices, then the odd ones.
func progressiveSliceOrder(depth int) []int {
	order := make([]int, 0, depth)
	for z := 0; z < depth; z += 2 {
		order = append(order, z)
	}
	for z := 1; z < depth; z += 2 {
		order = append(order, z)
	}
	return order
}

// decodeProgressiveSlices decodes the slices in progressiveSliceOrder with
// the shared worker budget and reports each one to decoded in that order, on
// the calling goroutine. Batches of a few slices per worker keep the even
// slices ahead of the odd ones and the bands flowing, while slow per-slice
// codecs (an external JPEG 2000 decoder costs tens of milliseconds a slice)
// no longer run one slice at a time. Every worker of a batch is joined before
// its slices are reported, so observers only read completed planes.
func decodeProgressiveSlices(ctx context.Context, depth, voxels int, decode, decoded func(int) error) error {
	order := progressiveSliceOrder(depth)
	evens := (depth + 1) / 2
	batch := max(1, progressiveDecodeBatchPerWorker*parallel.DefaultWorkers())
	for start := 0; start < len(order); {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A batch never mixes even and odd slices, so the even slices are
		// reported as soon as the last of them is decoded.
		end := min(len(order), start+batch)
		if start < evens {
			end = min(end, evens)
		}
		part := order[start:end]
		start = end
		if err := decodeVolumeSlices(ctx, len(part), voxels, func(item int) error {
			return decode(part[item])
		}); err != nil {
			return err
		}
		for _, index := range part {
			// An observer may cancel; report nothing after it did.
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := decoded(index); err != nil {
				return err
			}
		}
	}
	return nil
}

// progressiveDecodeBatchPerWorker is the number of slices per worker decoded
// between two rounds of progress reports.
const progressiveDecodeBatchPerWorker = 2

// decoded records slice z and reports the preview after the even slices and
// the bands as they complete.
func (d *progressiveDecoder) sliceDecoded(z int) error {
	if d == nil || d.observe == nil {
		return nil
	}
	d.ready[z] = true
	d.decoded++
	depth := d.volume.Depth
	if d.reportEvenSlices && d.decoded == (depth+1)/2 {
		d.observe(VolumeDecodeProgress{
			Phase: VolumeDecodeEvenSlices, DecodedSlices: d.decoded, TotalSlices: depth, Descriptor: d.descriptor,
			EvenSlices: VolumeBandReader{volume: d.volume, cols: d.volume.Cols, rows: d.volume.Rows, start: 0, end: depth, step: 2, descriptor: d.descriptor, float32Data: d.float32, packed: d.packed},
		})
	}
	if !d.skipPreview && d.decoded == (depth+1)/2 {
		preview, err := d.preview()
		if err != nil {
			return err
		}
		d.observe(VolumeDecodeProgress{
			Phase: VolumeDecodePreview, DecodedSlices: d.decoded, TotalSlices: depth,
			Descriptor: d.descriptor, Preview: preview,
		})
	}
	for d.prefix < depth && d.ready[d.prefix] {
		d.prefix++
	}
	if d.prefix-d.reported >= progressiveBandSlices || (d.prefix == depth && d.reported < depth) {
		d.observe(VolumeDecodeProgress{
			Phase: VolumeDecodeBand, DecodedSlices: d.decoded, TotalSlices: depth, Descriptor: d.descriptor,
			BandStart: d.reported, BandEnd: d.prefix,
			Band: VolumeBandReader{
				volume: d.volume, cols: d.volume.Cols, rows: d.volume.Rows,
				start: d.reported, end: d.prefix, descriptor: d.descriptor,
				float32Data: d.float32, packed: d.packed,
			},
		})
		d.reported = d.prefix
	}
	return nil
}

// preview builds the 2× preview from the even slices.
func (d *progressiveDecoder) preview() (*Volume, error) {
	source := d.volume
	fx, fy := min(2, source.Cols), min(2, source.Rows)
	cols, rows := max(1, source.Cols/fx), max(1, source.Rows/fy)
	depth := (source.Depth + 1) / 2
	count, ok := checkedVoxelCount(cols, rows, depth)
	if !ok {
		return nil, fmt.Errorf("render: preview dimensions overflow")
	}
	values := make([]float32, count)
	block := float64(fx * fy)
	for z := 0; z < depth; z++ {
		sourceSlice := 2 * z * source.Rows * source.Cols
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				sum := 0.0
				for dy := 0; dy < fy; dy++ {
					for dx := 0; dx < fx; dx++ {
						index := sourceSlice + (y*fy+dy)*source.Cols + x*fx + dx
						if d.packed != nil {
							sum += d.packed.modality(index, d.descriptor)
						} else {
							sum += float64(d.float32[index])
						}
					}
				}
				values[(z*rows+y)*cols+x] = float32(sum / block)
			}
		}
	}
	sliceSpacing := source.SliceSpacing * 2
	if depth == 1 {
		sliceSpacing = source.SliceSpacing
	}
	origin := source.Origin.
		Add(source.AxisX.Scale(float64(fx-1) / 2 * source.ColSpacing)).
		Add(source.AxisY.Scale(float64(fy-1) / 2 * source.RowSpacing))
	return newDerivedFloat32Volume(source, cols, rows, depth, origin,
		source.ColSpacing*float64(fx), source.RowSpacing*float64(fy), sliceSpacing, values)
}

// newDerivedFloat32Volume is a regular volume on source's axes holding
// float32 modality values in its own store, built as BinShrinkVolumeContext
// builds its levels.
func newDerivedFloat32Volume(source *Volume, cols, rows, depth int, origin Vec3, colSpacing, rowSpacing, sliceSpacing float64, values []float32) (*Volume, error) {
	photometric := normalizedPhotometric(source.Photometric())
	if photometric == "" {
		photometric = "MONOCHROME2"
	}
	defaultWindow := WindowLevel{Center: defaultWindowCenter, Width: defaultWindowWidth}
	if len(source.slices) > 0 && source.slices[0] != nil && source.slices[0].DefaultWindow.Width > 0 {
		defaultWindow = source.slices[0].DefaultWindow
	}
	frames := make([]*Frame, depth)
	positions := make([]float64, depth)
	origins := make([]Vec3, depth)
	geometries := make([]SliceGeometry, depth)
	for z := 0; z < depth; z++ {
		sliceOrigin := origin.Add(source.Normal.Scale(float64(z) * sliceSpacing))
		frames[z] = &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(rows), Columns: uint16(cols), SamplesPerPixel: 1,
				BitsAllocated: 32, BitsStored: 32, HighBit: 31,
				PhotometricInterpretation: photometric,
			},
			DefaultWindow: defaultWindow,
		}
		positions[z] = float64(z) * sliceSpacing
		origins[z] = sliceOrigin
		geometries[z] = SliceGeometry{
			Origin: sliceOrigin, RowDir: source.AxisX, ColDir: source.AxisY, Normal: source.Normal,
			RowSpacing: rowSpacing, ColSpacing: colSpacing, Rows: rows, Columns: cols,
		}
	}
	target := &Volume{
		Cols: cols, Rows: rows, Depth: depth,
		Origin: origin, AxisX: source.AxisX, AxisY: source.AxisY, Normal: source.Normal,
		ColSpacing: colSpacing, RowSpacing: rowSpacing, SliceSpacing: sliceSpacing,
		positions: positions, sliceOrigins: origins, regular: true,
		geometry: BuildVolumeGeometry(geometries, DefaultGeometryTolerances()),
		slices:   frames, store: NewVolumeStore(),
	}
	descriptor, err := target.snapshotDescriptor(VolumeDerivationNormalized, 0, false)
	if err != nil {
		_ = target.Close()
		return nil, err
	}
	generation, err := target.store.replaceFloat32Owned(descriptor, values)
	if err != nil {
		_ = target.Close()
		return nil, err
	}
	target.snapshotMu.Lock()
	target.generation = generation
	target.snapshotStats = VolumePreparationStats{VoxelBytes: uint64(len(values)) * 4, Frames: depth}
	target.snapshotMu.Unlock()
	return target, nil
}

// decodeFloat32Progressive decodes a float32 volume even slices first.
func (v *Volume) decodeFloat32Progressive(ctx context.Context, observe func(VolumeDecodeProgress), options VolumeDecodeOptions) ([]float32, error) {
	// Scalar previews/bands have a scalar-only contract. Color preparation
	// publishes the complete component generation without a scalar preview.
	if v.Components() > 1 {
		return decodeVolumeFloat32Context(ctx, v)
	}
	count, ok := checkedVoxelCount(v.Cols, v.Rows, v.Depth)
	if !ok {
		return nil, fmt.Errorf("%w: volume dimensions overflow", ErrInvalidVolumeSnapshot)
	}
	descriptor, err := v.snapshotDescriptor(VolumeDerivationNormalized, 0, false)
	if err != nil {
		return nil, err
	}
	decoder := &progressiveDecoder{
		volume: v, observe: observe, descriptor: descriptor, skipPreview: options.SkipPreview, reportEvenSlices: options.ReportEvenSlices,
		float32: make([]float32, count), ready: make([]bool, v.Depth),
	}
	if err := decodeProgressiveSlices(ctx, v.Depth, count, func(index int) error {
		return decodeFloat32SliceInto(ctx, v, decoder.float32, index)
	}, decoder.sliceDecoded); err != nil {
		return nil, err
	}
	return decoder.float32, nil
}

// decodePacked16Progressive decodes a 16-bit stored volume even slices
// first; its descriptor carries the packed format and rescale.
func (v *Volume) decodePacked16Progressive(ctx context.Context, plan packed16Plan, observe func(VolumeDecodeProgress), options VolumeDecodeOptions) (*packed16VolumePayload, error) {
	count, ok := checkedVoxelCount(v.Cols, v.Rows, v.Depth)
	if !ok {
		return nil, fmt.Errorf("%w: volume dimensions overflow", ErrInvalidVolumeSnapshot)
	}
	descriptor, err := v.snapshotDescriptor(VolumeDerivationNormalized, 0, false)
	if err != nil {
		return nil, err
	}
	payload := &packed16VolumePayload{}
	descriptor.ScalarFormat = VolumeScalarU16StoredLE
	if plan.signed {
		payload.signed = make([]int16, count)
		descriptor.ScalarFormat = VolumeScalarI16StoredLE
	} else {
		payload.unsigned = make([]uint16, count)
	}
	descriptor.SampleDomain = VolumeSampleDomainStored
	descriptor.RescaleSlope, descriptor.RescaleIntercept = plan.slope, plan.intercept
	descriptor.RowStrideBytes = uint64(v.Cols) * 2
	descriptor.SliceStrideBytes = uint64(v.Rows) * descriptor.RowStrideBytes
	descriptor.ByteLength = payload.byteLen()
	decoder := &progressiveDecoder{
		volume: v, observe: observe, descriptor: descriptor, skipPreview: options.SkipPreview, reportEvenSlices: options.ReportEvenSlices, packed: payload, ready: make([]bool, v.Depth),
	}
	if err := decodeProgressiveSlices(ctx, v.Depth, count, func(index int) error {
		return decodePacked16SliceInto(ctx, v, payload, index)
	}, decoder.sliceDecoded); err != nil {
		return nil, err
	}
	return payload, nil
}

// decodeFloat32SliceInto decodes slice index into its plane of values.
func decodeFloat32SliceInto(ctx context.Context, v *Volume, values []float32, index int) error {
	plane := v.Cols * v.Rows
	slice, err := loadFrameContext(ctx, v.slices[index])
	if err != nil {
		return fmt.Errorf("render: load volume frame %d: %w", index+1, err)
	}
	if v.Components() == 3 {
		decoded, err := decodeSliceFrame(slice)
		if err != nil {
			return fmt.Errorf("render: decode color volume frame %d: %w", index+1, err)
		}
		if len(decoded.rgba) != plane*4 {
			return fmt.Errorf("%w: incomplete color frame", ErrInvalidVolumeSnapshot)
		}
		base := index * plane * 3
		for pixel := 0; pixel < plane; pixel++ {
			for component := 0; component < 3; component++ {
				values[base+pixel*3+component] = float32(decoded.rgba[pixel*4+component])
			}
		}
		return nil
	}
	if err := decodeGrayscaleSliceInto(values[index*plane:(index+1)*plane], slice); err != nil {
		return fmt.Errorf("render: decode volume frame %d: %w", index+1, err)
	}
	return nil
}

// decodePacked16SliceInto decodes the stored samples of slice index.
func decodePacked16SliceInto(ctx context.Context, v *Volume, payload *packed16VolumePayload, index int) error {
	plane := v.Cols * v.Rows
	slice, err := loadFrameContext(ctx, v.slices[index])
	if err != nil {
		return fmt.Errorf("render: load volume frame %d: %w", index+1, err)
	}
	metadata := effectiveFrameMetadata(slice)
	frame, err := grayscaleDisplayFrame(slice, metadata, normalizedPhotometric(metadata.PhotometricInterpretation), v.Rows, v.Cols)
	if err != nil {
		return fmt.Errorf("render: decode volume frame %d: %w", index+1, err)
	}
	if payload.signed != nil {
		err = display.DecodeStoredInto(payload.signed[index*plane:(index+1)*plane], frame)
	} else {
		err = display.DecodeStoredInto(payload.unsigned[index*plane:(index+1)*plane], frame)
	}
	if err != nil {
		return fmt.Errorf("render: decode volume frame %d: %w", index+1, err)
	}
	return nil
}
