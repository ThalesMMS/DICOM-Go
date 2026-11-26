package nrrd

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/seg"
)

// SlicerSegmentMask describes one segment on an already aligned grid.
// Slices are ordered along increasing k. A nil plane has no foreground,
// and an omitted slice list declares an entirely empty segment. Each
// supplied plane must match the grid's columns and rows.
type SlicerSegmentMask struct {
	Segment seg.Segment
	Slices  []*roi.RasterMask
}

// PackSlicerSegmentation prepares a layered labelmap and its segment metadata
// for WriteSlicerSegmentation. In input order, each segment takes the first
// layer with a free label that does not overlap earlier segments on that layer.
// Labels start at 1 per layer, including labels reserved for empty segments.
// The volume uses unsigned 8-bit labels when they fit and 16-bit otherwise.
// The volume owns its packed voxel data. The limits apply across all layers,
// with zero fields using NIfTI defaults.
func PackSlicerSegmentation(ctx context.Context, dimensions [3]int, indexToLPS render.GeometryAffine, masks []SlicerSegmentMask, limits nifti.ReadLimits) (*nifti.Volume, []SlicerSegment, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if len(masks) == 0 || !indexToLPS.Finite() {
		return nil, nil, fmt.Errorf("%w: missing segments or invalid LPS affine", ErrMalformedHeader)
	}
	if len(masks) > 1<<16 {
		return nil, nil, fmt.Errorf("%w: too many Slicer segments", ErrLimitExceeded)
	}
	defaults := nifti.DefaultReadLimits()
	if limits.MaxVoxels == 0 {
		limits.MaxVoxels = defaults.MaxVoxels
	}
	if limits.MaxPayloadBytes == 0 {
		limits.MaxPayloadBytes = defaults.MaxPayloadBytes
	}
	packed := slicerLabelLayers{width: 1, voxels: 1, limits: limits}
	sampleLimit := min(limits.MaxVoxels, limits.MaxPayloadBytes, uint64(math.MaxInt))
	for axis, size := range dimensions {
		if size < 1 || size > math.MaxInt32 {
			return nil, nil, fmt.Errorf("%w: size %d of axis %d", ErrMalformedHeader, size, axis)
		}
		if uint64(size) > sampleLimit/uint64(packed.voxels) {
			return nil, nil, fmt.Errorf("%w: Slicer grid dimensions %v", ErrLimitExceeded, dimensions)
		}
		packed.voxels *= size
	}

	segments := make([]SlicerSegment, 0, len(masks))
	for index, mask := range masks {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(mask.Slices) != 0 && len(mask.Slices) != dimensions[2] {
			return nil, nil, fmt.Errorf("%w: segment %d has %d slices, want %d", ErrMalformedHeader, index, len(mask.Slices), dimensions[2])
		}
		layer, label, err := packed.place(ctx, dimensions, mask.Slices)
		if err != nil {
			return nil, nil, fmt.Errorf("Slicer segment %d: %w", index, err)
		}
		segment := NewSlicerSegment(mask.Segment, label, layer)
		segment.Index = index
		segments = append(segments, segment)
	}
	readers := make([]io.Reader, len(packed.layers))
	for i, layer := range packed.layers {
		readers[i] = bytes.NewReader(layer.data)
	}
	datatype := nifti.DatatypeUint8
	if packed.width == 2 {
		datatype = nifti.DatatypeUint16
	}
	volume, err := nifti.NewVolume(ctx, io.MultiReader(readers...), nifti.NewVolumeOptions{
		Datatype: datatype, Dimensions: [4]int{dimensions[0], dimensions[1], dimensions[2], len(packed.layers)},
		IndexToLPS: indexToLPS, Limits: limits,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return volume, segments, nil
}

type slicerLabelLayer struct {
	data      []byte
	lastLabel int
}

type slicerLabelLayers struct {
	layers        []slicerLabelLayer
	width, voxels int
	limits        nifti.ReadLimits
}

func (p *slicerLabelLayers) place(ctx context.Context, dimensions [3]int, slices []*roi.RasterMask) (int, int, error) {
	index := len(p.layers)
	for candidate, layer := range p.layers {
		if layer.lastLabel == math.MaxUint16 {
			continue
		}
		overlap, err := visitSlicerMask(ctx, dimensions, slices, layer.data, p.width, 0)
		if err != nil {
			return 0, 0, err
		}
		if !overlap {
			index = candidate
			break
		}
	}
	if index == len(p.layers) {
		if err := p.checkSize(index+1, p.width); err != nil {
			return 0, 0, err
		}
		p.layers = append(p.layers, slicerLabelLayer{data: make([]byte, p.voxels*p.width)})
	}
	label := p.layers[index].lastLabel + 1
	if p.width == 1 && label > math.MaxUint8 {
		if err := p.widen(ctx); err != nil {
			return 0, 0, err
		}
	}
	layer := &p.layers[index]
	if _, err := visitSlicerMask(ctx, dimensions, slices, layer.data, p.width, uint16(label)); err != nil {
		return 0, 0, err
	}
	layer.lastLabel = label
	return index, label, nil
}

func (p *slicerLabelLayers) checkSize(layers, width int) error {
	byteLimit := min(p.limits.MaxPayloadBytes, uint64(math.MaxInt))
	if uint64(layers) > p.limits.MaxVoxels/uint64(p.voxels) ||
		uint64(layers) > byteLimit/uint64(width)/uint64(p.voxels) {
		return fmt.Errorf("%w: %d Slicer layers of %d voxels at %d bytes per voxel", ErrLimitExceeded, layers, p.voxels, width)
	}
	return nil
}

func (p *slicerLabelLayers) widen(ctx context.Context) error {
	if err := p.checkSize(len(p.layers), 2); err != nil {
		return err
	}
	for i := range p.layers {
		if err := ctx.Err(); err != nil {
			return err
		}
		widened := make([]byte, p.voxels*2)
		for j, label := range p.layers[i].data {
			if j%8192 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			widened[j*2] = label
		}
		p.layers[i].data = widened
	}
	p.width = 2
	return nil
}

// visitSlicerMask checks for overlap when label is zero and packs the label
// otherwise. Runs avoid a temporary list with an entry for every set voxel.
func visitSlicerMask(ctx context.Context, dimensions [3]int, slices []*roi.RasterMask, data []byte, width int, label uint16) (bool, error) {
	columns, rows := dimensions[0], dimensions[1]
	for z, mask := range slices {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if mask == nil {
			continue
		}
		if mask.Columns != columns || mask.Rows != rows {
			return false, fmt.Errorf("%w: slice %d mask size %dx%d, want %dx%d", ErrMalformedHeader, z, mask.Columns, mask.Rows, columns, rows)
		}
		for y := 0; y < rows; y++ {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			row := (z*rows + y) * columns
			for _, run := range mask.Runs(y) {
				if run.Start < 0 || run.End > columns || run.Start >= run.End {
					return false, fmt.Errorf("%w: slice %d row %d mask run [%d,%d)", ErrMalformedHeader, z, y, run.Start, run.End)
				}
				for x := run.Start; x < run.End; x++ {
					if x%8192 == 0 {
						if err := ctx.Err(); err != nil {
							return false, err
						}
					}
					offset := (row + x) * width
					switch {
					case label == 0:
						if data[offset] != 0 || (width == 2 && data[offset+1] != 0) {
							return true, nil
						}
					case width == 1:
						data[offset] = byte(label)
					default:
						binary.LittleEndian.PutUint16(data[offset:], label)
					}
				}
			}
		}
	}
	return false, nil
}
