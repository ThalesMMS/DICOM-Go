package sr

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/object"
)

var ErrImageLibrary = errors.New("dicom/sr: image library rejected")

// ImageLibraryError contains no descriptor value or source-instance identity.
type ImageLibraryError struct{ Code, Path string }

func (e *ImageLibraryError) Error() string {
	if e == nil {
		return ErrImageLibrary.Error()
	}
	s := ErrImageLibrary.Error() + ": " + e.Code
	if validNumericPath(e.Path) {
		s += " at " + e.Path
	}
	return s
}
func (e *ImageLibraryError) Unwrap() error { return ErrImageLibrary }
func (e *ImageLibraryError) Is(target error) bool {
	return target == ErrResourceLimitExceeded && e != nil && e.Code == "resource-limit"
}
func imageLibraryError(code, path string) error { return &ImageLibraryError{code, path} }

// ImageLibraryEntry keeps its image reference and acquisition context together.
// Descriptors use the existing typed ContentItem values, including nested view
// modifiers and extensible, unrecognized content. Unknown descriptors are never
// silently removed. Template/code/numeric encoding metadata use the binding.
type ImageLibraryEntry struct {
	Image       ImageReference
	Descriptors []ContentItem
	Header      ContentItem
}
type ImageLibraryGroup struct {
	// Order uses negative descriptor ordinals and positive entry ordinals; nil
	// selects descriptors before entries. Read preserves non-significant order.
	Order       []int
	Descriptors []ContentItem
	Entries     []ImageLibraryEntry
	Header      ContentItem
}

// ImageLibraryOptions are finite even when fields are zero. Context cancellation
// is checked during traversal, extraction and promotion, not in background I/O.
type ImageLibraryOptions struct {
	MaxItems, MaxDepth int
	MaxValueBytes      int64
}

func DefaultImageLibraryOptions() ImageLibraryOptions {
	return ImageLibraryOptions{10000, 32, 16 << 20}
}
func normalizeImageLibraryOptions(o ImageLibraryOptions) (ImageLibraryOptions, error) {
	d := DefaultImageLibraryOptions()
	if o.MaxItems == 0 {
		o.MaxItems = d.MaxItems
	}
	if o.MaxDepth == 0 {
		o.MaxDepth = d.MaxDepth
	}
	if o.MaxValueBytes == 0 {
		o.MaxValueBytes = d.MaxValueBytes
	}
	if o.MaxItems < 1 || o.MaxItems > 1000000 || o.MaxDepth < 1 || o.MaxDepth > 64 || o.MaxValueBytes < 1 {
		return o, imageLibraryError("resource-limit", "")
	}
	return o, nil
}

// ImageLibraryMetadataBinding privately snapshots the original library subtree.
// Relative metadata paths start at the library's virtual root 1. Rebinding after
// an authorized edit is explicit; stale associations never silently survive.
type ImageLibraryMetadataBinding struct {
	order    []int
	document *Document
	snapshot *TemplateMetadataSnapshot
	digest   [sha256.Size]byte
	options  ImageLibraryOptions
}

func (b *ImageLibraryMetadataBinding) Metadata() TemplateValidationMetadata {
	if b == nil {
		return TemplateValidationMetadata{}
	}
	return b.snapshot.Metadata()
}
func (b *ImageLibraryMetadataBinding) NumericMetadata() NumericRepresentationMetadata {
	if b == nil {
		return NumericRepresentationMetadata{}
	}
	return cloneNumericMetadata(b.snapshot.numeric)
}

func libraryDocument(descriptors []ContentItem, groups []ImageLibraryGroup) *Document {
	items := append([]ContentItem(nil), descriptors...)
	for _, g := range groups {
		children := append([]ContentItem(nil), g.Descriptors...)
		for _, e := range g.Entries {
			item := e.Header
			item.ValueType = ValueImage
			item.RelationshipType = RelationshipContains
			item.Image = e.Image
			item.Children = e.Descriptors
			children = append(children, item)
		}
		children = libraryOrderedItems(children, len(g.Descriptors), g.Order)
		item := g.Header
		item.ValueType = ValueContainer
		item.RelationshipType = RelationshipContains
		item.ConceptName = imageLibraryGroupConcept
		if item.ContinuityOfContent == "" {
			item.ContinuityOfContent = "SEPARATE"
		}
		item.Children = children
		items = append(items, item)
	}
	return &Document{SOPClassUID: Comprehensive3DSRStorage, Title: imageLibraryConcept, Content: items}
}
func boundedLibraryDocument(descriptors []ContentItem, groups []ImageLibraryGroup, o ImageLibraryOptions) (*Document, error) {
	remaining := o.MaxItems
	reserve := func(n int) bool {
		if n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	if !reserve(len(descriptors)) || !reserve(len(groups)) {
		return nil, imageLibraryError("resource-limit", "")
	}
	for _, g := range groups {
		if !reserve(len(g.Descriptors)) || !reserve(len(g.Entries)) {
			return nil, imageLibraryError("resource-limit", "")
		}
		for _, e := range g.Entries {
			if !reserve(len(e.Descriptors)) {
				return nil, imageLibraryError("resource-limit", "")
			}
		}
	}
	for _, g := range groups {
		if err := validLibraryOrder(g.Order, len(g.Descriptors), len(g.Entries)); err != nil {
			return nil, err
		}
	}
	return libraryDocument(descriptors, groups), nil
}
func libraryDigest(doc *Document, o ImageLibraryOptions) ([sha256.Size]byte, error) {
	budget := &snapshotHashBudget{remaining: o.MaxValueBytes}
	d, _, err := snapshotDocumentProof(doc, newEmptyLibraryMetadataIndex(), MetadataSnapshotOptions{MaxDepth: o.MaxDepth, MaxItems: o.MaxItems, MaxValueBytes: o.MaxValueBytes}, false, budget)
	return d, err
}
func newEmptyLibraryMetadataIndex() *templateMetadataIndex {
	x, _ := newTemplateMetadataIndex(TemplateValidationMetadata{})
	return x
}

// BindImageLibraryMetadata permits an explicit caller-authorized new association
// after editing. The caller supplies relative paths and numeric/code facts for
// the edited tree. No old paths are inferred, renumbered, or reused implicitly.
func BindImageLibraryMetadata(descriptors []ContentItem, groups []ImageLibraryGroup, metadata TemplateValidationMetadata, numeric NumericRepresentationMetadata, options ImageLibraryOptions) (*ImageLibraryMetadataBinding, error) {
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return nil, err
	}
	doc, err := boundedLibraryDocument(descriptors, groups, o)
	if err != nil {
		return nil, err
	}
	d, err := libraryDigest(doc, o)
	if err != nil {
		return nil, err
	}
	snapshot, err := BindNumericRepresentations(doc, metadata, numeric, MetadataSnapshotOptions{MaxDepth: o.MaxDepth, MaxItems: o.MaxItems, MaxValueBytes: o.MaxValueBytes})
	if err != nil {
		return nil, err
	}
	return &ImageLibraryMetadataBinding{document: doc, snapshot: snapshot, digest: d, options: o}, nil
}

func imageLibraryDataSets(report *MeasurementReport) ([]core.DataSet, error) {
	if report.ImageLibraryGroups == nil && report.ImageLibraryDescriptors == nil {
		return nil, nil
	}
	o := DefaultImageLibraryOptions()
	if report.ImageLibraryMetadata != nil {
		o = report.ImageLibraryMetadata.options
	}
	if err := ValidateImageLibrary(context.Background(), report.ImageLibraryDescriptors, report.ImageLibraryGroups, o); err != nil {
		return nil, err
	}
	if report.ImageLibrary != nil && (!report.imageLibraryReadProjection || !imageLibraryProjectionMatches(report)) {
		return nil, imageLibraryError("ambiguous-representations", "")
	}
	doc, err := boundedLibraryDocument(report.ImageLibraryDescriptors, report.ImageLibraryGroups, o)
	if err != nil {
		return nil, err
	}
	if len(report.ImageLibraryOrder) > 0 {
		if err := validLibraryOrder(report.ImageLibraryOrder, len(report.ImageLibraryDescriptors), len(report.ImageLibraryGroups)); err != nil {
			return nil, err
		}
		doc.Content = libraryOrderedItems(doc.Content, len(report.ImageLibraryDescriptors), report.ImageLibraryOrder)
	}
	if b := report.ImageLibraryMetadata; b != nil {
		d, err := libraryDigest(doc, o)
		if err != nil {
			return nil, err
		}
		if d != b.digest {
			return nil, imageLibraryError("stale-metadata", "")
		}
		obj, err := b.document.DatasetWithMetadataSnapshot(b.snapshot)
		if err != nil {
			return nil, err
		}
		seq, ok := obj.Get(tagContentSequence)
		if !ok {
			return []core.DataSet{}, nil
		}
		v, ok := seq.Value.(core.SequenceValue)
		if !ok {
			return nil, imageLibraryError("shape", "")
		}
		return v.Items, nil
	}
	if _, err := libraryDigest(doc, o); err != nil {
		return nil, err
	}
	out := make([]core.DataSet, 0, len(doc.Content))
	for _, item := range doc.Content {
		ds, err := contentItemDataSet(item)
		if err != nil {
			return nil, err
		}
		out = append(out, ds)
	}
	return out, nil
}

func readGroupedImageLibrary(obj *object.Object, options ImageLibraryOptions) ([]ContentItem, []ImageLibraryGroup, *ImageLibraryMetadataBinding, error) {
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := preflightLibraryObject(context.Background(), obj, o); err != nil {
		return nil, nil, nil, err
	}
	dec := &decoder{maxDepth: o.MaxDepth, maxItems: o.MaxItems, maxReferences: o.MaxItems, maxPathComponents: o.MaxDepth + 1}
	children, _ := dec.sequence(obj, tagContentSequence)
	items, err := readContentItems(dec, children, 1, ContentItemIdentifier{1})
	if err != nil {
		return nil, nil, nil, err
	}
	if dec.err != nil {
		return nil, nil, nil, dec.err
	}
	var descriptors []ContentItem
	groups := []ImageLibraryGroup{}
	for i, item := range items {
		path := fmt.Sprintf("1.%d", i+1)
		if item.RelationshipType == RelationshipHasAcqContext {
			descriptors = append(descriptors, item)
			continue
		}
		if item.ValueType != ValueContainer || !matchesConcept(item.ConceptName, imageLibraryGroupConcept) || item.RelationshipType != RelationshipContains {
			return nil, nil, nil, imageLibraryError("library-shape", path)
		}
		header := item
		header.Children = nil
		g := ImageLibraryGroup{Header: header}
		for j, child := range item.Children {
			p := fmt.Sprintf("%s.%d", path, j+1)
			if child.RelationshipType == RelationshipHasAcqContext {
				g.Descriptors = append(g.Descriptors, child)
				g.Order = append(g.Order, -len(g.Descriptors))
			} else if child.ValueType == ValueImage && child.RelationshipType == RelationshipContains {
				g.Entries = append(g.Entries, ImageLibraryEntry{Image: child.Image, Descriptors: child.Children, Header: func() ContentItem { h := child; h.Children = nil; return h }()})
				g.Order = append(g.Order, len(g.Entries))
			} else {
				return nil, nil, nil, imageLibraryError("group-shape", p)
			}
		}
		groups = append(groups, g)
	}
	order := make([]int, 0, len(items))
	di, gi := 0, 0
	for _, item := range items {
		if item.RelationshipType == RelationshipHasAcqContext {
			di++
			order = append(order, -di)
		} else {
			gi++
			order = append(order, gi)
		}
	}
	rootContexts := ContentItemCodeContexts{Path: "1"}
	_, rootContexts.ConceptName, rootContexts.ConceptNameEquivalentCodes, _ = decodeCodeWithMetadata(dec, obj, tagConceptNameCodeSeq)
	if !codeContextsEmpty(rootContexts) {
		dec.addCodeContexts(rootContexts)
	}
	if identification, ok := decodeTemplateIdentification(dec, obj, ContentItemIdentifier{1}); ok {
		dec.addTemplateIdentification(identification)
	}
	numeric, err := readNumericMetadata(obj, ReadOptions{MaxDepth: o.MaxDepth, MaxItems: o.MaxItems, MaxReferences: o.MaxItems, MaxPathComponents: o.MaxDepth + 1, MaxFindings: 1024})
	if err != nil {
		return nil, nil, nil, err
	}
	canonical := libraryDocument(descriptors, groups)
	canonical.Content = libraryOrderedItems(canonical.Content, len(descriptors), order)
	byPath := map[string]NumericRepresentation{}
	for _, n := range numeric.Entries {
		byPath[n.Path] = n.Representation
	}
	walker := newReferenceWalker(canonical.Content, ContentItemIdentifier{1}, 1)
	for {
		entry, ok := walker.next()
		if !ok {
			break
		}
		if n, present := byPath[entry.path.String()]; present {
			if entry.item.Measurement == nil {
				return nil, nil, nil, imageLibraryError("numeric-association", entry.path.String())
			}
			value, _, err := n.Resolve()
			if err != nil {
				return nil, nil, nil, err
			}
			entry.item.Measurement.Value = value
		}
		walker.push(entry.item.Children, entry.path, entry.depth+1)
	}
	b, err := BindImageLibraryMetadataWithOrder(descriptors, groups, order, dec.templateMetadata, numeric, o)
	if err != nil {
		return nil, nil, nil, err
	}
	return descriptors, groups, b, nil
}

var libraryUnitPattern = regexp.MustCompile(`UNITS = (?:EV |DT )?\(([^,]+), UCUM, "([^"]+)"\)`)

func libraryRow(item ContentItem, modality string, root bool) (int, catalogRow, bool) {
	var fallbackTemplate int
	var fallback catalogRow
	for _, t := range standardTemplateCatalog {
		if (root && t.id != 1600) || (!root && (t.id < 1602 || t.id > 1608)) {
			continue
		}
		for _, row := range t.rows {
			if (!root || (row.relationship == RelationshipHasAcqContext && row.level == 1)) && row.include == 0 && item.ValueType == row.valueType && item.ConceptName.CodeValue == row.code.Value && item.ConceptName.CodingSchemeDesignator == row.code.Scheme {
				if root || libraryApplies(t.id, modality) {
					return t.id, row, true
				}
				fallbackTemplate, fallback = t.id, row
			}
		}
	}
	return fallbackTemplate, fallback, fallbackTemplate != 0
}
func libraryModality(items []ContentItem) string {
	for _, i := range items {
		if i.ValueType == ValueCode && i.ConceptName.CodeValue == "121139" && i.ConceptName.CodingSchemeDesignator == "DCM" && i.Code.CodingSchemeDesignator == "DCM" {
			return i.Code.CodeValue
		}
	}
	return ""
}
func libraryApplies(tid int, modality string) bool {
	switch tid {
	case 1602:
		return true
	case 1603:
		return strings.Contains(" CR DX IO MG PX RF RG XA ", " "+modality+" ") && modality != ""
	case 1604:
		return modality == "CT" || modality == "MR" || modality == "PT"
	case 1605:
		return modality == "CT"
	case 1606, 1608:
		return modality == "MR"
	case 1607:
		return modality == "PT"
	}
	return false
}

// ValidateImageLibrary checks finite structure, selected TID row values, units,
// singleton cardinality, modality applicability and PET glucose date/time.
// Extensible unknown HAS ACQ CONTEXT descriptors are retained and traversed;
// their clinical meaning/CID membership is not guessed by this validator.
func ValidateImageLibrary(ctx context.Context, descriptors []ContentItem, groups []ImageLibraryGroup, options ImageLibraryOptions) error {
	if ctx == nil {
		return imageLibraryError("nil-context", "")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return err
	}
	doc, err := boundedLibraryDocument(descriptors, groups, o)
	if err != nil {
		return err
	}
	if _, err = libraryDigest(doc, o); err != nil {
		if errors.Is(err, ErrResourceLimitExceeded) {
			return imageLibraryError("resource-limit", "")
		}
		return imageLibraryError("unpreservable-structure", "")
	}
	var validate func([]ContentItem, string, string, string, int, bool) error
	validate = func(items []ContentItem, path, modality, parent string, depth int, conditions bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth > o.MaxDepth {
			return imageLibraryError("resource-limit", path)
		}
		seen := map[string]int{}
		glucose := false
		observed := false
		date, time := false, false
		for n, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			p := fmt.Sprintf("%s.%d", path, n+1)
			if len(item.ReferencedContentItemIdentifier) > 0 {
				return imageLibraryError("by-reference-descriptor", p)
			}
			tid, row, known := libraryRow(item, modality, parent == "library")
			if !known {
				for _, t := range standardTemplateCatalog {
					if (parent == "library" && t.id != 1600) || (parent != "library" && (t.id < 1602 || t.id > 1608)) {
						continue
					}
					for _, r := range t.rows {
						if r.include == 0 && item.ConceptName.CodeValue == r.code.Value && item.ConceptName.CodingSchemeDesignator == r.code.Scheme {
							return imageLibraryError("value-type", p)
						}
					}
				}
			}
			if parent == "111031" {
				if item.RelationshipType != RelationshipHasConceptMod {
					return imageLibraryError("relationship", p)
				}
			} else if item.RelationshipType != RelationshipHasAcqContext {
				return imageLibraryError("relationship", p)
			}
			if known {
				key := strconv.Itoa(tid) + ":" + row.label
				seen[key]++
				if row.max > 0 && seen[key] > row.max {
					return imageLibraryError("duplicate-singleton", p)
				}
				if conditions && tid != 1600 && tid != 1602 && !libraryApplies(tid, modality) {
					return imageLibraryError("modality-applicability", p)
				}
				if tid == 1603 && row.level > 0 && parent != "111031" {
					return imageLibraryError("modifier-parent", p)
				}
				if row.level == 0 && parent == "111031" {
					return imageLibraryError("modifier-parent", p)
				}
				if m := libraryUnitPattern.FindStringSubmatch(row.valueSet); m != nil {
					if item.Measurement == nil || item.Measurement.Units.CodeValue != m[1] || item.Measurement.Units.CodingSchemeDesignator != "UCUM" {
						return imageLibraryError("units", p)
					}
				}
			}
			switch item.ValueType {
			case ValueNum:
				if item.Measurement != nil && (math.IsNaN(item.Measurement.Value) || math.IsInf(item.Measurement.Value, 0)) {
					return imageLibraryError("numeric-value", p)
				}
			case ValueCode:
				if item.Code.Empty() {
					return imageLibraryError("code-value", p)
				}
			case ValueDate:
				if !libraryDA(item.Date) {
					return imageLibraryError("date-value", p)
				}
			case ValueTime:
				if !libraryTM(item.Time) {
					return imageLibraryError("time-value", p)
				}
			case ValueDateTime:
				if _, err := dcmtime.ParseDatetime(item.DateTime); err != nil {
					return imageLibraryError("datetime-value", p)
				}
			case ValueUIDRef:
				if !validUID(item.UID) {
					return imageLibraryError("uid-value", p)
				}
			}
			if item.ConceptName.CodingSchemeDesignator == "LN" && item.ConceptName.CodeValue == "14749-6" {
				glucose = true
				for _, e := range item.ValueElements {
					if e.Tag() == core.NewTag(0x0040, 0xA032) {
						value, err := numericResidentString(e, core.VRDT)
						if err != nil {
							return imageLibraryError("observation-datetime", p)
						}
						if _, err := dcmtime.ParseDatetime(value); err != nil {
							return imageLibraryError("observation-datetime", p)
						}
						observed = true
					}
				}
			}
			if item.ConceptName.CodingSchemeDesignator == "DCM" && item.ConceptName.CodeValue == "127857" && item.ValueType == ValueDate {
				date = true
			}
			if item.ConceptName.CodingSchemeDesignator == "DCM" && item.ConceptName.CodeValue == "127858" && item.ValueType == ValueTime {
				time = true
			}
			if len(item.Children) > 0 {
				if err := validate(item.Children, p, modality, item.ConceptName.CodeValue, depth+1, conditions); err != nil {
					return err
				}
			}
		}
		if conditions && glucose && !observed && (!date || !time) {
			return imageLibraryError("pet-glucose-date-time", path)
		}
		return nil
	}
	if err := validate(descriptors, "1", "", "library", 1, false); err != nil {
		return err
	}
	instances := map[string]int{}
	for i, g := range groups {
		p := fmt.Sprintf("1.%d", len(descriptors)+i+1)
		groupCommon := g.Descriptors
		inherited := libraryModality(g.Descriptors)
		if err := validate(g.Descriptors, p, inherited, "", 2, false); err != nil {
			return err
		}
		for j, e := range g.Entries {
			ordinal := len(g.Descriptors) + j + 1
			for k, value := range g.Order {
				if value == j+1 {
					ordinal = k + 1
					break
				}
			}
			ep := fmt.Sprintf("%s.%d", p, ordinal)
			if !validUID(e.Image.SOPClassUID) || !validUID(e.Image.SOPInstanceUID) {
				return imageLibraryError("image-reference", ep)
			}
			for _, f := range e.Image.Frames {
				if f < 1 {
					return imageLibraryError("frame-number", ep)
				}
			}
			modality := libraryModality(e.Descriptors)
			if modality == "" {
				modality = inherited
			}

			if previous, ok := instances[e.Image.SOPInstanceUID]; ok && previous != i {
				return imageLibraryError("instance-in-multiple-groups", ep)
			}
			instances[e.Image.SOPInstanceUID] = i
			if err := validate(e.Descriptors, ep, modality, "", 3, false); err != nil {
				return err
			}
			effective, err := effectiveLibraryDescriptors(groupCommon, e.Descriptors)
			if err != nil {
				return imageLibraryError("group-conflict", ep)
			}
			for _, d := range effective {
				if d.ValueType == ValueNum && d.ConceptName.CodeValue == "121140" && d.ConceptName.CodingSchemeDesignator == "DCM" && d.Measurement != nil {
					n := d.Measurement.Value
					if n < 1 || math.Trunc(n) != n {
						return imageLibraryError("frame-number", ep)
					}
					for _, f := range e.Image.Frames {
						if float64(f) > n {
							return imageLibraryError("frame-number", ep)
						}
					}
				}
			}
			if err := validate(effective, ep, modality, "", 3, true); err != nil {
				return err
			}
		}
	}
	return nil
}
func libraryDA(s string) bool { _, err := dcmtime.ParseDate(s); return len(s) == 8 && err == nil }
func libraryTM(s string) bool { _, err := dcmtime.ParseTime(s); return s != "" && err == nil }
func validUID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if p == "" || strings.Trim(p, "0123456789") != "" || (len(p) > 1 && p[0] == '0') {
			return false
		}
	}
	return true
}

// TID1601 explicitly makes entry descriptors override group common facts.
// CODE/TEXT alternatives in1602 are separate rows and may coexist.
func effectiveLibraryDescriptors(common, own []ContentItem) ([]ContentItem, error) {
	overrides := map[string]bool{}
	key := func(item ContentItem) string {
		return item.ConceptName.CodingSchemeDesignator + "\x00" + item.ConceptName.CodeValue + "\x00" + string(item.ValueType)
	}
	for _, item := range own {
		overrides[key(item)] = true
	}
	out := make([]ContentItem, 0, len(common)+len(own))
	for _, item := range common {
		if !overrides[key(item)] {
			out = append(out, item)
		}
	}
	return append(out, own...), nil
}

func validLibraryOrder(order []int, descriptors, entries int) error {
	if len(order) == 0 {
		return nil
	}
	if len(order) != descriptors+entries {
		return imageLibraryError("order-shape", "")
	}
	seen := map[int]bool{}
	for _, n := range order {
		if n == 0 || n > entries || n < -descriptors || seen[n] {
			return imageLibraryError("order-shape", "")
		}
		seen[n] = true
	}
	return nil
}
func libraryOrderedItems(items []ContentItem, descriptors int, order []int) []ContentItem {
	if len(order) == 0 {
		return items
	}
	out := make([]ContentItem, 0, len(items))
	for _, n := range order {
		if n < 0 {
			out = append(out, items[-n-1])
		} else {
			out = append(out, items[descriptors+n-1])
		}
	}
	return out
}

// BindImageLibraryMetadataWithOrder also preserves explicit original root order.
func BindImageLibraryMetadataWithOrder(descriptors []ContentItem, groups []ImageLibraryGroup, order []int, metadata TemplateValidationMetadata, numeric NumericRepresentationMetadata, options ImageLibraryOptions) (*ImageLibraryMetadataBinding, error) {
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return nil, err
	}
	if err := validLibraryOrder(order, len(descriptors), len(groups)); err != nil {
		return nil, err
	}
	for _, g := range groups {
		if err := validLibraryOrder(g.Order, len(g.Descriptors), len(g.Entries)); err != nil {
			return nil, err
		}
	}
	doc, err := boundedLibraryDocument(descriptors, groups, o)
	if err != nil {
		return nil, err
	}
	doc.Content = libraryOrderedItems(doc.Content, len(descriptors), order)
	d, err := libraryDigest(doc, o)
	if err != nil {
		return nil, err
	}
	snapshot, err := BindNumericRepresentations(doc, metadata, numeric, MetadataSnapshotOptions{MaxDepth: o.MaxDepth, MaxItems: o.MaxItems, MaxValueBytes: o.MaxValueBytes})
	if err != nil {
		return nil, err
	}
	return &ImageLibraryMetadataBinding{document: doc, snapshot: snapshot, digest: d, options: o, order: append([]int(nil), order...)}, nil
}
func structuredLibraryContainer(report *MeasurementReport, children []core.DataSet) (core.DataSet, error) {
	ds := containerItem(RelationshipContains, imageLibraryConcept, "", children...)
	if b := report.ImageLibraryMetadata; b != nil {
		obj, err := b.document.DatasetWithMetadataSnapshot(b.snapshot)
		if err != nil {
			return core.DataSet{}, err
		}
		for _, tag := range []core.Tag{tagConceptNameCodeSeq, tagContentTemplateSequence} {
			if e, ok := obj.Get(tag); ok {
				idx := elementIndex(ds.Elements, tag)
				if idx >= 0 {
					ds.Elements[idx] = e
				} else {
					ds.Elements = append(ds.Elements, e)
				}
			}
		}
	}
	return ds, nil
}

// Ordinary, descriptor-free one-group reports keep the legacy flat contract.
// Acquisition context, multiple groups or richer encoding metadata opt into
// lossless specialized groups; arbitrary old nested containers remain legacy.
func structuredImageLibraryNeeded(obj *object.Object, o ImageLibraryOptions) (bool, error) {
	if err := preflightLibraryObject(context.Background(), obj, o); err != nil {
		return false, err
	}
	rich := false
	groups := 0
	dec := &decoder{}
	var visit func(*object.Object, int) error
	visit = func(node *object.Object, depth int) error {
		if depth > o.MaxDepth {
			return imageLibraryError("resource-limit", "")
		}
		if dec.string(node, tagRelationshipType) == RelationshipHasAcqContext {
			rich = true
		}
		for _, tag := range []core.Tag{tagConceptNameCodeSeq, tagConceptCodeSeq} {
			if codes, ok := node.GetSequence(tag); ok {
				for _, code := range codes {
					for _, el := range code.Elements() {
						if el.Tag() != tagCodeValue && el.Tag() != tagCodingScheme && el.Tag() != tagCodeMeaning {
							rich = true
						}
					}
				}
			}
		}
		if ids, ok := node.GetSequence(tagContentTemplateSequence); ok && len(ids) > 0 {
			if depth > 0 || dec.string(ids[0], tagTemplateIdentifier) != "1600" || len(ids[0].Elements()) > 2 {
				rich = true
			}
		}
		children, _ := dec.sequence(node, tagContentSequence)
		for _, child := range children {
			if depth == 0 {
				concept, _ := decodeCode(dec, child, tagConceptNameCodeSeq)
				if matchesConcept(concept, imageLibraryGroupConcept) {
					groups++
				}
			}
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		return dec.err
	}
	if err := visit(obj, 0); err != nil {
		return false, err
	}
	return rich || groups > 1, nil
}

// ValidateImageLibraryTemplate runs the existing pinned TID1600 registry as a
// separate qualification layer. Domain checks are performed first. The returned
// report preserves unresolved CID/condition findings; success is not a complete
// SR IOD, clinical, peer or PACS qualification.
func ValidateImageLibraryTemplate(ctx context.Context, report *MeasurementReport, options ImageLibraryOptions, templateOptions TemplateValidationOptions) (ValidationReport, error) {
	if report == nil {
		return ValidationReport{}, imageLibraryError("report-required", "")
	}
	o, err := normalizeImageLibraryOptions(options)
	if err != nil {
		return ValidationReport{}, err
	}
	if err := ValidateImageLibrary(ctx, report.ImageLibraryDescriptors, report.ImageLibraryGroups, o); err != nil {
		return ValidationReport{}, err
	}
	if report.ImageLibrary != nil && (!report.imageLibraryReadProjection || !imageLibraryProjectionMatches(report)) {
		return ValidationReport{}, imageLibraryError("ambiguous-representations", "")
	}
	doc, err := boundedLibraryDocument(report.ImageLibraryDescriptors, report.ImageLibraryGroups, o)
	if err != nil {
		return ValidationReport{}, err
	}
	if err := validLibraryOrder(report.ImageLibraryOrder, len(report.ImageLibraryDescriptors), len(report.ImageLibraryGroups)); err != nil {
		return ValidationReport{}, err
	}
	doc.Content = libraryOrderedItems(doc.Content, len(report.ImageLibraryDescriptors), report.ImageLibraryOrder)
	registry, err := NewStandardTemplateRegistry(1600)
	if err != nil {
		return ValidationReport{}, err
	}
	validator, err := NewTemplateValidator(registry, templateOptions)
	if err != nil {
		return ValidationReport{}, err
	}
	key, _ := StandardTemplateKey(1600)
	metadata := TemplateValidationMetadata{}
	if b := report.ImageLibraryMetadata; b != nil {
		digest, err := libraryDigest(doc, o)
		if err != nil {
			return ValidationReport{}, err
		}
		if digest != b.digest {
			return ValidationReport{}, imageLibraryError("stale-metadata", "")
		}
		// The binding's document root is the library itself. Its digest and
		// metadata are checked in that original coordinate system first.
		if err := b.snapshot.Check(b.document); err != nil {
			return ValidationReport{}, err
		}
		metadata = b.Metadata()
	}
	// TID1600 is a non-root template: row 1 describes the Image Library
	// CONTAINER, not its children. Introduce exactly that node and relocate
	// defensive sidecar copies into the validator's document coordinates.
	for i := range metadata.CodeContexts {
		metadata.CodeContexts[i].Path = "1." + metadata.CodeContexts[i].Path
	}
	for i := range metadata.TemplateIdentifications {
		metadata.TemplateIdentifications[i].Path = "1." + metadata.TemplateIdentifications[i].Path
	}
	library := ContentItem{ValueType: ValueContainer, RelationshipType: RelationshipContains,
		ConceptName: imageLibraryConcept, ContinuityOfContent: "SEPARATE", Children: doc.Content}
	doc.Content = []ContentItem{library}
	return validator.ValidateWithMetadata(ctx, doc, key, metadata)
}

// A rich read preserves the established flat-reference API without discarding
// descriptors. Only reader-derived projections may accompany structured data;
// every write rechecks exact references and emitted ordering, then emits one
// structured library. Manual dual authoring and divergent edits fail closed.
func imageLibraryProjectionMatches(report *MeasurementReport) bool {
	if len(report.ImageLibraryOrder) > 0 {
		if validLibraryOrder(report.ImageLibraryOrder, len(report.ImageLibraryDescriptors), len(report.ImageLibraryGroups)) != nil {
			return false
		}
	}
	index := 0
	reference := func(want ImageReference) bool {
		if index >= len(report.ImageLibrary) {
			return false
		}
		got := report.ImageLibrary[index]
		index++
		return got.StudyInstanceUID == want.StudyInstanceUID && got.SeriesInstanceUID == want.SeriesInstanceUID && got.SOPClassUID == want.SOPClassUID && got.SOPInstanceUID == want.SOPInstanceUID && reflect.DeepEqual(got.Frames, want.Frames)
	}
	group := func(g ImageLibraryGroup) bool {
		if len(g.Order) == 0 {
			for _, e := range g.Entries {
				if !reference(e.Image) {
					return false
				}
			}
			return true
		}
		for _, ordinal := range g.Order {
			if ordinal > 0 {
				if ordinal > len(g.Entries) || !reference(g.Entries[ordinal-1].Image) {
					return false
				}
			}
		}
		return true
	}
	if len(report.ImageLibraryOrder) == 0 {
		for _, g := range report.ImageLibraryGroups {
			if !group(g) {
				return false
			}
		}
	} else {
		for _, ordinal := range report.ImageLibraryOrder {
			if ordinal > 0 {
				if !group(report.ImageLibraryGroups[ordinal-1]) {
					return false
				}
			}
		}
	}
	return index == len(report.ImageLibrary)
}
