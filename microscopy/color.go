package microscopy

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"sync"

	"github.com/ThalesMMS/dicom-go/icc"
)

const (
	MaxOpticalPaths            = 64
	MaxOpticalPathProfileBytes = 1 << 20
	MaxInstanceProfileBytes    = 8 << 20
	MaxColorSources            = 4096
	MaxColorProfileBytes       = 32 << 20
)

var (
	ErrUnknownOpticalPath        = errors.New("dicom/microscopy: unknown optical path")
	ErrMissingOpticalPath        = errors.New("dicom/microscopy: frame optical path unavailable")
	ErrMissingOpticalPathProfile = errors.New("dicom/microscopy: optical path ICC absent")
	ErrColorResourceLimit        = errors.New("dicom/microscopy: color metadata resource limit")
)

type ColorState string

const (
	ColorConverted          ColorState = "converted"
	ColorIdentity           ColorState = "sRGB"
	ColorMissingProfile     ColorState = "missing-profile"
	ColorInvalidProfile     ColorState = "invalid-profile"
	ColorUnsupportedProfile ColorState = "unsupported-profile"
	ColorUnknownPath        ColorState = "unknown-path"
	ColorMissingPath        ColorState = "missing-path"
	ColorResourceLimited    ColorState = "resource-limited"
)

// ColorDiagnostic identifies the exact source/path and the reason colours
// cannot be converted. Missing/invalid/unsupported ICC shows stored colours;
// missing/unknown references and resource refusals prevent tile publication.
type ColorDiagnostic struct {
	Source      InstanceRef
	OpticalPath string
	State       ColorState
	Cause       error
}

func (d ColorDiagnostic) Error() string {
	return fmt.Sprintf("optical path %q: %s", d.OpticalPath, d.State)
}
func (d ColorDiagnostic) Unwrap() error { return d.Cause }

type ColorResult struct {
	Transform          *icc.Transform
	ProfileDescription string
	Diagnostic         ColorDiagnostic
}

// FrameOpticalPath resolves a one-based frame without decoding Pixel Data or
// materializing the total matrix. Explicit frame identifiers outrank sequence
// order; TILED_FULL order is spatial tiles, focal depth, then optical path.
func (i Instance) FrameOpticalPath(frame int) (string, error) {
	l := i.Level
	if l.TileWidth <= 0 || l.TileHeight <= 0 || l.MatrixWidth <= 0 || l.MatrixHeight <= 0 {
		return "", ErrInvalidSlide
	}
	if frame < 1 || frame > l.FrameCount {
		return "", fmt.Errorf("%w: frame %d", ErrInvalidSlide, frame)
	}
	if !l.ImplicitTiling {
		if frame > len(l.Tiles) {
			return "", ErrMissingOpticalPath
		}
		id := l.Tiles[frame-1].OpticalPath
		if id == "" {
			return "", ErrMissingOpticalPath
		}
		return id, nil
	}
	paths := l.OpticalPathIDs
	if len(paths) == 0 {
		return "", ErrMissingOpticalPath
	}
	count := int64(ceilDiv(l.MatrixWidth, l.TileWidth)) * int64(ceilDiv(l.MatrixHeight, l.TileHeight)) * int64(max(1, len(l.FocalPlaneOffsets)))
	if count <= 0 {
		return "", ErrInvalidSlide
	}
	index := int(int64(frame-1+l.FrameOffset) / count)
	if index >= len(paths) || paths[index] == "" {
		return "", ErrMissingOpticalPath
	}
	return paths[index], nil
}

// ColorResolver owns bounded per-source profiles. Identical identifiers in
// different instances remain separate, while profile transforms share an LRU.
// Imported Tile.OpticalPath is the binding produced by Read/Level; ResolveFrame
// additionally resolves it directly from the frame index for non-tile callers.
type ColorResolver struct {
	mu       sync.RWMutex
	profiles map[InstanceRef]map[string][]byte
	bytes    int
	cache    *icc.TransformCache
}

func NewColorResolver(cache *icc.TransformCache) *ColorResolver {
	if cache == nil {
		cache = icc.NewTransformCache(icc.CacheOptions{})
	}
	return &ColorResolver{profiles: make(map[InstanceRef]map[string][]byte), cache: cache}
}
func (r *ColorResolver) Add(i Instance) error {
	if r == nil {
		return ErrColorResourceLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.profiles[i.Level.Source]; exists {
		return fmt.Errorf("%w: duplicate color source", ErrInvalidSlide)
	}
	if len(r.profiles) >= MaxColorSources || len(i.OpticalPaths) > MaxOpticalPaths {
		return ErrColorResourceLimit
	}
	paths := make(map[string][]byte, len(i.OpticalPaths))
	count := 0
	// Admission precedes any payload allocation/publication.
	for _, path := range i.OpticalPaths {
		if path.Identifier == "" {
			return ErrMissingOpticalPath
		}
		if len(path.Identifier) > 64 {
			return ErrColorResourceLimit
		}
		if _, exists := paths[path.Identifier]; exists {
			return fmt.Errorf("%w: duplicate optical path", ErrInvalidSlide)
		}
		if len(path.ICCProfile) > MaxOpticalPathProfileBytes || len(path.ICCProfile) > MaxInstanceProfileBytes-count {
			return ErrColorResourceLimit
		}
		count += len(path.ICCProfile)
		paths[path.Identifier] = nil
	}
	if count > MaxColorProfileBytes-r.bytes {
		return ErrColorResourceLimit
	}
	for _, path := range i.OpticalPaths {
		paths[path.Identifier] = append([]byte(nil), path.ICCProfile...)
	}
	r.profiles[i.Level.Source] = paths
	r.bytes += count
	return nil
}
func (r *ColorResolver) ResolveFrame(i Instance, frame int) ColorResult {
	id, err := i.FrameOpticalPath(frame)
	if err != nil {
		return ColorResult{Diagnostic: ColorDiagnostic{Source: i.Level.Source, State: ColorMissingPath, Cause: err}}
	}
	return r.ResolveTile(Tile{Source: i.Level.Source, FrameNumber: frame, OpticalPath: id})
}
func (r *ColorResolver) ResolveTile(tile Tile) ColorResult {
	d := ColorDiagnostic{Source: tile.Source, OpticalPath: tile.OpticalPath}
	if tile.OpticalPath == "" {
		d.State = ColorMissingPath
		d.Cause = ErrMissingOpticalPath
		return ColorResult{Diagnostic: d}
	}
	if r == nil {
		d.State = ColorResourceLimited
		d.Cause = ErrColorResourceLimit
		return ColorResult{Diagnostic: d}
	}
	r.mu.RLock()
	data, ok := r.profiles[tile.Source][tile.OpticalPath]
	r.mu.RUnlock()
	if !ok {
		d.State = ColorUnknownPath
		d.Cause = ErrUnknownOpticalPath
		return ColorResult{Diagnostic: d}
	}
	if len(data) == 0 {
		d.State = ColorMissingProfile
		d.Cause = ErrMissingOpticalPathProfile
		return ColorResult{Diagnostic: d}
	}
	value := r.cache.Resolve(data)
	switch {
	case errors.Is(value.Err, icc.ErrCacheResourceLimit):
		d.State = ColorResourceLimited
		d.Cause = value.Err
	case errors.Is(value.Err, icc.ErrUnsupportedProfile):
		d.State = ColorUnsupportedProfile
		d.Cause = value.Err
	case value.Err != nil:
		d.State = ColorInvalidProfile
		d.Cause = value.Err
	case value.Transform.Identity():
		d.State = ColorIdentity
	default:
		d.State = ColorConverted
	}
	return ColorResult{Transform: value.Transform, ProfileDescription: value.Description, Diagnostic: d}
}
func (r *ColorResolver) CacheStats() icc.CacheStats {
	if r == nil {
		return icc.CacheStats{}
	}
	return r.cache.Stats()
}
func (r *ColorResolver) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.profiles = make(map[InstanceRef]map[string][]byte)
	r.bytes = 0
	r.cache.Clear()
}

// ColorManagedTileSource applies the selected ICC before the decoded-tile LRU
// and compositing, so display and rendered export consume the same colours.
// The source image is never mutated, even when a source reuses its buffers.
type ColorManagedTileSource struct {
	Source TileSource
	Colors *ColorResolver
}

func (s ColorManagedTileSource) FetchTile(ctx context.Context, tile Tile) (image.Image, error) {
	result := s.Colors.ResolveTile(tile)
	switch result.Diagnostic.State {
	case ColorUnknownPath, ColorMissingPath, ColorResourceLimited:
		return nil, result.Diagnostic
	}
	if s.Source == nil {
		return nil, fmt.Errorf("microscopy tile source is nil")
	}
	img, err := s.Source.FetchTile(ctx, tile)
	if err != nil || img == nil {
		return img, err
	}
	if result.Transform == nil || result.Transform.Identity() {
		return img, nil
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || int64(bounds.Dx()) > (16<<20)/int64(bounds.Dy()) {
		return nil, ErrColorResourceLimit
	}
	switch img.(type) {
	case *image.RGBA64, *image.NRGBA64:
		owned := image.NewRGBA64(bounds)
		draw.Draw(owned, bounds, img, bounds.Min, draw.Src)
		return result.Transform.Apply(owned), nil
	}
	owned := image.NewNRGBA(bounds)
	draw.Draw(owned, bounds, img, bounds.Min, draw.Src)
	return result.Transform.Apply(owned), nil
}
