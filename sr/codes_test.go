package sr

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/codes"
)

// TestConceptsAreThoseOfPS316: every concept and unit the writer emits is
// the PS3.16 term, meaning included. The angle measurement used (121207,
// DCM), which PS3.16 defines as Height.
func TestConceptsAreThoseOfPS316(t *testing.T) {
	entries := []CodedEntry{
		imagingMeasurementReportConcept, languageConcept, observerTypeConcept, personObserverType, deviceObserverType,
		personObserverNameConcept, personObserverOrganizationConcept, deviceObserverUIDConcept, deviceObserverNameConcept,
		deviceObserverManufacturerConcept, deviceObserverModelConcept, deviceObserverSerialConcept, procedureReportedConcept,
		imageLibraryConcept, imageLibraryGroupConcept, imagingMeasurementsConcept, derivedImagingMeasurementsConcept,
		qualitativeEvaluationsConcept, measurementGroupConcept, trackingIdentifierConcept, trackingUniqueIdentifierConcept,
		findingConcept, findingSiteConcept, referencedSegmentConcept, sourceSeriesConcept, sourceOfMeasurementConcept,
		derivationConcept, DefaultProcedureReported, KOSTitleOfInterest, KOSFindingConcept,
		MillimeterUnit(), DegreeUnit(), SquareMillimeterUnit(),
	}
	for _, entry := range entries {
		term, ok := codes.Lookup(entry.CodingSchemeDesignator, entry.CodeValue)
		if !ok || term.Retired || term.Meaning != entry.CodeMeaning {
			t.Errorf("%+v: PS3.16 has %+v (found %v)", entry, term, ok)
		}
	}
	// Measurement concepts take the meanings of their Context Group.
	inGroups := map[int][]CodedEntry{cidLinearMeasurement: {LongAxisConcept(), ShortAxisConcept()}}
	for _, kind := range []MeasurementKind{MeasurementKindLength, MeasurementKindArea} {
		concept, _, _ := defaultConceptAndUnits(kind)
		id := cidLinearMeasurement
		if kind == MeasurementKindArea {
			id = cidAreaMeasurement
		}
		inGroups[id] = append(inGroups[id], concept)
	}
	for id, entries := range inGroups {
		group, _ := codes.ContextGroupByID(id)
		for _, entry := range entries {
			term, ok := group.Member(codes.Code{Value: entry.CodeValue, Scheme: entry.CodingSchemeDesignator})
			if !ok || term.Meaning != entry.CodeMeaning {
				t.Errorf("%+v: CID %d has %+v (found %v)", entry, id, term, ok)
			}
		}
	}
	if angle, _, _ := defaultConceptAndUnits(MeasurementKindAngle); angle.CodeValue != "1483009" || angle.CodingSchemeDesignator != "SCT" || angle.CodeMeaning != "Angle" {
		t.Fatalf("angle concept %+v", angle)
	}
}
