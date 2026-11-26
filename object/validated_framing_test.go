package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// DicomEncodedDataSetValidatorTests checks that a fatal framing or resource
// error withholds the dataset while retaining lexical findings from earlier
// complete elements.
func TestValidationDiagnosticsSurviveMalformedDataSet(t *testing.T) {
	modality := core.NewTag(0x0008, 0x0060)
	patientID := core.NewTag(0x0010, 0x0020)
	const badCS = "BAD!MODALITY"
	for _, syntax := range []transfer.Syntax{
		transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian,
		transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian,
	} {
		t.Run(syntax.Name, func(t *testing.T) {
			wireSyntax := syntax
			if syntax.Deflated {
				wireSyntax = transfer.ExplicitVRLittleEndian
			}
			prefix := dicomtest.EncodeElement(core.NewRawElement(modality, core.VRCS, []byte(badCS)), wireSyntax)
			second := dicomtest.EncodeElement(core.NewRawElement(patientID, core.VRLO, []byte("SYNTHETIC-62")), wireSyntax)
			for _, scenario := range []struct {
				name        string
				secondBytes int
				maxElements int
				op          parser.Op
				cause       error
			}{
				{"complete", len(second), 0, "", validation.ErrValidationFailed},
				{"tag incomplete", 3, 0, parser.OpReadTag, io.ErrUnexpectedEOF},
				{"value incomplete", len(second) - 2, 0, parser.OpReadValue, io.ErrUnexpectedEOF},
				{"element budget exceeded", len(second), 1, parser.OpCheckElementCount, parser.ErrMaxElementsExceeded},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					wire := append(bytes.Clone(prefix), second[:scenario.secondBytes]...)
					if syntax.Deflated {
						wire = deflateBytes(t, wire)
						if len(wire)%2 != 0 {
							wire = append(wire, 0)
						}
					}
					dataset, report, err := ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
						ReadFileOptions{MaxElements: scenario.maxElements}, validation.Options{Mode: validation.ModeStrict, MaxFindings: 1})
					if !errors.Is(err, scenario.cause) {
						t.Fatalf("read error = %v, want %v", err, scenario.cause)
					}
					if scenario.op != "" {
						var framing *parser.ParseError
						if dataset != nil || !errors.As(err, &framing) || framing.Op != scenario.op {
							t.Fatalf("framing failure returned a dataset or unexpected parse context: dataset=%t parse error=%v", dataset != nil, err)
						}
						if framing.Offset < int64(len(prefix)) || strings.Contains(err.Error(), badCS) || strings.Contains(err.Error(), "SYNTHETIC-62") {
							t.Fatalf("framing error has invalid logical offset or patient value: %v", err)
						}
						if scenario.op != parser.OpReadTag && framing.Tag != patientID {
							t.Fatalf("framing error refers to %s, want %s", framing.Tag, patientID)
						}
					} else if dataset == nil || !dataset.Has(patientID) {
						t.Fatal("complete dataset should remain available for lexical inspection")
					}
					if !report.HasErrors() || len(report.Findings) != 1 {
						t.Fatalf("expected one retained lexical error for Modality, got %+v", report)
					}
					finding := report.Findings[0]
					if finding.Tag != modality || finding.Code != validation.CodeValueFormat || finding.Severity != validation.SeverityError ||
						!finding.OffsetSet || finding.Offset != 0 ||
						finding.Path.String() != modality.String() || strings.Contains(finding.Message, badCS) {
						t.Fatalf("prior lexical finding lost source context or exposed its value: %+v", finding)
					}
				})
			}
		})
	}
}

func TestValidationDiagnosticsSurviveMalformedNestedItem(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	seqTag := core.NewTag(0x0008, 0x1111)
	modality := core.NewTag(0x0008, 0x0060)
	wire := dicomtest.SequenceHeaderBytes(syntax, seqTag, uint32(core.UndefinedLength))
	wire = append(wire, dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagItem, uint32(core.UndefinedLength))...)
	valueOffset := int64(len(wire))
	wire = append(wire, dicomtest.EncodeElement(core.NewRawElement(modality, core.VRCS, []byte("BAD!")), syntax)...)
	wire = append(wire, 0x10, 0x00, 0x20) // interrupted second tag in the item

	completionCalled := false
	hooks, err := validation.NewHookChain(validation.HookRegistration{
		Name:   "observe-completion",
		Points: []validation.HookPoint{validation.HookItemComplete, validation.HookSequenceComplete, validation.HookDataSetComplete},
		Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
			completionCalled = true
			return validation.HookDecision{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	dataset, report, err := ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
		ReadFileOptions{}, validation.Options{Hooks: hooks, Mode: validation.ModeStrict})
	if dataset != nil || !errors.Is(err, io.ErrUnexpectedEOF) || completionCalled {
		t.Fatalf("dataset=%v err=%v completion called=%v", dataset, err, completionCalled)
	}
	wantPath := validation.Path{{Tag: seqTag, ItemIndex: 0}, {Tag: modality, ItemIndex: validation.NoItem}}
	for _, finding := range report.Findings {
		if finding.Code == validation.CodeValueFormat && finding.Path.String() == wantPath.String() && finding.OffsetSet && finding.Offset == valueOffset {
			return
		}
	}
	t.Fatalf("missing lexical finding for %s at %d: %+v", wantPath, valueOffset, report)
}
