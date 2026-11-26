package dimse

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestBuildRetrieveIdentifierRoundTrip(t *testing.T) {
	const study, series, image = "1.2.3", "1.2.4", "1.2.5"
	for _, tc := range []struct {
		name  string
		model QueryRetrieveModel
		keys  RetrieveIdentifierKeys
	}{
		{"study-root study", QueryRetrieveModelStudyRoot, RetrieveIdentifierKeys{
			Level: " study ", StudyInstanceUID: study,
		}},
		{"study-root series", QueryRetrieveModelStudyRoot, RetrieveIdentifierKeys{
			Level: QueryRetrieveLevelSeries, StudyInstanceUID: study, SeriesInstanceUID: series,
		}},
		{"study-root image", QueryRetrieveModelStudyRoot, RetrieveIdentifierKeys{
			Level: QueryRetrieveLevelImage, StudyInstanceUID: study, SeriesInstanceUID: series, SOPInstanceUID: image,
		}},
		{"patient-root patient", QueryRetrieveModelPatientRoot, RetrieveIdentifierKeys{
			Level: QueryRetrieveLevelPatient, PatientID: "JOSÉ-1",
		}},
		{"patient-root image", QueryRetrieveModelPatientRoot, RetrieveIdentifierKeys{
			Level: QueryRetrieveLevelImage, PatientID: "TEST-2", StudyInstanceUID: study, SeriesInstanceUID: series, SOPInstanceUID: image,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
				t.Run(syntax.UID, func(t *testing.T) {
					for _, operation := range []struct {
						name  string
						build func(QueryRetrieveModel, RetrieveIdentifierKeys) (*object.Object, error)
					}{
						{"GET", BuildCGetIdentifier},
						{"MOVE", BuildCMoveIdentifier},
					} {
						t.Run(operation.name, func(t *testing.T) {
							identifier, err := operation.build(tc.model, tc.keys)
							if err != nil {
								t.Fatal(err)
							}
							var wire bytes.Buffer
							if err := object.WriteDataSet(&wire, identifier, syntax); err != nil {
								t.Fatal(err)
							}
							got, err := object.ReadDataSet(&wire, syntax)
							if err != nil {
								t.Fatal(err)
							}
							// Baseline hierarchical keys are shared by the two operations.
							keys, err := ParseCGetIdentifierKeys(tc.model, got, CRetrieveExtendedNegotiation{})
							if err != nil {
								t.Fatal(err)
							}
							wantLevel := strings.ToUpper(strings.TrimSpace(tc.keys.Level))
							if keys.Level != wantLevel || keys.PatientID != tc.keys.PatientID ||
								!slices.Equal(keys.StudyInstanceUIDs, singleUID(tc.keys.StudyInstanceUID)) ||
								!slices.Equal(keys.SeriesInstanceUIDs, singleUID(tc.keys.SeriesInstanceUID)) ||
								!slices.Equal(keys.SOPInstanceUIDs, singleUID(tc.keys.SOPInstanceUID)) {
								t.Fatalf("parsed keys %+v, want %+v", keys, tc.keys)
							}
							if tc.keys.PatientID == "JOSÉ-1" {
								if charset, _ := got.GetString(getTagSpecificCharacterSet); charset != "ISO_IR 192" {
									t.Fatalf("Specific Character Set = %q", charset)
								}
							}
							if operation.name == "MOVE" {
								levelParser := studyRootCMoveLevel
								if tc.model == QueryRetrieveModelPatientRoot {
									levelParser = patientRootCMoveLevel
								}
								if level, err := levelParser(got); err != nil || level != wantLevel {
									t.Fatalf("MOVE level = %q, %v, want %q", level, err, wantLevel)
								}
							}
						})
					}
				})
			}
		})
	}
}

func singleUID(uid string) []string {
	if uid == "" {
		return nil
	}
	return []string{uid}
}

func TestBuildRetrieveIdentifierRejectsInvalidHierarchy(t *testing.T) {
	const studyRoot = QueryRetrieveModelStudyRoot
	const patientRoot = QueryRetrieveModelPatientRoot
	for _, tc := range []struct {
		name  string
		model QueryRetrieveModel
		keys  RetrieveIdentifierKeys
		want  string
	}{
		{"unsupported model", "UNKNOWN", RetrieveIdentifierKeys{Level: "STUDY", StudyInstanceUID: "1.2.3"}, "model"},
		{"unsupported level", studyRoot, RetrieveIdentifierKeys{Level: "PATIENT", PatientID: "TEST"}, "QueryRetrieveLevel"},
		{"missing study", studyRoot, RetrieveIdentifierKeys{Level: "SERIES", SeriesInstanceUID: "1.2.4"}, "missing StudyInstanceUID"},
		{"missing patient", patientRoot, RetrieveIdentifierKeys{Level: "STUDY", StudyInstanceUID: "1.2.3"}, "missing PatientID"},
		{"missing image", studyRoot, RetrieveIdentifierKeys{Level: "IMAGE", StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.4"}, "missing SOPInstanceUID"},
		{"patient outside model", studyRoot, RetrieveIdentifierKeys{Level: "STUDY", StudyInstanceUID: "1.2.3", PatientID: "TEST"}, "unexpected PatientID"},
		{"UID below level", studyRoot, RetrieveIdentifierKeys{Level: "STUDY", StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.4"}, "unexpected SeriesInstanceUID"},
		{"malformed UID", studyRoot, RetrieveIdentifierKeys{Level: "STUDY", StudyInstanceUID: "1.2.03"}, "invalid StudyInstanceUID"},
		{"multiple UIDs", studyRoot, RetrieveIdentifierKeys{Level: "STUDY", StudyInstanceUID: `1.2.3\1.2.4`}, "invalid StudyInstanceUID"},
		{"multiple patient IDs", patientRoot, RetrieveIdentifierKeys{Level: "PATIENT", PatientID: `A\B`}, "invalid PatientID"},
		{"wildcard patient ID", patientRoot, RetrieveIdentifierKeys{Level: "PATIENT", PatientID: "TEST*"}, "invalid PatientID"},
		{"long patient ID", patientRoot, RetrieveIdentifierKeys{Level: "PATIENT", PatientID: strings.Repeat("É", 65)}, "invalid PatientID"},
		{"invalid UTF-8", patientRoot, RetrieveIdentifierKeys{Level: "PATIENT", PatientID: "\xff"}, "invalid PatientID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, build := range []func(QueryRetrieveModel, RetrieveIdentifierKeys) (*object.Object, error){BuildCGetIdentifier, BuildCMoveIdentifier} {
				if got, err := build(tc.model, tc.keys); got != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("identifier = %v, %v, want error containing %q", got, err, tc.want)
				}
			}
		})
	}
}
