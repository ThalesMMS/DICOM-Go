// Package derivation records how an image was derived from other instances:
// the Source Image Sequence (0008,2112) with each reference's Purpose of
// Reference, the Derivation Code Sequence (0008,9215) and the Derivation
// Description (0008,2111). Classic images carry them at the top level
// (General Image Module, PS3.3 C.7.6.1); multi-frame functional group IODs
// carry them in the Derivation Image Functional Group (PS3.3 C.7.6.16.2.6).
package derivation

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagCodeValue                   = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator      = core.NewTag(0x0008, 0x0102)
	tagCodeMeaning                 = core.NewTag(0x0008, 0x0104)
	tagReferencedSOPClassUID       = core.NewTag(0x0008, 0x1150)
	tagReferencedSOPInstanceUID    = core.NewTag(0x0008, 0x1155)
	tagReferencedFrameNumber       = core.NewTag(0x0008, 0x1160)
	tagDerivationDescription       = core.NewTag(0x0008, 0x2111)
	tagSourceImageSequence         = core.NewTag(0x0008, 0x2112)
	tagDerivationImageSequence     = core.NewTag(0x0008, 0x9124)
	tagDerivationCodeSequence      = core.NewTag(0x0008, 0x9215)
	tagPurposeOfReference          = core.NewTag(0x0040, 0xa170)
	tagSharedFunctionalGroups      = core.NewTag(0x5200, 0x9229)
	tagPerFrameFunctionalGroups    = core.NewTag(0x5200, 0x9230)
	tagSOPClassUID                 = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID              = core.NewTag(0x0008, 0x0018)
	maxDerivationDescriptionLength = 1024 // ST
)

// ErrInvalid reports a code, reference or data set that cannot be recorded.
var ErrInvalid = errors.New("dicom/derivation: invalid derivation")

// Code is a coded entry (Code Sequence Macro, PS3.3 Table 8.8-1).
type Code struct {
	Value, Scheme, Meaning string
}

// Codes of CID 7202 (Source Image Purposes of Reference) and CID 7203 (Image
// Derivation) used by the library.
var (
	// LossyCompression (CID 7203) derives an image by lossy compression.
	LossyCompression = Code{"113040", "DCM", "Lossy Compression"}
	// Segmentation (CID 7203) derives a Segmentation from its source images.
	Segmentation = Code{"113076", "DCM", "Segmentation"}
	// MultiplanarReformatting (CID 7203) derives a plane resampled from a
	// volume (MPR).
	MultiplanarReformatting = Code{"113072", "DCM", "Multiplanar reformatting"}
	// CurvedMultiplanarReformatting (CID 7203) derives an image resampled
	// along a curve (CPR).
	CurvedMultiplanarReformatting = Code{"113073", "DCM", "Curved multiplanar reformatting"}
	// MaximumIntensityProjection and MinimumIntensityProjection (CID 7203)
	// derive a slab projection.
	MaximumIntensityProjection = Code{"113078", "DCM", "Maximum intensity projection"}
	MinimumIntensityProjection = Code{"113079", "DCM", "Minimum intensity projection"}
	// SpatialResampling (CID 7203) derives an image resampled to another
	// grid.
	SpatialResampling = Code{"113085", "DCM", "Spatial resampling"}
	// UncompressedPredecessor (CID 7202) references the image a lossy
	// compressed one was made from.
	UncompressedPredecessor = Code{"121320", "DCM", "Uncompressed predecessor"}
	// SourceImageForProcessing (CID 7202) references the input of an image
	// processing operation.
	SourceImageForProcessing = Code{"121322", "DCM", "Source image for image processing operation"}
	// MaskImageForProcessing (CID 7202) references the image subtracted
	// in an image processing operation, such as a subtraction's mask.
	MaskImageForProcessing = Code{"121321", "DCM", "Mask image for image processing operation"}
	// PixelByPixelSubtraction (CID 7203) derives an image by subtracting
	// another sample by sample.
	PixelByPixelSubtraction = Code{"113062", "DCM", "Pixel by pixel subtraction"}
	// SpatialRegistration (DCM 112353) is the purpose of a reference to the
	// Spatial Registration an image was resampled through, and
	// SourceDeformableSpatialRegistration (DCM 125028) that of a Deformable
	// Spatial Registration.
	SpatialRegistration                 = Code{"112353", "DCM", "Spatial Registration"}
	SourceDeformableSpatialRegistration = Code{"125028", "DCM", "Source Deformable Spatial Registration"}
)

func (c Code) validate() error {
	if strings.TrimSpace(c.Value) == "" || strings.TrimSpace(c.Scheme) == "" || strings.TrimSpace(c.Meaning) == "" {
		return fmt.Errorf("%w: code needs a value, scheme and meaning", ErrInvalid)
	}
	return nil
}

// Item returns the code as a sequence item.
func (c Code) Item() core.DataSet {
	return derivedio.DataSet(
		derivedio.SH(tagCodeValue, strings.TrimSpace(c.Value)),
		derivedio.SH(tagCodingSchemeDesignator, strings.TrimSpace(c.Scheme)),
		derivedio.LO(tagCodeMeaning, strings.TrimSpace(c.Meaning)),
	)
}

func (c Code) equal(item core.DataSet) bool {
	text := func(tag core.Tag) string {
		for _, e := range item.Elements {
			if e.Tag() == tag {
				values := e.StringValues()
				if len(values) == 1 {
					return strings.TrimSpace(values[0])
				}
			}
		}
		return ""
	}
	return text(tagCodeValue) == strings.TrimSpace(c.Value) && text(tagCodingSchemeDesignator) == strings.TrimSpace(c.Scheme)
}

// Source references a source instance (Image SOP Instance Reference Macro,
// PS3.3 Table 10-3).
type Source struct {
	SOPClassUID, SOPInstanceUID string
	// Frames are Referenced Frame Numbers (1-based); empty references the
	// whole instance.
	Frames []int
	// Purpose is the Purpose of Reference; nil omits it, which only the
	// top-level Source Image Sequence permits.
	Purpose *Code
}

// SourceOf references obj by its SOP Class and SOP Instance UIDs.
func SourceOf(obj *object.Object, purpose *Code) (Source, error) {
	if obj == nil {
		return Source{}, fmt.Errorf("%w: nil source", ErrInvalid)
	}
	source := Source{SOPClassUID: derivedio.CleanUID(obj, tagSOPClassUID), SOPInstanceUID: derivedio.CleanUID(obj, tagSOPInstanceUID), Purpose: purpose}
	if source.SOPClassUID == "" || source.SOPInstanceUID == "" {
		return Source{}, fmt.Errorf("%w: source has no SOP Class or SOP Instance UID", ErrInvalid)
	}
	return source, nil
}

// WithFrameIndices returns a frame-specific reference from zero-based indices
// into the physical SOP instance. A single-frame instance needs no Referenced
// Frame Number. Multi-frame selections use one-based numbers in input order,
// with duplicate selections omitted. Construct Source with no Frames to
// reference a whole instance deliberately.
func (s Source) WithFrameIndices(frameCount int, indices ...int) (Source, error) {
	if frameCount < 1 || frameCount > math.MaxInt32 || len(indices) == 0 {
		return Source{}, fmt.Errorf("%w: frame count %d and %d selected frames", ErrInvalid, frameCount, len(indices))
	}
	for _, index := range indices {
		if index < 0 || index >= frameCount {
			return Source{}, fmt.Errorf("%w: frame index %d is outside the %d-frame source", ErrInvalid, index, frameCount)
		}
	}
	s.Frames = nil
	if frameCount == 1 {
		return s, nil
	}
	if len(indices) == 1 {
		s.Frames = []int{indices[0] + 1}
		return s, nil
	}
	seen := make(map[int]struct{}, len(indices))
	s.Frames = make([]int, 0, len(indices))
	for _, index := range indices {
		if _, exists := seen[index]; !exists {
			seen[index] = struct{}{}
			s.Frames = append(s.Frames, index+1)
		}
	}
	return s, nil
}

// Item returns the reference as a Source Image Sequence item.
func (s Source) Item() (core.DataSet, error) {
	class, instance := strings.TrimSpace(s.SOPClassUID), strings.TrimSpace(s.SOPInstanceUID)
	if class == "" || instance == "" {
		return core.DataSet{}, fmt.Errorf("%w: source reference needs SOP Class and SOP Instance UIDs", ErrInvalid)
	}
	elements := []core.Element{
		derivedio.UI(tagReferencedSOPClassUID, class),
		derivedio.UI(tagReferencedSOPInstanceUID, instance),
	}
	if len(s.Frames) > 0 {
		for _, frame := range s.Frames {
			if frame < 1 {
				return core.DataSet{}, fmt.Errorf("%w: frame number %d", ErrInvalid, frame)
			}
		}
		elements = append(elements, derivedio.IS(tagReferencedFrameNumber, s.Frames...))
	}
	if s.Purpose != nil {
		if err := s.Purpose.validate(); err != nil {
			return core.DataSet{}, err
		}
		elements = append(elements, derivedio.Seq(tagPurposeOfReference, s.Purpose.Item()))
	}
	return derivedio.DataSet(elements...), nil
}

// Derivation is one derivation step.
type Derivation struct {
	// Description is appended to an existing Derivation Description after
	// "; " (the result is cut at the 1024 characters of ST).
	Description string
	// Codes are added to the Derivation Code Sequence unless present.
	Codes []Code
	// Sources are appended to the Source Image Sequence.
	Sources []Source
}

func (d Derivation) sourceItems() ([]core.DataSet, error) {
	items := make([]core.DataSet, 0, len(d.Sources))
	for _, s := range d.Sources {
		item, err := s.Item()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// Apply records d at the top level of obj (General Image Module).
func Apply(obj *object.Object, d Derivation) error {
	if obj == nil {
		return fmt.Errorf("%w: nil object", ErrInvalid)
	}
	elements, err := record(obj.Elements(), d)
	if err != nil {
		return err
	}
	for _, e := range elements {
		obj.Put(e)
	}
	return nil
}

// Item returns d as a Derivation Image Sequence item (Derivation Image
// Macro), where the Derivation Code Sequence and each source's Purpose of
// Reference are required.
func (d Derivation) Item() (core.DataSet, error) {
	if len(d.Codes) == 0 {
		return core.DataSet{}, fmt.Errorf("%w: a derivation image item needs a derivation code", ErrInvalid)
	}
	for _, s := range d.Sources {
		if s.Purpose == nil {
			return core.DataSet{}, fmt.Errorf("%w: a derivation image source needs a purpose of reference", ErrInvalid)
		}
	}
	elements, err := record(nil, d)
	if err != nil {
		return core.DataSet{}, err
	}
	if !hasTag(elements, tagSourceImageSequence) {
		elements = append(elements, derivedio.Seq(tagSourceImageSequence)) // Type 2
	}
	return derivedio.DataSet(elements...), nil
}

// ApplyFunctionalGroups records d in the Derivation Image Functional Group of
// a multi-frame functional group object: in each Per-Frame item when the
// object already keeps that group per frame (each item then references its
// own frame of whole-instance sources), otherwise in the Shared item.
func ApplyFunctionalGroups(obj *object.Object, d Derivation) error {
	if obj == nil {
		return fmt.Errorf("%w: nil object", ErrInvalid)
	}
	shared, ok := obj.Get(tagSharedFunctionalGroups)
	sharedItems, sequence := shared.Value.(core.SequenceValue)
	if !ok || !sequence || len(sharedItems.Items) != 1 {
		return fmt.Errorf("%w: no Shared Functional Groups Sequence item", ErrInvalid)
	}
	perFrame, _ := obj.Get(tagPerFrameFunctionalGroups)
	if frames, ok := perFrame.Value.(core.SequenceValue); ok && len(frames.Items) > 0 && hasTag(frames.Items[0].Elements, tagDerivationImageSequence) {
		items := make([]core.DataSet, len(frames.Items))
		for i, frame := range frames.Items {
			step := d
			step.Sources = make([]Source, len(d.Sources))
			for j, s := range d.Sources {
				if len(s.Frames) == 0 {
					s.Frames = []int{i + 1}
				}
				step.Sources[j] = s
			}
			item, err := step.Item()
			if err != nil {
				return err
			}
			items[i] = appendItem(frame, tagDerivationImageSequence, item)
		}
		obj.Put(derivedio.Seq(tagPerFrameFunctionalGroups, items...))
		return nil
	}
	item, err := d.Item()
	if err != nil {
		return err
	}
	obj.Put(derivedio.Seq(tagSharedFunctionalGroups, appendItem(sharedItems.Items[0], tagDerivationImageSequence, item)))
	return nil
}

// ApplyTo records d where obj's IOD keeps it: in the functional groups of a
// multi-frame functional group object, otherwise at the top level.
func ApplyTo(obj *object.Object, d Derivation) error {
	if obj != nil && obj.Has(tagSharedFunctionalGroups) {
		return ApplyFunctionalGroups(obj, d)
	}
	return Apply(obj, d)
}

// record returns the Derivation Description, Derivation Code Sequence and
// Source Image Sequence elements of existing updated with d.
func record(existing []core.Element, d Derivation) ([]core.Element, error) {
	var out []core.Element
	if description := strings.TrimSpace(d.Description); description != "" {
		if previous := elementText(existing, tagDerivationDescription); previous != "" {
			description = previous + "; " + description
		}
		out = append(out, derivedio.Str(tagDerivationDescription, core.VRST, truncate(description, maxDerivationDescriptionLength)))
	}
	if len(d.Codes) > 0 {
		codes, err := sequenceItems(existing, tagDerivationCodeSequence)
		if err != nil {
			return nil, err
		}
		added := false
		for _, c := range d.Codes {
			if err := c.validate(); err != nil {
				return nil, err
			}
			present := false
			for _, item := range codes {
				present = present || c.equal(item)
			}
			if !present {
				codes = append(codes, c.Item())
				added = true
			}
		}
		if added {
			out = append(out, derivedio.Seq(tagDerivationCodeSequence, codes...))
		}
	}
	if len(d.Sources) > 0 {
		sources, err := sequenceItems(existing, tagSourceImageSequence)
		if err != nil {
			return nil, err
		}
		items, err := d.sourceItems()
		if err != nil {
			return nil, err
		}
		out = append(out, derivedio.Seq(tagSourceImageSequence, append(sources, items...)...))
	}
	return out, nil
}

func sequenceItems(elements []core.Element, tag core.Tag) ([]core.DataSet, error) {
	for _, e := range elements {
		if e.Tag() != tag {
			continue
		}
		sequence, ok := e.Value.(core.SequenceValue)
		if !ok {
			return nil, fmt.Errorf("%w: %s is not a sequence", ErrInvalid, tag)
		}
		return append([]core.DataSet(nil), sequence.Items...), nil
	}
	return nil, nil
}

func appendItem(ds core.DataSet, tag core.Tag, item core.DataSet) core.DataSet {
	elements := make([]core.Element, 0, len(ds.Elements)+1)
	var items []core.DataSet
	for _, e := range ds.Elements {
		if e.Tag() == tag {
			if sequence, ok := e.Value.(core.SequenceValue); ok {
				items = append(items, sequence.Items...)
			}
			continue
		}
		elements = append(elements, e)
	}
	return derivedio.DataSet(append(elements, derivedio.Seq(tag, append(items, item)...))...)
}

func hasTag(elements []core.Element, tag core.Tag) bool {
	for _, e := range elements {
		if e.Tag() == tag {
			return true
		}
	}
	return false
}

func elementText(elements []core.Element, tag core.Tag) string {
	for _, e := range elements {
		if e.Tag() == tag {
			return strings.TrimSpace(strings.Join(e.StringValues(), "\\"))
		}
	}
	return ""
}

// truncate cuts s to at most n characters.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

// LossyDescription is the Derivation Description of a lossy compression step.
func LossyDescription(method string, ratio float64) string {
	return "Lossy compression " + strings.TrimSpace(method) + ", compression ratio " + strconv.FormatFloat(ratio, 'g', 4, 64)
}
