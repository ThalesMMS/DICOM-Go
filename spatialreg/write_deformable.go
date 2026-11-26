package spatialreg

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// DeformableRegistration is one Deformable Registration Sequence item
// (PS3.3 C.20.3): registered point x maps to the source Frame of Reference
// at Post(Pre(x) + D(x)), D the grid's displacement at x.
type DeformableRegistration struct {
	SourceFrameOfReferenceUID string
	// Pre and Post are optional homogeneous matrices (the zero value is
	// omitted); their Type is RIGID, RIGID_SCALE or AFFINE.
	Pre, Post Matrix
	// Grid holds the displacements, placed in the registered Frame of
	// Reference; nil writes a matrix-only item.
	Grid *Grid
	// Comment is Frame of Reference Transformation Comment, for example how
	// the field was computed.
	Comment string
	// ReferencedImages optionally lists the source images used.
	ReferencedImages []ImageReference
}

// DeformableWriteOptions describe a new Deformable Spatial Registration
// Storage instance; the fields shared with WriteOptions mean the same.
type DeformableWriteOptions struct {
	Common                        object.DerivedCommon
	SOPInstanceUID                string
	SeriesInstanceUID             string
	SeriesNumber                  int
	SeriesDescription             string
	InstanceNumber                int
	ContentLabel                  string
	ContentDescription            string
	ContentCreatorName            string
	RegisteredFrameOfReferenceUID string
	Registrations                 []DeformableRegistration
	ContentTime                   time.Time
}

// WriteDeformable builds a Deformable Spatial Registration Storage file
// (PS3.3 A.39.3, SOP Class 1.2.840.10008.5.1.4.1.1.66.3): the Spatial
// Registration Series, Frame of Reference and Deformable Spatial
// Registration modules, the Vector Grid Data as Little Endian OF. Read
// reads the result back.
func WriteDeformable(opts DeformableWriteOptions) (*object.File, error) {
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
	var items []core.DataSet
	var references []MatrixRegistration
	for i, r := range opts.Registrations {
		if !core.IsValidUID(r.SourceFrameOfReferenceUID) || r.SourceFrameOfReferenceUID == registered {
			return nil, fmt.Errorf("%w: deformable registration %d: source Frame of Reference UID %q", ErrWrite, i+1, r.SourceFrameOfReferenceUID)
		}
		if r.Grid == nil && r.Pre.Type == "" && r.Post.Type == "" {
			return nil, fmt.Errorf("%w: deformable registration %d has neither a grid nor a matrix", ErrWrite, i+1)
		}
		item, err := deformableItem(r)
		if err != nil {
			return nil, fmt.Errorf("%w: deformable registration %d: %w", ErrWrite, i+1, err)
		}
		items = append(items, item)
		references = append(references, MatrixRegistration{ReferencedImages: r.ReferencedImages})
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
	instance := max(opts.InstanceNumber, 1)
	seriesNumber := derivedio.IS(derivedio.TagSeriesNumber)
	if opts.SeriesNumber > 0 {
		seriesNumber = derivedio.IS(derivedio.TagSeriesNumber, opts.SeriesNumber)
	}
	dataset := derivedio.Object(
		derivedio.CS(tagSpecificCharacterSet, "ISO_IR 192"),
		derivedio.UI(derivedio.TagSOPClassUID, DeformableSpatialRegistrationStorage),
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
		derivedio.Seq(tagDeformableRegistrationSequence, items...),
	)
	if description := strings.TrimSpace(opts.SeriesDescription); description != "" {
		dataset.Put(derivedio.LO(tagSeriesDescription, description))
	}
	if err := opts.Common.Apply(dataset, object.InheritAll); err != nil {
		return nil, err
	}
	putCommonInstanceReferences(dataset, references)
	return derivedio.File(DeformableSpatialRegistrationStorage, sopInstanceUID, dataset)
}

func deformableItem(r DeformableRegistration) (core.DataSet, error) {
	elements := []core.Element{
		derivedio.UI(tagSourceFrameOfReferenceUID, r.SourceFrameOfReferenceUID),
		derivedio.Seq(tagRegistrationTypeCodeSequence),
	}
	for _, m := range []struct {
		tag    core.Tag
		matrix Matrix
	}{{tagPreDeformationMatrixSequence, r.Pre}, {tagPostDeformationMatrixSequence, r.Post}} {
		if m.matrix.Type == "" {
			continue
		}
		if err := ValidateMatrix(m.matrix.Values, TransformType(m.matrix.Type)); err != nil {
			return core.DataSet{}, err
		}
		elements = append(elements, derivedio.Seq(m.tag, derivedio.DataSet(
			derivedio.DS(tagFrameTransformationMatrix, m.matrix.Values[:]...),
			derivedio.CS(tagFrameTransformationMatrixType, m.matrix.Type),
		)))
	}
	if r.Grid != nil {
		grid, err := gridItem(r.Grid)
		if err != nil {
			return core.DataSet{}, err
		}
		elements = append(elements, derivedio.Seq(tagDeformableRegistrationGridSequence, grid))
	}
	if comment := strings.TrimSpace(r.Comment); comment != "" {
		elements = append(elements, derivedio.LO(tagFrameTransformationComment, comment))
	}
	if len(r.ReferencedImages) > 0 {
		var images []core.DataSet
		for _, ref := range r.ReferencedImages {
			if !core.IsValidUID(ref.SOPClassUID) || !core.IsValidUID(ref.SOPInstanceUID) || !core.IsValidUID(ref.SeriesInstanceUID) {
				return core.DataSet{}, fmt.Errorf("incomplete image reference")
			}
			images = append(images, derivedio.DataSet(
				derivedio.UI(tagReferencedSOPClassUID, ref.SOPClassUID),
				derivedio.UI(tagReferencedSOPInstanceUID, ref.SOPInstanceUID),
			))
		}
		elements = append(elements, derivedio.Seq(tagReferencedImageSequence, images...))
	}
	return derivedio.DataSet(elements...), nil
}

func gridItem(g *Grid) (core.DataSet, error) {
	count, ok := checkedProduct64(uint64(g.Dimensions[0]), uint64(g.Dimensions[1]), uint64(g.Dimensions[2]))
	if !ok || count != uint64(len(g.vectors)) || !validBasis(g.RowDirection, g.ColumnDirection, g.NormalDirection) ||
		g.RowDirection.Cross(g.ColumnDirection).Dot(g.NormalDirection) < 0 {
		return core.DataSet{}, fmt.Errorf("invalid deformation grid")
	}
	data := make([]byte, 12*len(g.vectors))
	for i, v := range g.vectors {
		binary.LittleEndian.PutUint32(data[12*i:], math.Float32bits(v.X))
		binary.LittleEndian.PutUint32(data[12*i+4:], math.Float32bits(v.Y))
		binary.LittleEndian.PutUint32(data[12*i+8:], math.Float32bits(v.Z))
	}
	return derivedio.DataSet(
		derivedio.DS(tagImagePositionPatient, g.Origin.X, g.Origin.Y, g.Origin.Z),
		derivedio.DS(tagImageOrientationPatient, g.RowDirection.X, g.RowDirection.Y, g.RowDirection.Z, g.ColumnDirection.X, g.ColumnDirection.Y, g.ColumnDirection.Z),
		derivedio.UL(tagGridDimensions, g.Dimensions[0], g.Dimensions[1], g.Dimensions[2]),
		derivedio.FD(tagGridResolution, g.Spacing.X, g.Spacing.Y, g.Spacing.Z),
		derivedio.Raw(tagVectorGridData, core.VROF, data),
	), nil
}
