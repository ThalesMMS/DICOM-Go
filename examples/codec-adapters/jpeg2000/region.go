package jpeg2000

import (
	"context"
	"errors"
	"fmt"
	"image"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"

	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
)

// coreBackend is the core decoder of a codec that decodes with it.
func (c *Codec) coreBackend() (coreDecoder, bool) {
	if c == nil || c.format != encapsulated.JPEG2000 {
		return coreDecoder{}, false
	}
	core, ok := c.decoder.(coreDecoder)
	return core, ok
}

// DecodeFrameRegion implements pixeldata.FrameRegionDecoder with the core
// backend (JPEG 2000 Part 1 and HTJ2K): only the tiles region overlaps are
// decoded, so a region of a tiled image (whole-slide imaging) costs those
// tiles rather than the frame. Other backends return
// pixeldata.ErrRegionUnsupported so the caller decodes the whole frame and
// crops it.
func (c *Codec) DecodeFrameRegion(ctx context.Context, codestream []byte, metadata pixeldata.Metadata, region image.Rectangle) (pixeldata.Frames, error) {
	core, ok := c.coreBackend()
	if !ok || isYBRFull422(metadata) {
		// A YBR_FULL_422 frame is decoded whole and converted to RGB.
		return pixeldata.Frames{}, pixeldata.ErrRegionUnsupported
	}
	if err := validateMetadata(metadata); err != nil {
		return pixeldata.Frames{}, err
	}
	data, err := core.decodePartial(ctx, codestream, metadata, pixeldata.PartialDecodeRequest{Region: region})
	if err != nil {
		return pixeldata.Frames{}, err
	}
	return pixeldata.Frames{Rows: region.Dy(), Columns: region.Dx(), Data: [][]byte{data}}, nil
}

// PartialDecodeCapabilities implements pixeldata.PartialFrameDecoder: the
// core backend decodes regions by tile, reduces by up to the fewest
// decomposition levels of any tile-component (when the image origin lies on
// the reduced grid, ISO/IEC 15444-1 B.5) and truncates to any number of the
// quality layers. Other backends report nothing.
func (c *Codec) PartialDecodeCapabilities(codestream []byte, metadata pixeldata.Metadata) (pixeldata.PartialDecodeCapabilities, error) {
	core, ok := c.coreBackend()
	if !ok || isYBRFull422(metadata) {
		return pixeldata.PartialDecodeCapabilities{}, nil
	}
	stream, err := core.parse(codestream, metadata)
	if err != nil {
		return pixeldata.PartialDecodeCapabilities{}, err
	}
	levels, layers := codestreamScalability(stream)
	levels = min(levels, pixeldata.MaxReductionLevel)
	for levels > 0 && !originOnReducedGrid(stream, levels) {
		levels--
	}
	return pixeldata.PartialDecodeCapabilities{Region: true, ReductionLevels: levels, QualityLayers: layers}, nil
}

// DecodeFramePartial implements pixeldata.PartialFrameDecoder with the core
// backend: only the tiles the region overlaps are decoded, the reduction
// drops the finest resolution levels and the packets that carry them, and
// the quality layers after request.QualityLayers are not decoded. A
// reduction beyond the decomposition levels is pixeldata.ErrInvalidReduction
// (and ErrInvalidReduction), layers beyond the codestream's are
// pixeldata.ErrInvalidQualityLayers. Other backends, and an image origin
// off the reduced grid, return pixeldata.ErrPartialDecodeUnsupported.
func (c *Codec) DecodeFramePartial(ctx context.Context, codestream []byte, metadata pixeldata.Metadata, request pixeldata.PartialDecodeRequest) (pixeldata.Frames, error) {
	core, ok := c.coreBackend()
	if !ok || isYBRFull422(metadata) {
		return pixeldata.Frames{}, pixeldata.ErrPartialDecodeUnsupported
	}
	if err := validateMetadata(metadata); err != nil {
		return pixeldata.Frames{}, err
	}
	data, err := core.decodePartial(ctx, codestream, metadata, request)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	out := partialOutput(request, metadata)
	return pixeldata.Frames{Rows: out.Dy(), Columns: out.Dx(), Data: [][]byte{data}}, nil
}

// partialOutput is the rectangle a request decodes to: its region (the
// whole frame when zero) on the reduced grid.
func partialOutput(request pixeldata.PartialDecodeRequest, metadata pixeldata.Metadata) image.Rectangle {
	region := request.Region
	if region == (image.Rectangle{}) {
		region = image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows))
	}
	return pixeldata.ReducedRegion(region, request.ReductionLevel)
}

// codestreamScalability is the fewest decomposition levels of any
// tile-component, and the most quality layers of any tile.
func codestreamScalability(stream *j2kcore.Codestream) (levels, layers int) {
	levels = -1
	for tile := 0; tile < stream.NumTiles(); tile++ {
		for comp := range stream.Components {
			style := stream.CodingForTile(tile, comp)
			if levels < 0 || style.Levels < levels {
				levels = style.Levels
			}
			layers = max(layers, style.Layers)
		}
	}
	return max(levels, 0), layers
}

// originOnReducedGrid reports whether the decoded image's origin is a
// multiple of 2^reduce on the output grid, so that a reduced sample's cell
// starts at a multiple of 2^reduce of the frame, as
// pixeldata.ReducedRegion maps it.
func originOnReducedGrid(stream *j2kcore.Codestream, reduce int) bool {
	ox, oy := stream.OutputOrigin(0)
	return ox%(1<<reduce) == 0 && oy%(1<<reduce) == 0
}

// parse applies the input budget and JP2 policy, parses the codestream and
// checks it against metadata.
func (d coreDecoder) parse(payload []byte, metadata pixeldata.Metadata) (*j2kcore.Codestream, error) {
	maxInput := d.options.Limits.MaxInputBytes
	if maxInput <= 0 {
		maxInput = 1 << 30
	}
	if int64(len(payload)) > maxInput {
		return nil, fmt.Errorf("%w: codestream of %d bytes, limit %d", ErrResourceLimit, len(payload), maxInput)
	}
	if isJP2(payload) && !d.options.AllowJP2Wrapper {
		return nil, ErrJP2Wrapped
	}
	stream, err := j2kcore.ParseCodestream(payload)
	if err != nil {
		return nil, classifyCoreError(err)
	}
	if err := checkCoreGeometry(stream, metadata); err != nil {
		return nil, err
	}
	return stream, nil
}

// decodePartial decodes part of one frame with the core backend, with the
// checks and sample packing of DecodeFrameContext.
func (d coreDecoder) decodePartial(ctx context.Context, payload []byte, metadata pixeldata.Metadata, request pixeldata.PartialDecodeRequest) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stream, err := d.parse(payload, metadata)
	if err != nil {
		return nil, err
	}
	bounds := image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows))
	region := request.Region
	if region == (image.Rectangle{}) {
		region = bounds
	}
	if region.Empty() || !region.In(bounds) {
		return nil, fmt.Errorf("%w: %v", pixeldata.ErrInvalidRegion, region)
	}
	reduce, layers := request.ReductionLevel, request.QualityLayers
	levels, available := codestreamScalability(stream)
	switch {
	case reduce < 0 || reduce > levels:
		return nil, fmt.Errorf("%w: %w: %d levels asked, the codestream has %d", pixeldata.ErrInvalidReduction, ErrInvalidReduction, reduce, levels)
	case layers < 0 || layers > available:
		return nil, fmt.Errorf("%w: %d asked, the codestream has %d", pixeldata.ErrInvalidQualityLayers, layers, available)
	case !originOnReducedGrid(stream, reduce):
		return nil, fmt.Errorf("%w: image origin off the grid reduced by 2^%d", pixeldata.ErrPartialDecodeUnsupported, reduce)
	}
	reinterpret, err := d.signedness(stream, metadata)
	if err != nil {
		return nil, err
	}
	out := pixeldata.ReducedRegion(region, reduce)
	limits := d.options.Limits
	decoded, err := j2kcore.DecodeContextOptions(ctx, stream, j2kcore.Limits{
		MaxComponents:        limits.MaxComponents,
		MaxTiles:             limits.MaxTiles,
		MaxResolutions:       limits.MaxResolutions,
		MaxSamples:           limits.MaxSamples,
		MaxTileWorkBytes:     limits.MaxTileWorkBytes,
		MaxCodeBlocksPerTile: limits.MaxCodeBlocksPerTile,
	}, j2kcore.DecodeOptions{Region: out, Reduce: reduce, Layers: layers})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, classifyCoreError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	partMetadata := metadata
	partMetadata.Rows, partMetadata.Columns = uint16(out.Dy()), uint16(out.Dx())
	return packCoreSamples(decoded, partMetadata, reinterpret)
}
