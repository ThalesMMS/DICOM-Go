package sr

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"sync"
)

// ErrMeasurementContext identifies refusal by the finite, opt-in TID 1500
// context profile. Such a refusal does not declare a generic DICOM VR invalid.
var ErrMeasurementContext = errors.New("dicom/sr: measurement context profile rejected")

const (
	MeasurementContextShape       = "context-shape"
	MeasurementContextDuplicate   = "duplicate-singleton"
	MeasurementContextConflict    = "context-conflict"
	MeasurementContextEvidence    = "evidence-admission"
	MeasurementContextLimit       = "context-resource-limit"
	MeasurementContextUnsupported = "unmodeled-content"
	// MeasurementContextRWVMExtension reports the explicit 1411 per-NUM
	// INFERRED FROM extension at the TID 1419 row 19 / SR IOD seam.
	MeasurementContextRWVMExtension = "rwvm-inferred-from-extension"
	MeasurementContextOpaqueValues  = "opaque-value-elements"
)

// MeasurementContextError contains only a stable code and a validated content path.
type MeasurementContextError struct{ Code, Path string }

func (e *MeasurementContextError) Error() string {
	if e == nil {
		return ErrMeasurementContext.Error()
	}
	s := ErrMeasurementContext.Error() + ": " + e.Code
	if validNumericPath(e.Path) {
		s += " at " + e.Path
	}
	return s
}
func (e *MeasurementContextError) Unwrap() error { return ErrMeasurementContext }
func (e *MeasurementContextError) Is(target error) bool {
	return target == ErrResourceLimitExceeded && e != nil && e.Code == MeasurementContextLimit
}
func measurementContextError(code, path string) error {
	return &MeasurementContextError{Code: code, Path: path}
}

// MeasurementContextCode carries all code forms through the existing sidecar.
// EquivalentCodes preserve order; Code Meaning is never used as identity.
type MeasurementContextCode struct {
	Code            CodedEntry
	Context         CodeContext
	EquivalentCodes []EquivalentCodeEntry
}
type MeasurementFindingSite struct {
	Site                  MeasurementContextCode
	Laterality            *MeasurementContextCode
	TopographicalModifier *MeasurementContextCode
}

// MeasurementAlgorithm is TID 4019 at one explicit parent level. Name and
// Version are mandatory when present; no inherited copy or default is invented.
type MeasurementAlgorithm struct {
	Name         string
	NameCode     *MeasurementContextCode
	Version      string
	Manufacturer string
	Parameters   []string
	Family       *MeasurementContextCode
}

// MeasurementTimePoint is TID 1502, exclusively observation context. Numeric
// representations retain DS/FD/rational provenance; order has UCUM 1, offset d.
type MeasurementTimePoint struct {
	SubjectIdentifier  string
	ProtocolIdentifier string
	TimePoint          string
	Types              []MeasurementContextCode
	Order              *NumericRepresentation
	TemporalOffset     *NumericRepresentation
	EventType          *MeasurementContextCode
}
type MeasurementQualitativeModifier struct{ ConceptName, Value MeasurementContextCode }

// MeasurementQualitativeEvaluation uses exactly one CODE or TEXT answer.
// Coded modifiers are admitted only for CODE answers by TID 1500/1501/1411.
type MeasurementQualitativeEvaluation struct {
	ConceptName MeasurementContextCode
	ValueType   ValueType
	Code        MeasurementContextCode
	Text        string
	Modifiers   []MeasurementQualitativeModifier
}

// MeasurementReferenceEvidence is an admitted Hierarchical SOP Instance
// Reference tuple, not a request to discover an instance or apply its RWVM.
type MeasurementReferenceEvidence struct{ StudyInstanceUID, SeriesInstanceUID, SOPClassUID, SOPInstanceUID string }

const MeasurementRWVMStorage = "1.2.840.10008.5.1.4.1.1.67"

type MeasurementNumericContext struct {
	Path              string
	ConceptName       MeasurementContextCode
	Units             MeasurementContextCode
	Numeric           *NumericRepresentation
	Method            *MeasurementContextCode
	FindingSites      []MeasurementFindingSite
	Algorithm         *MeasurementAlgorithm
	RealWorldValueMap *MeasurementReferenceEvidence
}
type MeasurementGroupContext struct {
	Path              string
	ActivitySession   string
	TimePoint         *MeasurementTimePoint
	Method            *MeasurementContextCode
	FindingSites      []MeasurementFindingSite
	Algorithm         *MeasurementAlgorithm
	RealWorldValueMap *MeasurementReferenceEvidence
	Qualitative       []MeasurementQualitativeEvaluation
	Measurements      []MeasurementNumericContext
}

// MeasurementReportContext records explicit container/group/NUM scopes.
// Dense nonempty Groups/Measurements slices must match authored legacy counts.
// Path is an output locator, not a persistent node identity or editable binding.
type MeasurementReportContext struct {
	ImagingAlgorithm     *MeasurementAlgorithm
	QualitativeAlgorithm *MeasurementAlgorithm
	Qualitative          []MeasurementQualitativeEvaluation
	Groups               []MeasurementGroupContext
}

// MeasurementContextFinding reports content retained in the canonical document
// or an explicit profile seam/opaque value limitation. It contains no values
// or clinical meaning.
type MeasurementContextFinding struct{ Code, Path string }
type MeasurementContextValidation struct {
	Unsupported   []MeasurementContextFinding
	Catalog       ValidationReport
	Relationships ValidationReport
}

// MeasurementContextOptions reuse the existing reader and the one structural
// snapshot. Extra quotas bound immutable evidence, projected strings and findings;
// they bound logical input, not RSS. Zero values select finite defaults.
type MeasurementContextOptions struct {
	Read          ReadOptions
	Snapshot      MetadataSnapshotOptions
	Catalog       TemplateValidationOptions
	StrictCatalog bool
	// CharacterSet is used only by explicit Bind; nil selects UTF-8. Read retains
	// the admitted source charset, and Build authors UTF-8.
	CharacterSet []string
	// ValueElementsBigEndian is used only by explicit Bind for opaque raw values
	// retained in the generic tree. Read derives it from the source; Build uses
	// canonical little endian values. Big endian opaque output is refused.
	ValueElementsBigEndian bool
	MaxEvidence            int
	// MaxEvidenceItems also caps source resident elements/sequence items before decoding.
	MaxEvidenceItems   int
	MaxStringBytes     int
	MaxContextFindings int
}

func DefaultMeasurementContextOptions() MeasurementContextOptions {
	r := DefaultReadOptions()
	r.ResolveReferences = true
	r.ValidateRelationships = true
	c := DefaultTemplateValidationOptions()
	c.Mode = ValidationModeWarn
	return MeasurementContextOptions{Read: r, Snapshot: DefaultMetadataSnapshotOptions(), Catalog: c, MaxEvidence: 100_000, MaxEvidenceItems: 300_000, MaxStringBytes: 1 << 20, MaxContextFindings: 1024}
}
func normalizeMeasurementContextOptions(o MeasurementContextOptions) (MeasurementContextOptions, error) {
	d := DefaultMeasurementContextOptions()
	o.Read = normalizeReadOptions(o.Read)
	if o.Snapshot.MaxDepth == 0 {
		o.Snapshot.MaxDepth = d.Snapshot.MaxDepth
	}
	if o.Snapshot.MaxItems == 0 {
		o.Snapshot.MaxItems = d.Snapshot.MaxItems
	}
	if o.Snapshot.MaxValueBytes == 0 {
		o.Snapshot.MaxValueBytes = d.Snapshot.MaxValueBytes
	}
	o.Catalog = normalizeTemplateValidationOptions(o.Catalog)
	o.Catalog.Mode = ValidationModeWarn
	if o.StrictCatalog {
		o.Catalog.Mode = ValidationModeStrict
	}
	if o.MaxEvidence == 0 {
		o.MaxEvidence = d.MaxEvidence
	}
	if o.MaxEvidenceItems == 0 {
		o.MaxEvidenceItems = d.MaxEvidenceItems
	}
	if o.MaxStringBytes == 0 {
		o.MaxStringBytes = d.MaxStringBytes
	}
	if o.MaxContextFindings == 0 {
		o.MaxContextFindings = d.MaxContextFindings
	}
	if o.Read.MaxDepth < 1 || o.Read.MaxDepth > 64 || o.Read.MaxItems < 1 || o.Read.MaxReferences < 1 || o.Read.MaxPathComponents < 1 || o.Read.MaxFindings < 1 || o.Snapshot.MaxDepth < 1 || o.Snapshot.MaxDepth > 64 || o.Snapshot.MaxItems < 1 || o.Snapshot.MaxValueBytes < 1 || o.MaxEvidence < 1 || o.MaxEvidenceItems < 1 || o.MaxStringBytes < 1 || o.MaxContextFindings < 1 {
		return o, measurementContextError(MeasurementContextLimit, "")
	}
	return o, nil
}

// DetailedMeasurementReport is a guarded view of the canonical generic tree.
// Report and Context return fresh projections. Output never reconstructs read
// content through the legacy model. No concurrent mutation is permitted.
type DetailedMeasurementReport struct {
	document            *Document
	snapshot            *TemplateMetadataSnapshot
	evidence            []MeasurementReferenceEvidence
	evidenceIndex       map[string]MeasurementReferenceEvidence
	charset             []string
	frameOfReferenceUID string
	options             MeasurementContextOptions
	validation          MeasurementContextValidation
}

func (r *DetailedMeasurementReport) Document() *Document {
	if r == nil {
		return nil
	}
	return r.document
}
func (r *DetailedMeasurementReport) MetadataSnapshot() *TemplateMetadataSnapshot {
	if r == nil {
		return nil
	}
	return r.snapshot
}
func (r *DetailedMeasurementReport) check() error {
	if r == nil {
		return measurementContextError(MeasurementContextShape, "")
	}
	return r.snapshot.Check(r.document)
}
func (r *DetailedMeasurementReport) Report() (*MeasurementReport, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	report, _, _, err := r.project()
	return report, err
}
func (r *DetailedMeasurementReport) Context() (MeasurementReportContext, error) {
	if err := r.check(); err != nil {
		return MeasurementReportContext{}, err
	}
	_, c, _, err := r.project()
	return c, err
}
func (r *DetailedMeasurementReport) Evidence() ([]MeasurementReferenceEvidence, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	return append([]MeasurementReferenceEvidence(nil), r.evidence...), nil
}
func (r *DetailedMeasurementReport) Validation() (MeasurementContextValidation, error) {
	if err := r.check(); err != nil {
		return MeasurementContextValidation{}, err
	}
	v := r.validation
	v.Unsupported = append([]MeasurementContextFinding(nil), v.Unsupported...)
	v.Catalog.Findings = append([]DiagnosticFinding(nil), v.Catalog.Findings...)
	v.Relationships.Findings = append([]DiagnosticFinding(nil), v.Relationships.Findings...)
	for i := range v.Catalog.Findings {
		v.Catalog.Findings[i].Path = v.Catalog.Findings[i].Path.Clone()
		v.Catalog.Findings[i].Target = v.Catalog.Findings[i].Target.Clone()
	}
	for i := range v.Relationships.Findings {
		v.Relationships.Findings[i].Path = v.Relationships.Findings[i].Path.Clone()
		v.Relationships.Findings[i].Target = v.Relationships.Findings[i].Target.Clone()
	}
	return v, nil
}

// Dataset checks the one guard before any output allocation, then reemits the
// original generic content, numeric/code metadata and admitted evidence. Header
// and Common follow Document's mutable header contract, outside the tree guard.
func (r *DetailedMeasurementReport) Dataset() (*object.Object, error) {
	if err := r.check(); err != nil {
		return nil, err
	}
	if err := r.opaqueOutputAdmission(); err != nil {
		return nil, err
	}
	obj, err := r.document.DatasetWithMetadataSnapshot(r.snapshot)
	if err != nil {
		return nil, err
	}
	if len(r.evidence) > 0 {
		refs := make([]ImageReference, 0, len(r.evidence))
		for _, e := range r.evidence {
			refs = append(refs, ImageReference{StudyInstanceUID: e.StudyInstanceUID, SeriesInstanceUID: e.SeriesInstanceUID, SOPClassUID: e.SOPClassUID, SOPInstanceUID: e.SOPInstanceUID})
		}
		obj.Put(currentRequestedProcedureEvidenceSequence(refs))
	}
	if len(r.charset) > 0 {
		obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue(append([]string(nil), r.charset...))})
	}
	if r.frameOfReferenceUID != "" {
		obj.Put(strElem(derivedio.TagFrameOfReferenceUID, core.VRUI, r.frameOfReferenceUID))
	}
	if err = r.document.Common.Apply(obj, object.InheritWithoutFrameOfReference); err != nil {
		return nil, err
	}
	return obj, nil
}
func (r *DetailedMeasurementReport) File() (*object.File, error) {
	obj, err := r.Dataset()
	if err != nil {
		return nil, err
	}
	return derivedio.File(r.document.SOPClassUID, r.document.SOPInstanceUID, obj)
}

// BindMeasurementReportWithContext admits a caller's already-correct snapshot
// and evidence. It never repairs paths or rebinds an obsolete snapshot.
func BindMeasurementReportWithContext(doc *Document, snapshot *TemplateMetadataSnapshot, evidence []MeasurementReferenceEvidence, options MeasurementContextOptions) (*DetailedMeasurementReport, error) {
	o, err := normalizeMeasurementContextOptions(options)
	if err != nil {
		return nil, err
	}
	if err = snapshot.Check(doc); err != nil {
		return nil, err
	}
	admitted, err := admitMeasurementEvidence(evidence, o)
	if err != nil {
		return nil, err
	}
	charset := options.CharacterSet
	if charset == nil {
		charset = []string{"ISO_IR 192"}
	}
	if len(charset) > 64 {
		return nil, measurementContextError(MeasurementContextLimit, "")
	}
	if _, err = dicomenc.ParseCharacterSet(charset...); err != nil {
		return nil, measurementContextError(MeasurementContextShape, "")
	}
	r := &DetailedMeasurementReport{document: doc, snapshot: snapshot, evidence: admitted, charset: append([]string(nil), charset...), options: o}
	r.evidenceIndex = make(map[string]MeasurementReferenceEvidence, len(admitted))
	for _, e := range admitted {
		r.evidenceIndex[e.SOPInstanceUID] = e
	}
	if _, _, r.validation.Unsupported, err = r.project(); err != nil {
		return nil, err
	}
	if err = r.checkEvidence(); err != nil {
		return nil, err
	}
	registry, err := measurementContextStandardRegistry()
	if err != nil {
		return nil, err
	}
	validator, err := NewTemplateValidator(registry, o.Catalog)
	if err != nil {
		return nil, err
	}
	key, _ := StandardTemplateKey(1500)
	r.validation.Catalog, err = validator.ValidateWithMetadataSnapshot(context.Background(), doc, key, snapshot)
	if err != nil {
		return nil, err
	}
	r.validation.Relationships, err = ValidateRelationships(doc, RelationshipOptions{Mode: ValidationModeStrict, MaxDepth: o.Read.MaxDepth, MaxItems: o.Read.MaxItems, MaxFindings: o.Read.MaxFindings})
	if err != nil {
		return nil, err
	}
	return r, nil
}

var measurementContextRegistryOnce sync.Once
var measurementContextRegistry *TemplateRegistry
var measurementContextRegistryError error

func measurementContextStandardRegistry() (*TemplateRegistry, error) {
	measurementContextRegistryOnce.Do(func() {
		measurementContextRegistry, measurementContextRegistryError = NewStandardTemplateRegistry(1500)
	})
	return measurementContextRegistry, measurementContextRegistryError
}
