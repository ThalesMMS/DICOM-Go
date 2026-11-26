package spatialreg

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// Tags the writer adds to those the readers use.
var (
	tagSpecificCharacterSet          = core.NewTag(0x0008, 0x0005)
	tagContentDate                   = core.NewTag(0x0008, 0x0023)
	tagContentTime                   = core.NewTag(0x0008, 0x0033)
	tagSeriesDescription             = core.NewTag(0x0008, 0x103E)
	tagReferencedSOPClassUID         = core.NewTag(0x0008, 0x1150)
	tagReferencedSeriesSequence      = core.NewTag(0x0008, 0x1115)
	tagReferencedInstanceSequence    = core.NewTag(0x0008, 0x114A)
	tagInstanceNumber                = core.NewTag(0x0020, 0x0013)
	tagContentLabel                  = core.NewTag(0x0070, 0x0080)
	tagContentDescription            = core.NewTag(0x0070, 0x0081)
	tagContentCreatorsName           = core.NewTag(0x0070, 0x0084)
	tagRegistrationTypeCodeSequence  = core.NewTag(0x0070, 0x030D)
	tagFrameTransformationComment    = core.NewTag(0x3006, 0x00C8)
	tagStudiesContainingOtherRefSeq  = core.NewTag(0x0008, 0x1200)
	tagReferencedStudyInstanceUIDTag = core.NewTag(0x0020, 0x000D)
)

// ErrWrite reports options that cannot make a valid Spatial Registration.
var ErrWrite = errors.New("spatialreg: invalid Spatial Registration")

// ImageReference identifies one image a registration was computed from.
type ImageReference struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPClassUID       string
	SOPInstanceUID    string
}

// MatrixRegistration is one Registration Sequence item: the matrix mapping
// the source Frame of Reference into the registered one (PS3.3 C.20.2).
type MatrixRegistration struct {
	SourceFrameOfReferenceUID string
	// Matrix maps source to registered patient coordinates; its Type is
	// RIGID, RIGID_SCALE or AFFINE and is checked with ValidateMatrix.
	Matrix Matrix
	// Comment is Frame of Reference Transformation Comment, for example
	// how the matrix was computed and its fiducial registration error.
	Comment string
	// ReferencedImages optionally lists the source images used.
	ReferencedImages []ImageReference
}

// WriteOptions describe a new Spatial Registration Storage instance.
type WriteOptions struct {
	// Common supplies the patient, study, Frame of Reference and equipment:
	// Source is normally an image of the registered (fixed) series.
	Common            object.DerivedCommon
	SOPInstanceUID    string // generated when empty
	SeriesInstanceUID string // generated when empty
	SeriesNumber      int
	SeriesDescription string
	InstanceNumber    int // 1 when zero
	// ContentLabel is a CS (upper case, digits, space, underscore); when
	// empty "REGISTRATION".
	ContentLabel       string
	ContentDescription string
	ContentCreatorName string
	// RegisteredFrameOfReferenceUID is the Frame of Reference the matrices
	// map into; when empty, the Common source's.
	RegisteredFrameOfReferenceUID string
	Registrations                 []MatrixRegistration
	// ContentTime is Content Date and Time; zero uses the current time.
	ContentTime time.Time
}

// Write builds a Spatial Registration Storage file (PS3.3 A.39). The first
// Registration Sequence item states the registered Frame of Reference with
// an identity matrix, as PS3.3 C.20.2 allows, followed by one item per
// source; images named in ReferencedImages are listed in the Common Instance
// Reference module. ReadAffine reads the result back.
func Write(opts WriteOptions) (*object.File, error) {
	if len(opts.Registrations) == 0 {
		return nil, fmt.Errorf("%w: no registration", ErrWrite)
	}
	registered := strings.TrimSpace(opts.RegisteredFrameOfReferenceUID)
	if registered == "" && opts.Common.Source != nil {
		registered = derivedio.CleanUID(opts.Common.Source, derivedio.TagFrameOfReferenceUID)
	}
	if !core.IsValidUID(registered) {
		return nil, fmt.Errorf("%w: registered Frame of Reference UID %q", ErrWrite, registered)
	}
	for i, r := range opts.Registrations {
		if !core.IsValidUID(r.SourceFrameOfReferenceUID) {
			return nil, fmt.Errorf("%w: registration %d: source Frame of Reference UID %q", ErrWrite, i+1, r.SourceFrameOfReferenceUID)
		}
		if r.SourceFrameOfReferenceUID == registered {
			return nil, fmt.Errorf("%w: registration %d maps the registered Frame of Reference onto itself", ErrWrite, i+1)
		}
		if err := ValidateMatrix(r.Matrix.Values, TransformType(r.Matrix.Type)); err != nil {
			return nil, fmt.Errorf("%w: registration %d: %w", ErrWrite, i+1, err)
		}
		for _, ref := range r.ReferencedImages {
			if !core.IsValidUID(ref.SOPClassUID) || !core.IsValidUID(ref.SOPInstanceUID) || !core.IsValidUID(ref.SeriesInstanceUID) {
				return nil, fmt.Errorf("%w: registration %d: incomplete image reference", ErrWrite, i+1)
			}
		}
	}
	label := strings.TrimSpace(opts.ContentLabel)
	if label == "" {
		label = "REGISTRATION"
	}
	if len(label) > 16 || strings.IndexFunc(label, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '_')
	}) >= 0 {
		return nil, fmt.Errorf("%w: Content Label %q is not a CS value", ErrWrite, label)
	}
	sopInstanceUID, err := uidOrNew(opts.SOPInstanceUID)
	if err != nil {
		return nil, err
	}
	seriesInstanceUID, err := uidOrNew(opts.SeriesInstanceUID)
	if err != nil {
		return nil, err
	}
	now := opts.ContentTime
	if now.IsZero() {
		now = time.Now()
	}
	instance := opts.InstanceNumber
	if instance == 0 {
		instance = 1
	}
	seriesNumber := derivedio.IS(derivedio.TagSeriesNumber)
	if opts.SeriesNumber > 0 {
		seriesNumber = derivedio.IS(derivedio.TagSeriesNumber, opts.SeriesNumber)
	}
	items := []core.DataSet{registrationItem(registered, Matrix{Type: string(TransformRigid), Values: identityMatrix().Values}, "Registered Frame of Reference", nil)}
	for _, r := range opts.Registrations {
		items = append(items, registrationItem(r.SourceFrameOfReferenceUID, r.Matrix, r.Comment, r.ReferencedImages))
	}
	dataset := derivedio.Object(
		derivedio.CS(tagSpecificCharacterSet, "ISO_IR 192"),
		derivedio.UI(derivedio.TagSOPClassUID, SpatialRegistrationStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, sopInstanceUID),
		derivedio.CS(derivedio.TagModality, "REG"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, seriesInstanceUID),
		seriesNumber,
		derivedio.IS(tagInstanceNumber, instance),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, registered),
		derivedio.Str(tagContentDate, core.VRDA, now.Format("20060102")),
		derivedio.Str(tagContentTime, core.VRTM, now.Format("150405")),
		derivedio.CS(tagContentLabel, label),
		derivedio.LO(tagContentDescription, opts.ContentDescription),
		derivedio.Str(tagContentCreatorsName, core.VRPN, opts.ContentCreatorName),
		derivedio.Seq(tagRegistrationSequence, items...),
	)
	if description := strings.TrimSpace(opts.SeriesDescription); description != "" {
		dataset.Put(derivedio.LO(tagSeriesDescription, description))
	}
	if err := opts.Common.Apply(dataset, object.InheritAll); err != nil {
		return nil, err
	}
	putCommonInstanceReferences(dataset, opts.Registrations)
	return derivedio.File(SpatialRegistrationStorage, sopInstanceUID, dataset)
}

func uidOrNew(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return core.NewUID()
	}
	if !core.IsValidUID(value) {
		return "", fmt.Errorf("%w: UID %q", ErrWrite, value)
	}
	return value, nil
}

func registrationItem(frame string, matrix Matrix, comment string, refs []ImageReference) core.DataSet {
	matrixItem := derivedio.DataSet(
		derivedio.DS(tagFrameTransformationMatrix, matrix.Values[:]...),
		derivedio.CS(tagFrameTransformationMatrixType, matrix.Type),
	)
	registration := []core.Element{
		derivedio.Seq(tagRegistrationTypeCodeSequence),
		derivedio.Seq(tagMatrixSequence, matrixItem),
	}
	if comment = strings.TrimSpace(comment); comment != "" {
		registration = append(registration, derivedio.LO(tagFrameTransformationComment, comment))
	}
	elements := []core.Element{
		derivedio.UI(derivedio.TagFrameOfReferenceUID, frame),
		derivedio.Seq(tagMatrixRegistrationSequence, derivedio.DataSet(registration...)),
	}
	if len(refs) > 0 {
		var images []core.DataSet
		for _, ref := range refs {
			images = append(images, derivedio.DataSet(
				derivedio.UI(tagReferencedSOPClassUID, ref.SOPClassUID),
				derivedio.UI(tagReferencedSOPInstanceUID, ref.SOPInstanceUID),
			))
		}
		elements = append(elements, derivedio.Seq(tagReferencedImageSequence, images...))
	}
	return derivedio.DataSet(elements...)
}

// putCommonInstanceReferences lists the referenced images by series (PS3.3
// C.12.2): those of the registration's own study in Referenced Series
// Sequence, the others under Studies Containing Other Referenced Instances.
func putCommonInstanceReferences(dataset *object.Object, registrations []MatrixRegistration) {
	study := derivedio.CleanUID(dataset, derivedio.TagStudyInstanceUID)
	type seriesKey struct{ study, series string }
	var order []seriesKey
	instances := map[seriesKey][]core.DataSet{}
	seen := map[string]bool{}
	for _, r := range registrations {
		for _, ref := range r.ReferencedImages {
			if seen[ref.SOPInstanceUID] {
				continue
			}
			seen[ref.SOPInstanceUID] = true
			studyUID := ref.StudyInstanceUID
			if studyUID == "" {
				studyUID = study
			}
			key := seriesKey{studyUID, ref.SeriesInstanceUID}
			if _, ok := instances[key]; !ok {
				order = append(order, key)
			}
			instances[key] = append(instances[key], derivedio.DataSet(
				derivedio.UI(tagReferencedSOPClassUID, ref.SOPClassUID),
				derivedio.UI(tagReferencedSOPInstanceUID, ref.SOPInstanceUID),
			))
		}
	}
	var local []core.DataSet
	other := map[string][]core.DataSet{}
	var otherOrder []string
	for _, key := range order {
		item := derivedio.DataSet(
			derivedio.UI(derivedio.TagSeriesInstanceUID, key.series),
			derivedio.Seq(tagReferencedInstanceSequence, instances[key]...),
		)
		if key.study == study {
			local = append(local, item)
			continue
		}
		if _, ok := other[key.study]; !ok {
			otherOrder = append(otherOrder, key.study)
		}
		other[key.study] = append(other[key.study], item)
	}
	if len(local) > 0 {
		dataset.Put(derivedio.Seq(tagReferencedSeriesSequence, local...))
	}
	if len(otherOrder) > 0 {
		var studies []core.DataSet
		for _, uid := range otherOrder {
			studies = append(studies, derivedio.DataSet(
				derivedio.UI(tagReferencedStudyInstanceUIDTag, uid),
				derivedio.Seq(tagReferencedSeriesSequence, other[uid]...),
			))
		}
		dataset.Put(derivedio.Seq(tagStudiesContainingOtherRefSeq, studies...))
	}
}
