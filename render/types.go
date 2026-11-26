package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ThalesMMS/dicom-go/icc"
	"image"
	"sync"

	"github.com/ThalesMMS/dicom-go/dynamic"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

const (
	defaultWindowCenter = 40.0
	defaultWindowWidth  = 400.0
)

// WindowLevel is a DICOM window center/width pair used for display rendering.
type WindowLevel struct {
	Center   float64
	Width    float64
	Function display.VOIFunction
	LUT      *display.LUT
}

// Rescale applies DICOM modality LUT slope/intercept to stored pixel values.
type Rescale struct {
	Slope     float64
	Intercept float64
}

// FrameIdentity identifies one immutable render-input generation. It contains no
// source data. A fresh value must be used after pixels or rendering metadata
// change. Its address, never SOP UID/path, is the cache identity.
// The non-zero size keeps distinct allocations distinct.
type FrameIdentity struct{ marker byte }

// Frame is the headless render input for one decoded DICOM image frame.
type Frame struct {
	// CacheIdentity optionally shares cache entries between adapters for the
	// same immutable input generation. Nil preserves pointer identity. A Frame
	// and its identity must remain immutable while retained by a cache. Callers
	// must not reuse an identity for different pixels or rendering metadata.
	CacheIdentity *FrameIdentity `json:"-"`

	SOPInstanceUID string
	FrameIndex     int

	Metadata pixeldata.Metadata

	TransferSyntaxUID  string
	TransferSyntaxName string
	ByteOrder          binary.ByteOrder
	PixelBytes         []byte
	// LoadPixels optionally supplies a decoded immutable frame on demand. Geometry
	// and preparation estimates use this Frame's metadata without calling it.
	// The returned frame must have no loader and its bytes must remain valid for
	// the duration of the call. Implementations must honor cancellation and allow
	// concurrent calls; the render engine never mutates either frame.
	LoadPixels func(context.Context) (*Frame, error) `json:"-"`

	DefaultWindow WindowLevel
	Rescale       Rescale
	// PreserveModalityPrecision retains binary64 modality values in the 2D
	// render cache. It does not alter volume/GPU scalar formats. Set before
	// sharing a Frame with a cache; render inputs are immutable while cached.
	PreserveModalityPrecision bool
	// ColorTransform, when set, converts colour frames (three samples or
	// PALETTE COLOR) from the ICC Profile's colour space to sRGB after
	// rendering; nil leaves the samples as stored.
	ColorTransform *icc.Transform
	// PixelPadding, when set, marks stored values automatic windows ignore
	// (Pixel Padding Value and Range Limit).
	PixelPadding *pixeldata.PixelPadding

	ImagePosition    []float64
	ImageOrientation []float64
	PixelSpacing     []float64
	SliceThickness   float64
	// SpacingBetweenSlices is Spacing Between Slices (0018,0088) as read,
	// sign included; zero when absent. With SliceThickness it sizes the
	// through-plane voxel only when slice positions cannot
	// (object.ResolveSliceSpacing).
	SpacingBetweenSlices float64
	SliceLocation        float64
	SliceLocationOK      bool
	InstanceNumber       int
	Sort                 float64
	// Temporal preserves the frame's independent temporal/spatial identity.
	// Geometry guardrails use it to reject an unsplit 4D acquisition before it
	// can be mistaken for one 3D volume.
	Temporal dynamic.FrameMetadata
	// EchoNumber is the first value of Echo Numbers (0018,0086) and
	// AcquisitionNumber is Acquisition Number (0020,0012), each with an
	// explicit presence flag so a real zero is never confused with an absent
	// attribute. Stack.VolumeCandidates uses them to separate echoes and,
	// on request, acquisitions that share one Series Instance UID.
	EchoNumber           int
	HasEchoNumber        bool
	AcquisitionNumber    int
	HasAcquisitionNumber bool

	DecodeErr    error
	Encapsulated bool
}

// Stack is the headless render input for a sorted frame stack.
type Stack struct {
	UID       string
	StudyUID  string
	Modality  string
	BodyPart  string
	Thumbnail image.Image

	DefaultWindow WindowLevel

	PixelSpacing   []float64
	SliceThickness float64
	// SpacingBetweenSlices is the series Spacing Between Slices (0018,0088),
	// sign included; zero when absent.
	SpacingBetweenSlices float64
	Frames               []*Frame

	volumeMu     sync.Mutex
	volumeVoxels *volumeVoxels
	mprVolume    *Volume
	volumeClosed bool
	volumeStore  *VolumeStore
}

// SetThumbnail updates presentation-only preview state without invalidating or
// regenerating clinical voxel data.
func (s *Stack) SetThumbnail(thumbnail image.Image) {
	if s == nil {
		return
	}
	s.volumeMu.Lock()
	s.Thumbnail = thumbnail
	s.volumeMu.Unlock()
}

// ThumbnailImage returns the presentation-only preview under the stack lock.
func (s *Stack) ThumbnailImage() image.Image {
	if s == nil {
		return nil
	}
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	return s.Thumbnail
}

// Close releases normalized/regularized volume generations owned by this
// stack. Source Frame.PixelBytes remain caller-owned so the independent 2D
// path can define its own study lifecycle.
func (s *Stack) Close() error {
	if s == nil {
		return nil
	}
	s.volumeMu.Lock()
	volume := s.mprVolume
	store := s.volumeStore
	s.mprVolume = nil
	s.volumeVoxels = nil
	s.volumeClosed = true
	s.volumeMu.Unlock()
	if volume != nil {
		return volume.Close()
	}
	if store != nil {
		return store.Close()
	}
	return nil
}

// VolumeStoreStats reports canonical volume residency without building a
// volume when one has not yet been requested.
func (s *Stack) VolumeStoreStats() VolumeStoreStats {
	if s == nil {
		return VolumeStoreStats{}
	}
	s.volumeMu.Lock()
	volume := s.mprVolume
	store := s.volumeStore
	s.volumeMu.Unlock()
	if volume == nil {
		if store == nil {
			return VolumeStoreStats{}
		}
		return store.Stats()
	}
	return volume.VolumeStoreStats()
}

// SetVolumeStore transfers a caller-supplied store to the Stack before the
// first volume build. Stack.Close closes it. This is the integration point for
// a study-level hard memory budget.
func (s *Stack) SetVolumeStore(store *VolumeStore) error {
	if s == nil || store == nil {
		return fmt.Errorf("%w: nil volume store", ErrInvalidVolumeSnapshot)
	}
	s.volumeMu.Lock()
	defer s.volumeMu.Unlock()
	if s.volumeClosed {
		return ErrVolumeStoreClosed
	}
	if s.mprVolume != nil || s.volumeStore != nil {
		return ErrVolumeInUse
	}
	s.volumeStore = store
	return nil
}

// FirstDisplayFrame returns the first frame that can be displayed.
func (s *Stack) FirstDisplayFrame() *Frame {
	if s == nil {
		return nil
	}
	for _, frame := range s.Frames {
		if frame != nil && frame.DecodeErr == nil && !frame.Encapsulated {
			return frame
		}
	}
	return nil
}

// Displayable reports whether the stack has at least one renderable frame.
func (s *Stack) Displayable() bool {
	return s.FirstDisplayFrame() != nil
}
