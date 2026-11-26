package sr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// requireDSRDump returns the path of DCMTK dsrdump, skipping unless
// DICOMGO_DCMTK_INTEGRATION=1 and dsrdump is installed.
func requireDSRDump(t *testing.T) string {
	t.Helper()
	return dcmtkoracle.Tool(t, "dsrdump")
}

// oracleValueTypes are the value types DCMTK 3.7 reads (it has no TABLE).
var oracleValueTypes = []ValueType{
	ValueContainer, ValueText, ValueCode, ValueNum, ValueDateTime, ValueDate, ValueTime, ValueUIDRef,
	ValuePName, ValueComposite, ValueImage, ValueWaveform, ValueSCoord, ValueSCoord3D, ValueTCoord,
}

var oracleRelationships = []string{
	RelationshipContains, RelationshipHasObsContext, RelationshipHasAcqContext, RelationshipHasConceptMod,
	RelationshipHasProperties, RelationshipInferredFrom, RelationshipSelectedFrom,
}

// oracleItem is a Content Item of a value type with a value DCMTK accepts.
func oracleItem(valueType ValueType, relationship string) ContentItem {
	item := ContentItem{
		ValueType: valueType, RelationshipType: relationship,
		ConceptName: CodedEntry{CodeValue: "T" + strings.ToUpper(string(valueType)), CodingSchemeDesignator: "99TEST", CodeMeaning: string(valueType) + " item"},
	}
	switch valueType {
	case ValueContainer:
		item.ContinuityOfContent = "SEPARATE"
	case ValueText:
		item.Text = "text"
	case ValueCode:
		item.Code = CodedEntry{CodeValue: "C1", CodingSchemeDesignator: "99TEST", CodeMeaning: "code"}
	case ValueNum:
		item.Measurement = &Measurement{Value: 1.5, Units: CodedEntry{CodeValue: "mm", CodingSchemeDesignator: "UCUM", CodeMeaning: "millimeter"}}
	case ValueDateTime:
		item.DateTime = "20260926120000"
	case ValueDate:
		item.Date = "20260926"
	case ValueTime:
		item.Time = "120000"
	case ValueUIDRef:
		item.UID = "1.2.826.0.1.3680043.10.543.999.1"
	case ValuePName:
		item.PersonName = "TEST^ORACLE"
	case ValueComposite:
		item.Composite = CompositeReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.11.1", SOPInstanceUID: "1.2.826.0.1.3680043.10.543.999.2"}
	case ValueImage:
		item.Image = ImageReference{
			StudyInstanceUID: "1.2.826.0.1.3680043.10.543.999.11", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.999.13",
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.826.0.1.3680043.10.543.999.3",
		}
	case ValueWaveform:
		item.Waveform = WaveformReference{CompositeReference: CompositeReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.9.1.1", SOPInstanceUID: "1.2.826.0.1.3680043.10.543.999.4"}}
	case ValueSCoord:
		item.Spatial = SpatialCoordinates{GraphicType: "POINT", Coordinates: []Point2D{{X: 1, Y: 2}}}
	case ValueSCoord3D:
		item.Spatial3D = SpatialReference{FrameOfReferenceUID: "1.2.826.0.1.3680043.10.543.999.5", GraphicType: "POINT", Coordinates: []Point3D{{X: 1, Y: 2, Z: 3}}}
	case ValueTCoord:
		item.Temporal = TemporalCoordinates{RangeType: "POINT", SamplePositions: []uint32{1}}
	}
	return item
}

// oracleRootTitles are the document titles (root concept names) of the SR
// IODs whose root template fixes them (PS3.16 TID 2020, 2100, 4000, 4100,
// 4120, 5300, 7000, 10030, 11001 and 11020). DCMTK checks them since
// 92b04fe1, after 3.7.0: at the pinned 8d68392d dsrdump refuses any other
// title with "Invalid Root Concept Name" before judging a relationship.
var oracleRootTitles = map[string]CodedEntry{
	"Spectacle Prescription Report":             {CodeValue: "111671", CodingSchemeDesignator: "DCM", CodeMeaning: "Spectacle Prescription Report"},
	"Macular Grid Thickness and Volume Report":  {CodeValue: "111690", CodingSchemeDesignator: "DCM", CodeMeaning: "Macular Grid Thickness and Volume Report"},
	"Mammography CAD SR":                        {CodeValue: "111036", CodingSchemeDesignator: "DCM", CodeMeaning: "Mammography CAD Report"},
	"Chest CAD SR":                              {CodeValue: "112000", CodingSchemeDesignator: "DCM", CodeMeaning: "Chest CAD Report"},
	"Colon CAD SR":                              {CodeValue: "112220", CodingSchemeDesignator: "DCM", CodeMeaning: "Colon CAD Report"},
	"Simplified Adult Echo SR":                  {CodeValue: "125200", CodingSchemeDesignator: "DCM", CodeMeaning: "Adult Echocardiography Procedure Report"},
	"Implantation Plan SR Document":             {CodeValue: "112345", CodingSchemeDesignator: "DCM", CodeMeaning: "Implantation Plan"},
	"Patient Radiation Dose SR":                 {CodeValue: "128401", CodingSchemeDesignator: "DCM", CodeMeaning: "Patient Radiation Dose Report"},
	"Planned Imaging Agent Administration SR":   {CodeValue: "130226", CodingSchemeDesignator: "DCM", CodeMeaning: "Planned Imaging Agent Administration"},
	"Performed Imaging Agent Administration SR": {CodeValue: "130227", CodingSchemeDesignator: "DCM", CodeMeaning: "Performed Imaging Agent Administration"},
}

// oracleRootTitle returns the document title of an oracle document: the root
// template's title when the IOD fixes one, a private code otherwise.
func oracleRootTitle(sopClassUID string) CodedEntry {
	if iod := relationshipIODsByClass[sopClassUID]; iod != nil {
		if title, ok := oracleRootTitles[iod.name]; ok {
			return title
		}
	}
	return CodedEntry{CodeValue: "TROOT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Relationship oracle"}
}

// writeOracleDocument encodes content without the relationship checks.
func writeOracleDocument(t *testing.T, path, sopClassUID string, content []ContentItem) {
	t.Helper()
	modality := "SR"
	if sopClassUID == KeyObjectSelectionDocumentStorage || sopClassUID == renditionSelectionDocument {
		modality = "KO"
	}
	document := &Document{
		SOPClassUID: sopClassUID, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.999.10", Modality: modality,
		StudyInstanceUID: "1.2.826.0.1.3680043.10.543.999.11", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.999.12",
		Title:       oracleRootTitle(sopClassUID),
		ContentDate: "20260926", ContentTime: "120000", CompletionFlag: "COMPLETE", VerificationFlag: "UNVERIFIED",
		Content: content,
	}
	elements, err := document.encodeElements()
	if err != nil {
		t.Fatal(err)
	}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

type oracleTriple struct {
	source       ValueType
	relationship string
	target       ValueType
}

func (triple oracleTriple) String() string {
	return fmt.Sprintf("%s %s %s", triple.source, triple.relationship, triple.target)
}

var cannotAdd = regexp.MustCompile(`Cannot add "([a-z ]+) ([A-Z0-9]+)" to ([A-Z0-9]+) in`)

// dsrdumpRejections reads a file with dsrdump, skipping invalid items, and
// returns the by-value relationships DCMTK refused.
func dsrdumpRejections(t *testing.T, dsrdump, path string) map[oracleTriple]bool {
	t.Helper()
	out, err := dcmtkoracle.Command(context.Background(), t, "same accept/refuse decision per triple (documented divergences excepted)", dsrdump, "-Ei", path).CombinedOutput()
	rejected := map[oracleTriple]bool{}
	for _, match := range cannotAdd.FindAllStringSubmatch(string(out), -1) {
		rejected[oracleTriple{ValueType(match[3]), strings.ToUpper(match[1]), ValueType(match[2])}] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Any other error means an oracle item is invalid, not a relationship.
		if strings.HasPrefix(line, "E: ") && !cannotAdd.MatchString(line) && !strings.Contains(line, "Reading content item") {
			t.Fatalf("dsrdump %s: %s\n%s", filepath.Base(path), line, out)
		}
	}
	if err != nil && len(rejected) == 0 {
		t.Fatalf("dsrdump %s: %v\n%s", filepath.Base(path), err, out)
	}
	return rejected
}

const renditionSelectionDocument = "1.2.840.10008.10.4"

// dcmtkUnsupported are SR SOP Classes DCMTK 3.7 dsrdump does not read.
var dcmtkUnsupported = map[string]bool{
	ExtensibleSRStorage:             true,
	"1.2.840.10008.5.1.4.1.1.88.76": true, // Enhanced X-Ray Radiation Dose SR
}

// dcmtkRelationshipDivergences are triples where DCMTK 3.7's constraint
// checkers differ from the PS3.3 2026d tables; see docs/SR_TEMPLATES_AND_REFERENCES.md.
var dcmtkRelationshipDivergences = func() map[string]map[oracleTriple]bool {
	out := map[string]map[oracleTriple]bool{}
	add := func(iod string, sources []ValueType, relationship string, targets ...ValueType) {
		if out[iod] == nil {
			out[iod] = map[oracleTriple]bool{}
		}
		for _, source := range sources {
			for _, target := range targets {
				out[iod][oracleTriple{source, relationship, target}] = true
			}
		}
	}
	// CP-2595 rows, which the 3.7.0 release refuses; the pinned 8d68392d
	// accepts them, and the entries keep a 3.7.0 build passing.
	add("Key Object Selection Document", []ValueType{ValueContainer}, RelationshipContains, ValueContainer)
	add("Key Object Selection Document", []ValueType{ValueImage, ValueComposite, ValueWaveform}, RelationshipHasAcqContext,
		ValueCode, ValueDate, ValueDateTime, ValueNum, ValueText, ValueTime, ValueUIDRef)
	// DCMTK keeps DATE and TIME targets the current tables no longer list.
	for _, iod := range []string{"X-Ray Radiation Dose SR", "Patient Radiation Dose SR"} {
		add(iod, []ValueType{ValuePName}, RelationshipHasProperties, ValueDate, ValueTime)
	}
	// DCMTK allows only COMPOSITE targets of HAS PROPERTIES.
	add("Implantation Plan SR Document", []ValueType{ValueText, ValueCode, ValueNum, ValueImage, ValueUIDRef, ValueComposite},
		RelationshipHasProperties, ValueText, ValueCode, ValueNum, ValueUIDRef, ValueImage)
	// DCMTK has COMPOSITE where the table has CONTAINER.
	add("Simplified Adult Echo SR", []ValueType{ValueText, ValueCode, ValueNum, ValueContainer}, RelationshipHasObsContext,
		ValueComposite, ValueContainer)
	return out
}()

// TestRelationshipConstraintsMatchDCMTK is opt-in evidence for #999: for every
// SR IOD, every by-value source/relationship/target combination DCMTK dsrdump
// can place is accepted or refused by DCMTK exactly as the PS3.3 table says,
// except for the documented divergences. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestRelationshipConstraintsMatchDCMTK(t *testing.T) {
	dsrdump := requireDSRDump(t)
	for index := range relationshipIODs {
		iod := &relationshipIODs[index]
		t.Run(iod.name, func(t *testing.T) {
			if dcmtkUnsupported[iod.sopClassUID] {
				t.Skip("DCMTK 3.7 does not read this SOP Class")
			}
			dir := t.TempDir()
			var mismatches []string
			known := dcmtkRelationshipDivergences[iod.name]
			compare := func(triple oracleTriple, dcmtk bool) {
				if ours := iod.allows(triple.source, triple.relationship, triple.target); ours != dcmtk && !known[triple] {
					mismatches = append(mismatches, fmt.Sprintf("%s: dicom-go %v, DCMTK %v", triple, ours, dcmtk))
				}
			}
			// Phase 1: the value types the root CONTAINER may contain. A
			// refused child stops DCMTK reading its siblings, so one per file.
			var placeable []ValueType
			for _, valueType := range oracleValueTypes {
				path := filepath.Join(dir, "root-"+string(valueType)+".dcm")
				writeOracleDocument(t, path, iod.sopClassUID, []ContentItem{oracleItem(valueType, RelationshipContains)})
				triple := oracleTriple{ValueContainer, RelationshipContains, valueType}
				accepted := !dsrdumpRejections(t, dsrdump, path)[triple]
				compare(triple, accepted)
				if accepted {
					placeable = append(placeable, valueType)
				}
			}
			// Phase 2: under each placeable source, one child per triple.
			var content []ContentItem
			var triples []oracleTriple
			for _, source := range placeable {
				for _, relationship := range oracleRelationships {
					for _, target := range oracleValueTypes {
						item := oracleItem(source, RelationshipContains)
						item.Children = []ContentItem{oracleItem(target, relationship)}
						content = append(content, item)
						triples = append(triples, oracleTriple{source, relationship, target})
					}
				}
			}
			path := filepath.Join(dir, "triples.dcm")
			writeOracleDocument(t, path, iod.sopClassUID, content)
			rejected := dsrdumpRejections(t, dsrdump, path)
			for _, triple := range triples {
				compare(triple, !rejected[triple])
			}
			sort.Strings(mismatches)
			if len(mismatches) > 0 {
				t.Errorf("%s (Table %s): %d of %d combinations differ from DCMTK:\n%s", iod.name, iod.table, len(mismatches), len(triples)+len(oracleValueTypes), strings.Join(mismatches, "\n"))
			}
			t.Logf("%d root value types placeable, %d combinations compared", len(placeable), len(triples)+len(oracleValueTypes))
		})
	}
}

// TestMKReportDocumentsMatchDSRDump is opt-in evidence for #999 over the DCMTK
// dcmsr/tests/mkreport corpus: every report dsrdump reads (value errors
// aside) has no relationship finding here, and the references resolve except
// for the one deliberate by-reference cycle, which dicom-go refuses.
// Enable with DICOMGO_DCMTK_INTEGRATION=1 and DICOMGO_DCMTK_MKREPORT naming
// an mkreport built from the DCMTK sources.
func TestMKReportDocumentsMatchDSRDump(t *testing.T) {
	dsrdump := requireDSRDump(t)
	mkreport := os.Getenv("DICOMGO_DCMTK_MKREPORT")
	if mkreport == "" {
		t.Skip("set DICOMGO_DCMTK_MKREPORT to an mkreport executable built from DCMTK dcmsr/tests/mkreport.cc")
	}
	dir := t.TempDir()
	cmd := dcmtkoracle.Command(context.Background(), t, "report corpus generator (output compared through dsrdump)", mkreport, "all")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkreport all: %v\n%s", err, out)
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "report*.dcm"))
	if len(paths) < 20 {
		t.Fatalf("mkreport wrote %d reports", len(paths))
	}
	for _, path := range paths {
		name := filepath.Base(path)
		out, dumpErr := dcmtkoracle.Command(context.Background(), t, "dsrdump fails exactly when dicom-go reports a relationship finding", dsrdump, "-Ev", path).CombinedOutput()
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		options := DefaultReadOptions()
		options.ResolveReferences = true
		options.ValidateRelationships = true
		result, err := ReadDocumentWithOptions(file.Dataset, options)
		_ = file.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var findings []string
		for _, finding := range result.Report.Findings {
			if name == "reportlp.dcm" && finding.Code == ReferenceCodeCycle {
				continue
			}
			findings = append(findings, finding.Code+" at "+finding.Path.String())
		}
		if (dumpErr == nil) != (len(findings) == 0) {
			t.Errorf("%s: dsrdump error %v, dicom-go findings %v\n%s", name, dumpErr, findings, out)
		}
		if name == "reportlp.dcm" && (len(result.Report.Findings) == 0 || result.Report.Findings[0].Code != ReferenceCodeCycle) {
			t.Errorf("%s: want the by-reference cycle reported, got %+v", name, result.Report.Findings)
		}
	}
}
