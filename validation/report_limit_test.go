package validation_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestValidationErrorVerdictSurvivesReportLimit(t *testing.T) {
	dataset := core.DataSet{Elements: []core.Element{
		textElement(tagPatientID, core.VRLO, "ID"),
		textElement(tagSOPInstanceUID, core.VRUI, "1..2"),
	}}
	for _, tc := range []struct {
		name string
		mode validation.Mode
	}{
		{"strict", validation.ModeStrict},
		{"preserve", validation.ModePreserve},
		{"warn", validation.ModeWarn},
	} {
		for _, limit := range []int{1, 2} {
			t.Run(tc.name+"/"+strconv.Itoa(limit), func(t *testing.T) {
				result, err := validation.ValidateDataSet(context.Background(), dataset, validation.Options{Mode: tc.mode, MaxFindings: limit})
				report := result.Report
				if want := tc.mode == validation.ModeStrict; errors.Is(err, validation.ErrValidationFailed) != want || err != nil && !want {
					t.Fatalf("ValidateDataSet error = %v, want strict rejection = %t", err, want)
				}
				if len(report.Findings) != limit || report.Findings[0].Code != validation.CodeElementOrder ||
					report.Truncated != (limit == 1) || report.Dropped != 2-limit {
					t.Fatalf("bounded report = %+v", report)
				}
				wantErrors := tc.mode != validation.ModeWarn
				if report.HasErrors() != wantErrors || report.Clone().HasErrors() != wantErrors {
					t.Fatalf("report error verdict = %t, want %t", report.HasErrors(), wantErrors)
				}
				merged, err := validation.MergeReports(validation.Options{MaxFindings: 1}, report)
				if err != nil || merged.HasErrors() != wantErrors {
					t.Fatalf("preserving merge = %+v, %v", merged, err)
				}
				warnings, err := validation.MergeReports(validation.Options{Mode: validation.ModeWarn, MaxFindings: 1}, report)
				if err != nil || warnings.HasErrors() || !warnings.Truncated || warnings.Dropped != 1 {
					t.Fatalf("warning merge = %+v, %v", warnings, err)
				}
			})
		}
	}
}

func TestPart10IdentityErrorSurvivesReportLimit(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	meta := core.DataSet{Elements: []core.Element{
		textElement(tagMediaStorageSOPClass, core.VRUI, "1.22"),
		textElement(tagMediaStorageSOPInstance, core.VRUI, "1.33"),
		core.NewRawElement(tagTransferSyntaxUID, core.VRUI, append([]byte(syntax.UID), 0)),
	}}
	dataset := core.DataSet{Elements: []core.Element{
		textElement(tagPatientID, core.VRLO, "ID"),
		textElement(tagSOPClassUID, core.VRUI, "1.22"),
		textElement(tagSOPInstanceUID, core.VRUI, "1.34"),
	}}
	report, err := validation.ValidateFile(context.Background(), meta, dataset, syntax,
		validation.Options{Mode: validation.ModeStrict, MaxFindings: 1})
	if !errors.Is(err, validation.ErrValidationFailed) || !report.HasErrors() ||
		!report.Truncated || report.Dropped != 1 || len(report.Findings) != 1 ||
		report.Findings[0].Code != validation.CodeElementOrder {
		t.Fatalf("bounded File Meta validation = %+v, %v", report, err)
	}
}

func TestStrictReadAndWriteApplyErrorsBeyondFindingsLimit(t *testing.T) {
	ctx := context.Background()
	hooks, err := validation.NewHookChain(validation.HookRegistration{
		Name: "preflight-notice", Points: []validation.HookPoint{validation.HookPreValidation},
		Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
			return validation.HookDecision{Findings: []validation.Finding{
				{Severity: validation.SeverityWarning},
				{Severity: validation.SeverityWarning},
			}}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := validation.Options{
		Mode: validation.ModeStrict, MaxFindings: 1, Hooks: hooks,
		DataSetRules: []validation.DataSetRuleRegistration{{
			Name: "instance-policy",
			Rule: validation.DataSetRuleFunc(func(_ context.Context, dataset validation.DataSetContext) []validation.Finding {
				for _, element := range dataset.DataSet.Elements {
					if element.Tag() == tagSOPInstanceUID && element.StringValue() == "1.34" {
						return []validation.Finding{{Tag: tagSOPInstanceUID, Severity: validation.SeverityError}}
					}
				}
				return nil
			}),
		}},
	}
	syntax := transfer.ExplicitVRLittleEndian
	for _, tc := range []struct {
		name string
		uid  string
		fail bool
	}{
		{"refused_uid", "1.34", true},
		{"warnings_only", "1.22", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			element := textElement(tagSOPInstanceUID, core.VRUI, tc.uid)
			var source bytes.Buffer
			if err := parser.NewWriter(&source, syntax).WriteElement(element); err != nil {
				t.Fatal(err)
			}
			assertBounded := func(t *testing.T, report validation.Report, err error) {
				t.Helper()
				if errors.Is(err, validation.ErrValidationFailed) != tc.fail || err != nil && !tc.fail {
					t.Fatalf("validation error = %v, want strict rejection=%t", err, tc.fail)
				}
				wantDropped := 1
				if tc.fail {
					wantDropped++
				}
				if !report.Truncated || len(report.Findings) != 1 || report.Dropped != wantDropped ||
					report.Findings[0].Severity != validation.SeverityWarning || !report.FindingsDropped || report.ErrorsDropped != tc.fail ||
					report.HasErrors() != tc.fail {
					t.Fatalf("bounded report = %+v, expected dropped=%d and errors=%t", report, wantDropped, tc.fail)
				}
				if tc.fail {
					var rejected *validation.ValidationError
					if !errors.As(err, &rejected) || !rejected.Report.ErrorsDropped || len(rejected.Report.Findings) != 1 {
						t.Fatalf("typed rejection did not carry bounded verdict: %v", err)
					}
				}
			}

			t.Run("read", func(t *testing.T) {
				obj, report, err := object.ReadDataSetWithValidation(ctx, bytes.NewReader(source.Bytes()), syntax, object.ReadFileOptions{}, opts)
				assertBounded(t, report, err)
				if obj == nil {
					t.Fatal("validated read discarded the inspected object")
				}
				if got, ok := obj.GetString(tagSOPInstanceUID); !ok || got != tc.uid {
					t.Fatalf("read value = %q, present=%t, want %q", got, ok, tc.uid)
				}
			})
			t.Run("write", func(t *testing.T) {
				var output bytes.Buffer
				input := object.FromElements([]core.Element{element}, nil)
				result, err := object.WriteDataSetWithValidation(ctx, &output, input, syntax, opts)
				assertBounded(t, result.Report, err)
				if tc.fail {
					if result.Complete || result.BytesWritten != 0 || output.Len() != 0 {
						t.Fatalf("strict write published output: complete=%t, accepted=%d, output=%d", result.Complete, result.BytesWritten, output.Len())
					}
				} else if !result.Complete || result.BytesWritten != int64(source.Len()) || !bytes.Equal(output.Bytes(), source.Bytes()) {
					t.Fatalf("warning-only write did not preserve bytes: complete=%t, accepted=%d, output=%d", result.Complete, result.BytesWritten, output.Len())
				}
			})
		})
	}
}

func TestStopFirstHonorsFindingOmittedBehindHookChange(t *testing.T) {
	first := textElement(tagSOPClassUID, core.VRUI, "1.22")
	second := textElement(tagSOPInstanceUID, core.VRUI, "1..2")
	replacement := textElement(tagSOPClassUID, core.VRUI, "1.33")
	calls := 0
	hooks, err := validation.NewHookChain(validation.HookRegistration{
		Name: "normalize-class", Points: []validation.HookPoint{validation.HookAfterElement},
		Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
			calls++
			if event.Element.Tag() == first.Tag() {
				return validation.HookDecision{Element: &replacement}, nil
			}
			return validation.HookDecision{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: []core.Element{first, second}},
		validation.Options{Mode: validation.ModeStrict, StopFirst: true, MaxFindings: 1, Hooks: hooks})
	if err != nil || calls != 1 || result.Report.HasErrors() ||
		len(result.Report.Changes) != 1 || len(result.Report.Findings) != 0 || !result.Report.Truncated || result.Report.Dropped != 1 {
		t.Fatalf("stop-first result = %+v, error=%v, hook calls=%d", result.Report, err, calls)
	}
}
