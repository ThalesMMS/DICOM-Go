package validation_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// PS3.5 Table 6.2-1 defines lexical forms, padding, and character limits.
// Exercise the validated reader with encoded values as well as the ordinary
// reader and writer so reporting never changes the supplied bytes.
func TestValidatedTextWirePreservesRejectedValues(t *testing.T) {
	tag := core.NewTag(0x0011, 0x4010)
	tests := []struct {
		name string
		vr   core.VR
		text string
		code validation.Code
	}{
		{"AE over maximum", core.VRAE, strings.Repeat("A", 17), validation.CodeValueLength},
		{"AS bad unit", core.VRAS, "013H", validation.CodeValueFormat},
		{"CS internal tab", core.VRCS, "CT\tAXIAL", validation.CodeValueRepertoire},
		{"DA invalid leap day", core.VRDA, "21000229", validation.CodeValueRange},
		{"DA retired notation", core.VRDA, "2000.02.29", validation.CodeValueFormat},
		{"DS incomplete exponent", core.VRDS, "-1.4E+", validation.CodeValueFormat},
		{"DS interior space", core.VRDS, "+1 25", validation.CodeValueFormat},
		{"IS negative underflow", core.VRIS, "-2147483649", validation.CodeValueRange},
		{"IS fraction", core.VRIS, "+7.0", validation.CodeValueFormat},
		{"DT timezone minute overflow", core.VRDT, "20000229232324-0961", validation.CodeValueRange},
		{"DT fraction before seconds", core.VRDT, "200002292323.1", validation.CodeValueRange},
		{"LO internal NUL", core.VRLO, "AB\x00C", validation.CodeValueRepertoire},
		{"LT over maximum", core.VRLT, strings.Repeat("a", 10241), validation.CodeValueLength},
		{"ST over maximum", core.VRST, strings.Repeat("b", 1025), validation.CodeValueLength},
		{"TM midnight overflow", core.VRTM, "240000", validation.CodeValueRange},
		{"TM beyond microseconds", core.VRTM, "123456.0000001", validation.CodeValueRange},
		{"UC internal newline", core.VRUC, "AB\nCD", validation.CodeValueRepertoire},
		{"UI padded with space", core.VRUI, "2.25.78 ", validation.CodeValuePadding},
		{"UI interior NUL", core.VRUI, "2.25\x00.78", validation.CodeValueRepertoire},
		{"UR non-hex escape", core.VRUR, "http://example.test/%g0", validation.CodeValueFormat},
		{"UT internal DEL", core.VRUT, "LINE\x7fLINE", validation.CodeValueRepertoire},
		{"PN fourth component group", core.VRPN, "A=B=C=G", validation.CodeValueFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			syntax := transfer.ExplicitVRLittleEndian
			raw := []byte(tc.text)
			if len(raw)%2 != 0 {
				raw = append(raw, tc.vr.PaddingByte())
			}
			wire := dicomtest.EncodeElements(syntax, core.NewRawElement(tag, tc.vr, raw))
			for _, policy := range []struct {
				name     string
				mode     validation.Mode
				severity validation.Severity
			}{
				{"strict", validation.ModeStrict, validation.SeverityError},
				{"warn", validation.ModeWarn, validation.SeverityWarning},
			} {
				t.Run(policy.name, func(t *testing.T) {
					obj, report, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
						object.ReadFileOptions{}, validation.Options{Mode: policy.mode})
					var finding *validation.Finding
					for i := range report.Findings {
						if report.Findings[i].Code == tc.code {
							finding = &report.Findings[i]
							break
						}
					}
					if finding == nil || finding.Tag != tag || finding.VR != tc.vr || finding.Severity != policy.severity {
						t.Fatalf("report = %#v, want %s %s for %s %s", report, policy.severity, tc.code, tag, tc.vr)
					}
					if policy.mode == validation.ModeStrict {
						if !errors.Is(err, validation.ErrValidationFailed) {
							t.Fatalf("strict error = %v, want ErrValidationFailed", err)
						}
						return
					}
					if err != nil || obj == nil {
						t.Fatalf("warn reader returned object %v, error %v", obj, err)
					}
					element, ok := obj.Get(tag)
					value, isRaw := element.Value.(core.RawValue)
					if !ok || element.VR() != tc.vr || !isRaw || !bytes.Equal(value.Bytes(), raw) {
						t.Fatalf("retained element VR %s, value type %T; want raw %s value of %d bytes", element.VR(), element.Value, tc.vr, len(raw))
					}
					var rewritten bytes.Buffer
					if err := object.WriteDataSet(&rewritten, obj, syntax); err != nil || !bytes.Equal(rewritten.Bytes(), wire) {
						t.Fatalf("ordinary writer changed the retained wire bytes (length %d, want %d), error %v", rewritten.Len(), len(wire), err)
					}
				})
			}
		})
	}
}

func TestValidatedTextWireRetainsValidBoundariesAndEmptyComponents(t *testing.T) {
	tag := core.NewTag(0x0011, 0x4010)
	tests := []struct {
		name   string
		vr     core.VR
		values []string
	}{
		{"AE maximum", core.VRAE, []string{"REMOTE_ARCHIVE_2"}},
		{"CS maximum", core.VRCS, []string{"HIGH_RESOLUTION1"}},
		{"DA Gregorian century leap day", core.VRDA, []string{"20000229"}},
		{"DS signed decimal and empty member", core.VRDS, []string{"-.25E+06", "", "+4."}},
		{"IS signed extremes and empty member", core.VRIS, []string{"-2147483648", "", "+2147483647"}},
		{"DT partial precision with zone", core.VRDT, []string{"200002-1200", "20000229235960.7+1400"}},
		{"TM hour and microsecond precision", core.VRTM, []string{"07", "075931.230004", "235960"}},
		{"UT significant leading space and line controls", core.VRUT, []string{" first\r\nsecond\tline\fend"}},
		{"ST literal backslash", core.VRST, []string{`left\right`}},
		{"LT maximum", core.VRLT, []string{strings.Repeat("L", 10240)}},
		{"ST maximum", core.VRST, []string{strings.Repeat("s", 1024)}},
		{"PN empty interior group and component", core.VRPN, []string{"AXEL^^R^^JR==ERIK^S", "", "LEE^MIN"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			syntax := transfer.ExplicitVRLittleEndian
			obj := object.FromElements([]core.Element{textElement(tag, tc.vr, tc.values...)}, nil)
			var wire bytes.Buffer
			written, err := object.WriteDataSetWithValidation(context.Background(), &wire, obj, syntax, validation.Options{Mode: validation.ModeStrict})
			if err != nil || !written.Complete || written.BytesWritten != int64(wire.Len()) || len(written.Report.Findings) != 0 {
				t.Fatalf("validated writer = %#v, error %v", written, err)
			}
			decoded, report, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire.Bytes()), syntax,
				object.ReadFileOptions{}, validation.Options{Mode: validation.ModeStrict})
			if err != nil || len(report.Findings) != 0 {
				t.Fatalf("validated reader report = %#v, error %v", report, err)
			}
			got, err := decoded.LookupStrings(tag)
			if err != nil || !reflect.DeepEqual(got, tc.values) {
				t.Fatalf("decoded %d values, want %d with exact text and components, error %v", len(got), len(tc.values), err)
			}
		})
	}
}

func TestValidatedTextWireLocatesNestedValueInSecondSibling(t *testing.T) {
	sequenceTag := core.NewTag(0x0040, 0xa730) // Content Sequence
	valueTag := core.NewTag(0x0040, 0xa040)    // Value Type
	const malformed = "INVALID!"
	makeItem := func(value string) core.DataSet {
		return core.DataSet{Elements: []core.Element{textElement(valueTag, core.VRCS, value)}}
	}
	inner := dicomtest.NewSequenceElement(sequenceTag, makeItem("TEXT"), makeItem(malformed))
	outer := dicomtest.NewSequenceElement(sequenceTag, makeItem("CONTAINER"), core.DataSet{Elements: []core.Element{inner}})
	wantPath := validation.Path{{Tag: sequenceTag, ItemIndex: 1}, {Tag: sequenceTag, ItemIndex: 1}, {Tag: valueTag, ItemIndex: validation.NoItem}}

	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			wire := dicomtest.EncodeElements(syntax, outer)
			// Encode the child separately to identify its exact element boundary.
			marker := dicomtest.EncodeElement(makeItem(malformed).Elements[0], syntax)
			index := bytes.Index(wire, marker)
			if index < 0 || bytes.Count(wire, marker) != 1 {
				t.Fatal("fixture must contain one identifiable malformed child")
			}
			_, warned, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
				object.ReadFileOptions{Dictionary: std.Dictionary}, validation.Options{Mode: validation.ModeWarn})
			if err != nil || len(warned.Findings) != 1 {
				t.Fatalf("warn report = %#v, error %v", warned, err)
			}
			finding := warned.Findings[0]
			if finding.Tag != valueTag || finding.Code != validation.CodeValueFormat || finding.VR != core.VRCS ||
				finding.Severity != validation.SeverityWarning || !reflect.DeepEqual(finding.Path, wantPath) ||
				!finding.OffsetSet || finding.Offset != int64(index) || strings.Contains(finding.Message, malformed) {
				t.Fatalf("warn finding = %#v, want path %s and header offset %d", finding, wantPath, index)
			}
			_, strict, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
				object.ReadFileOptions{Dictionary: std.Dictionary}, validation.Options{Mode: validation.ModeStrict})
			finding.Severity = validation.SeverityError
			if !errors.Is(err, validation.ErrValidationFailed) || len(strict.Findings) != 1 || !reflect.DeepEqual(strict.Findings[0], finding) {
				t.Fatalf("strict report = %#v, error %v, want the same finding at error severity", strict, err)
			}
		})
	}
}
