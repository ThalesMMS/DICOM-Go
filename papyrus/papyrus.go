// Package papyrus converts Papyrus 3.0 files into standard DICOM instances.
//
// A Papyrus 3.0 file is a Part 10 file whose images are not top-level Pixel
// Data: each image is a complete data set nested as an item of the private
// Image Sequence (0041,xx50) of the "PAPYRUS 3.0" block, while the top level
// carries the attributes the images share. Readers that do not know the
// private block see an empty image, so the conversion is explicit (opt-in)
// and reported: Convert returns one Part 10 instance per nested image.
package papyrus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/internal/iodmodules"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Creator is the Private Creator of the Papyrus 3.0 block in group 0041.
const Creator = "PAPYRUS 3.0"

// SecondaryCaptureImageStorageUID is the default SOP Class of images whose
// own class is not a current standard image storage class.
const SecondaryCaptureImageStorageUID = "1.2.840.10008.5.1.4.1.1.7"

const (
	papyrusGroup        = 0x0041
	offsetNumberImages  = 0x15
	offsetImageSequence = 0x50
	defaultMaxImages    = 10_000
)

var (
	// ErrNotPapyrus reports a file without a Papyrus 3.0 Image Sequence.
	ErrNotPapyrus = errors.New("papyrus: not a Papyrus 3.0 file")
	// ErrInvalidImage reports a nested image that cannot become an instance.
	ErrInvalidImage = errors.New("papyrus: invalid image")
	// ErrInvalidOptions reports unusable Options.
	ErrInvalidOptions = errors.New("papyrus: invalid options")
	// ErrResourceLimit reports a file beyond the configured limits.
	ErrResourceLimit = errors.New("papyrus: resource limit exceeded")
)

// Options configures Convert. The zero value is ready to use.
type Options struct {
	// FallbackSOPClassUID is the SOP Class of images whose own class is
	// missing, retired, not an image storage class or not in the standard;
	// empty selects Secondary Capture Image Storage.
	FallbackSOPClassUID string
	// MaxImages limits the nested images; zero selects 10000.
	MaxImages int
	// UNRecovery bounds the parse of an Image Sequence stored as UN, as
	// object.RecoverUN does; zero fields select its defaults.
	UNRecovery object.UNRecoveryOptions
}

// UIDChange records a UID Convert replaced or generated.
type UIDChange struct {
	Tag      core.Tag
	Old, New string
	// Reason is "missing", "invalid", "reserved" (a UID under the DICOM root
	// 1.2.840.10008 used as an instance UID) or "duplicate".
	Reason string
}

// ImageReport describes one converted image.
type ImageReport struct {
	// SOPClassUID is the class of the instance; SourceSOPClassUID is the
	// class the image declared when Convert replaced it.
	SOPClassUID, SourceSOPClassUID string
	SOPInstanceUID                 string
	// Inherited lists the top-level attributes copied into the image because
	// it lacked them.
	Inherited []core.Tag
	// Added lists attributes the target IOD requires that Convert set: an
	// empty value for each missing Type 2 attribute of its mandatory modules,
	// and Conversion Type WSD for Secondary Capture.
	Added []core.Tag
	// Removed lists the attributes dropped from the image: group lengths,
	// the Papyrus block and private attributes without a valid creator.
	Removed []core.Tag
	UIDs    []UIDChange
}

// Report describes a Convert call.
type Report struct {
	// DeclaredImages is the Papyrus Number of Images (0041,xx15), or -1 when
	// absent; a value other than len(Images) is also listed in Warnings.
	DeclaredImages int
	Images         []ImageReport
	// TransferSyntaxUID is the transfer syntax of the instances.
	TransferSyntaxUID string
	Warnings          []string
}

// IsPapyrus reports whether file carries a Papyrus 3.0 Image Sequence.
func IsPapyrus(file *object.File) bool {
	if file == nil || file.Dataset == nil {
		return false
	}
	tag, err := file.Dataset.PrivateTag(papyrusGroup, Creator, offsetImageSequence)
	return err == nil && file.Dataset.Has(tag)
}

// Convert returns one Part 10 instance per image of the Papyrus 3.0 file.
//
// Each instance is the nested image data set, completed with the top-level
// attributes it lacks (patient, study and series attributes Papyrus stores
// once). Group lengths, the Papyrus block and private attributes without a
// valid creator in the image are removed. Study, Series, Frame of Reference
// and SOP Instance UIDs that are missing, syntactically invalid or under the
// reserved DICOM root are replaced by generated UIDs (the same replacement
// for a value shared by several images; SOP Instance UIDs are also made
// unique). An image whose SOP Class is not a current standard image storage
// class becomes Options.FallbackSOPClassUID. Native pixel data is written
// Explicit VR Little Endian; encapsulated or big endian data keeps the file's
// transfer syntax. file is not modified, and every change is reported.
func Convert(ctx context.Context, file *object.File, opts Options) ([]*object.File, Report, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	fallback := strings.TrimSpace(opts.FallbackSOPClassUID)
	if fallback == "" {
		fallback = SecondaryCaptureImageStorageUID
	}
	if !isImageStorage(fallback) {
		return nil, Report{}, fmt.Errorf("%w: %q is not a current image storage SOP Class", ErrInvalidOptions, fallback)
	}
	if opts.MaxImages < 0 {
		return nil, Report{}, fmt.Errorf("%w: negative MaxImages", ErrInvalidOptions)
	}
	maxImages := opts.MaxImages
	if maxImages == 0 {
		maxImages = defaultMaxImages
	}
	if !IsPapyrus(file) {
		return nil, Report{}, ErrNotPapyrus
	}
	report := Report{DeclaredImages: -1}
	if tag, err := file.Dataset.PrivateTag(papyrusGroup, Creator, offsetNumberImages); err == nil {
		if n, err := declaredImages(file.Dataset, tag); err == nil {
			report.DeclaredImages = n
		}
	}
	items, err := imageItems(ctx, file, opts.UNRecovery)
	if err != nil {
		return nil, Report{}, err
	}
	if len(items) == 0 {
		return nil, Report{}, fmt.Errorf("%w: empty Image Sequence", ErrNotPapyrus)
	}
	if len(items) > maxImages {
		return nil, Report{}, fmt.Errorf("%w: %d images, limit %d", ErrResourceLimit, len(items), maxImages)
	}
	if report.DeclaredImages >= 0 && report.DeclaredImages != len(items) {
		report.Warnings = append(report.Warnings, fmt.Sprintf("Number of Images is %d, the Image Sequence has %d items", report.DeclaredImages, len(items)))
	}

	syntax := outputSyntax(file.TransferSyntax)
	report.TransferSyntaxUID = syntax.UID
	shared, err := sharedAttributes(file.Dataset)
	if err != nil {
		return nil, Report{}, err
	}
	fixer := uidFixer{replaced: map[core.Tag]map[string]string{}, generated: map[core.Tag]string{}, instances: map[string]bool{}}
	out := make([]*object.File, 0, len(items))
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return nil, Report{}, err
		}
		image, imageReport, err := convertImage(item, shared, fallback, &fixer)
		if err != nil {
			return nil, Report{}, fmt.Errorf("%w %d: %w", ErrInvalidImage, i+1, err)
		}
		image.SetValueByteOrder(syntax.ByteOrder)
		result := &object.File{Dataset: image, Meta: object.New(std.Dictionary), TransferSyntax: syntax}
		if err := result.RebuildFileMeta(); err != nil {
			return nil, Report{}, fmt.Errorf("%w %d: file meta: %w", ErrInvalidImage, i+1, err)
		}
		out = append(out, result)
		report.Images = append(report.Images, imageReport)
	}
	return out, report, nil
}

func declaredImages(dataset *object.Object, tag core.Tag) (int, error) {
	if n, err := dataset.GetInt(tag); err == nil {
		return int(n), nil
	}
	// A UN value is the little endian US of the Papyrus dictionary.
	raw, ok := dataset.GetRaw(tag)
	if !ok || len(raw) != 2 {
		return 0, ErrInvalidImage
	}
	return int(raw[0]) | int(raw[1])<<8, nil
}

// imageItems returns the nested images, parsing an Image Sequence that an
// Implicit VR reader left as UN with the Papyrus definition of the element.
func imageItems(ctx context.Context, file *object.File, limits object.UNRecoveryOptions) ([]core.DataSet, error) {
	tag, err := file.Dataset.PrivateTag(papyrusGroup, Creator, offsetImageSequence)
	if err != nil {
		return nil, ErrNotPapyrus
	}
	element, _ := file.Dataset.Get(tag)
	if sequence, ok := element.Value.(core.SequenceValue); ok {
		return sequence.Items, nil
	}
	if element.VR() != core.VRUN {
		return nil, fmt.Errorf("%w: Image Sequence has VR %s", ErrNotPapyrus, element.VR())
	}
	raw, ok := file.Dataset.GetRaw(tag)
	if !ok {
		return nil, fmt.Errorf("%w: unreadable Image Sequence", ErrNotPapyrus)
	}
	creator := core.Element{Header: core.ElementHeader{Tag: core.NewTag(papyrusGroup, tag.Element>>8), VR: core.VRLO}, Value: core.StringValue{Creator}}
	unknown := core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUN, Length: core.Length(len(raw)), LengthSet: true}, Value: core.RawValue(raw)}
	holder := object.FromElements([]core.Element{creator, unknown}, std.Dictionary)
	holder.SetValueByteOrder(file.Dataset.ValueByteOrder())
	limits.Dictionary = dictionary.Chain{catalog, std.Dictionary}
	// Not Strict: private attributes nested in the images stay UN (the
	// conversion drops those without a creator); only the sequence itself
	// must be recovered.
	limits.Strict = false
	recovered, unReport, err := holder.RecoverUN(ctx, limits)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: Image Sequence: %w", ErrNotPapyrus, err)
	}
	sequence, ok := recovered.GetSequence(tag)
	if !ok {
		reason := object.UNReason("not a sequence")
		for _, d := range unReport.Diagnostics {
			if d.Path == tag.String() {
				reason = d.Reason
			}
		}
		if reason == object.UNLimitExceeded {
			return nil, fmt.Errorf("%w: Image Sequence: %s", ErrResourceLimit, reason)
		}
		return nil, fmt.Errorf("%w: Image Sequence: %s", ErrNotPapyrus, reason)
	}
	items := make([]core.DataSet, len(sequence))
	for i, item := range sequence {
		items[i] = item.ToDataSet()
	}
	return items, nil
}

// catalog defines the Papyrus elements Convert reads.
var catalog = func() *dictionary.PrivateCatalog {
	c, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{
		{Creator: Creator, Group: papyrusGroup, Offset: 0x10, VR: core.VRSQ, Keyword: "PapyrusPointerSequence", Name: "Pointer Sequence", VM: "1"},
		{Creator: Creator, Group: papyrusGroup, Offset: offsetNumberImages, VR: core.VRUS, Keyword: "PapyrusNumberOfImages", Name: "Number of Images", VM: "1"},
		{Creator: Creator, Group: papyrusGroup, Offset: offsetImageSequence, VR: core.VRSQ, Keyword: "PapyrusImageSequence", Name: "Image Sequence", VM: "1"},
	})
	if err != nil {
		panic(err)
	}
	return c
}()

func outputSyntax(source transfer.Syntax) transfer.Syntax {
	if source.Encapsulated || !source.IsLittleEndian() {
		return source
	}
	return transfer.ExplicitVRLittleEndian
}

// sharedAttributes returns the top-level attributes images may inherit.
func sharedAttributes(dataset *object.Object) ([]core.Element, error) {
	var shared []core.Element
	for _, element := range dataset.Elements() {
		tag := element.Tag()
		if tag.Group == 0x0002 || tag.Element == 0 || dictionary.IsPrivateGroup(tag.Group) || tag == tagPixelData {
			continue
		}
		if _, ok := element.Value.(core.RawValue); ok || element.Value == nil {
			// A deferred or unparsed value is read now, so instances never
			// depend on the source staying open.
			raw, ok := dataset.GetRaw(tag)
			if !ok && element.Length() > 0 {
				return nil, fmt.Errorf("%w: unreadable %s", ErrInvalidImage, tag)
			}
			element.Value = core.RawValue(raw)
		}
		shared = append(shared, element)
	}
	return shared, nil
}

var (
	tagSOPClassUID         = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID      = core.NewTag(0x0008, 0x0018)
	tagConversionType      = core.NewTag(0x0008, 0x0064)
	tagStudyInstanceUID    = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID   = core.NewTag(0x0020, 0x000E)
	tagFrameOfReferenceUID = core.NewTag(0x0020, 0x0052)
	tagPixelData           = core.NewTag(0x7FE0, 0x0010)
)

func convertImage(item core.DataSet, shared []core.Element, fallback string, fixer *uidFixer) (*object.Object, ImageReport, error) {
	var report ImageReport
	reservations, _, err := dictionary.PrivateReservationsFromElements(item.Elements, dictionary.DefaultMaxPrivateCreators)
	if err != nil {
		return nil, report, err
	}
	used := map[core.Tag]bool{} // creator elements that reserve a kept block
	var kept []core.Element
	for _, element := range item.Elements {
		tag := element.Tag()
		switch {
		case tag.Element == 0 || tag.Group == 0x0002:
			report.Removed = append(report.Removed, tag)
			continue
		case dictionary.IsPrivateDataTag(tag):
			creator, err := reservations.Creator(tag)
			if err != nil || creator == Creator {
				report.Removed = append(report.Removed, tag)
				continue
			}
			used[core.NewTag(tag.Group, tag.Element>>8)] = true
		}
		kept = append(kept, element)
	}
	image := object.New(std.Dictionary)
	for _, element := range kept {
		tag := element.Tag()
		if dictionary.IsPrivateGroup(tag.Group) && !dictionary.IsPrivateDataTag(tag) && !used[tag] {
			// A creator (or an invalid private element) without kept data.
			report.Removed = append(report.Removed, tag)
			continue
		}
		element.Value = cloneValue(element.Value)
		image.Put(element)
	}
	if !image.Has(tagPixelData) {
		return nil, report, errors.New("no Pixel Data")
	}
	for _, element := range shared {
		if image.Has(element.Tag()) {
			continue
		}
		element.Value = cloneValue(element.Value)
		image.Put(element)
		report.Inherited = append(report.Inherited, element.Tag())
	}

	declared, _ := image.GetUID(tagSOPClassUID)
	declared = uid.NormalizeUID(declared)
	report.SOPClassUID = declared
	if !isImageStorage(declared) {
		report.SOPClassUID, report.SourceSOPClassUID = fallback, declared
		image.Put(uiElement(tagSOPClassUID, fallback))
	}
	if report.SOPClassUID == SecondaryCaptureImageStorageUID && !image.Has(tagConversionType) {
		image.Put(core.Element{Header: core.ElementHeader{Tag: tagConversionType, VR: core.VRCS}, Value: core.StringValue{"WSD"}})
		report.Added = append(report.Added, tagConversionType)
	}
	report.Added = append(report.Added, emitType2(image, report.SOPClassUID)...)
	for _, tag := range []core.Tag{tagStudyInstanceUID, tagSeriesInstanceUID, tagFrameOfReferenceUID, tagSOPInstanceUID} {
		change, err := fixer.fix(image, tag)
		if err != nil {
			return nil, report, err
		}
		if change.Reason != "" {
			report.UIDs = append(report.UIDs, change)
		}
	}
	report.SOPInstanceUID, _ = image.GetUID(tagSOPInstanceUID)
	return image, report, nil
}

// emitType2 adds an empty value for each missing Type 2 attribute of the
// mandatory modules of the IOD of class and returns their tags.
func emitType2(image *object.Object, class string) []core.Tag {
	entry, _ := uid.Dictionary.ByUID(class)
	modules, _ := iodmodules.Modules(strings.TrimSuffix(entry.Name, " Storage"))
	var added []core.Tag
	for _, use := range modules {
		if use.Usage != "M" {
			continue
		}
		attributes, _ := iodmodules.Attributes(use.Module)
		for _, attribute := range attributes {
			tag := core.NewTag(uint16(attribute.Tag>>16), uint16(attribute.Tag))
			if attribute.Type != "2" || image.Has(tag) {
				continue
			}
			if vr := dictionary.LookupVR(std.Dictionary, tag); vr == core.VRSQ {
				image.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true}, Value: core.SequenceValue{}})
			} else {
				image.Put(core.NewRawElement(tag, vr, nil))
			}
			added = append(added, tag)
		}
	}
	return added
}

// isImageStorage reports whether class is a current standard image storage
// SOP Class.
func isImageStorage(class string) bool {
	entry, ok := uid.Dictionary.ByUID(class)
	return ok && entry.Type == uid.SOPClass && !entry.Retired && strings.HasSuffix(entry.Name, "Image Storage")
}

// uidFixer replaces unusable UIDs consistently across the images of a file.
type uidFixer struct {
	replaced  map[core.Tag]map[string]string // per tag, old value to new
	generated map[core.Tag]string            // per tag, the UID of images without one
	instances map[string]bool                // SOP Instance UIDs already used
}

func (f *uidFixer) fix(image *object.Object, tag core.Tag) (UIDChange, error) {
	value, ok := image.GetUID(tag)
	value = uid.NormalizeUID(value)
	change := UIDChange{Tag: tag, Old: value}
	switch {
	case !ok || value == "":
		if tag == tagFrameOfReferenceUID {
			return UIDChange{}, nil // optional for the images Papyrus holds
		}
		change.Reason = "missing"
	case !core.IsValidUID(value):
		change.Reason = "invalid"
	case strings.HasPrefix(value, "1.2.840.10008."):
		change.Reason = "reserved"
	case tag == tagSOPInstanceUID && f.instances[value]:
		change.Reason = "duplicate"
	}
	if change.Reason == "" {
		if tag == tagSOPInstanceUID {
			f.instances[value] = true
		}
		return UIDChange{}, nil
	}
	var err error
	switch {
	case tag == tagSOPInstanceUID:
		change.New, err = newUID()
		f.instances[change.New] = true
	case change.Reason == "missing":
		if change.New = f.generated[tag]; change.New == "" {
			change.New, err = newUID()
			f.generated[tag] = change.New
		}
	default:
		if f.replaced[tag] == nil {
			f.replaced[tag] = map[string]string{}
		}
		if change.New = f.replaced[tag][value]; change.New == "" {
			change.New, err = newUID()
			f.replaced[tag][value] = change.New
		}
	}
	if err != nil {
		return UIDChange{}, err
	}
	image.Put(uiElement(tag, change.New))
	return change, nil
}

func newUID() (string, error) {
	return core.NewUID()
}

func uiElement(tag core.Tag, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue{value}}
}

// cloneValue returns a copy of value that shares no mutable buffer with it.
func cloneValue(value core.Value) core.Value {
	switch v := value.(type) {
	case core.RawValue:
		return core.RawValue(core.CloneBytes(v))
	case core.StringValue:
		return append(core.StringValue(nil), v...)
	case core.Uint16Value:
		return append(core.Uint16Value(nil), v...)
	case core.Int16Value:
		return append(core.Int16Value(nil), v...)
	case core.Uint32Value:
		return append(core.Uint32Value(nil), v...)
	case core.Int32Value:
		return append(core.Int32Value(nil), v...)
	case core.Uint64Value:
		return append(core.Uint64Value(nil), v...)
	case core.Int64Value:
		return append(core.Int64Value(nil), v...)
	case core.Float32Value:
		return append(core.Float32Value(nil), v...)
	case core.Float64Value:
		return append(core.Float64Value(nil), v...)
	case core.TagValue:
		return append(core.TagValue(nil), v...)
	case core.SequenceValue:
		items := make([]core.DataSet, len(v.Items))
		for i, item := range v.Items {
			elements := make([]core.Element, len(item.Elements))
			for j, e := range item.Elements {
				e.Value = cloneValue(e.Value)
				elements[j] = e
			}
			items[i] = core.DataSet{Elements: elements}
		}
		out := v
		out.Items = items
		return out
	default:
		return value
	}
}
