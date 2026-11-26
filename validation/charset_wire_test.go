package validation_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// Independently chosen characters use the byte assignments of the declared
// repertoires (PS3.5 6.1.2). Checking the resulting element bytes catches a
// read/write pair that would otherwise agree on the same wrong encoding.
func TestMetadataCharacterSetWireRoundTrip(t *testing.T) {
	nameTag := core.NewTag(0x0010, 0x0010)
	descriptionTag := core.NewTag(0x0008, 0x1030)
	tests := []struct {
		charset            string
		name               string
		encodedName        string
		description        string
		encodedDescription string
	}{
		{"ISO_IR 6", "West^Ari", "West^Ari", "Hand", "Hand"},
		{"ISO_IR 100", "Héloïse^Maël", "H\xe9lo\xefse^Ma\xebl", "Épaule", "\xc9paule"},
		{"ISO_IR 110", "Ābele^Ēva", "\xc0bele^\xaava", "Ziemeļi", "Zieme\xb6i"},
		{"ISO_IR 126", "Δέλτα^Ζήτα", "\xc4\xdd\xeb\xf4\xe1^\xc6\xde\xf4\xe1", "Ώμος", "\xbf\xec\xef\xf2"},
		{"ISO_IR 127", "نور^علي", "\xe6\xe8\xd1^\xd9\xe4\xea", "يد", "\xea\xcf"},
		{"ISO_IR 138", "אור^דן", "\xe0\xe5\xf8^\xe3\xef", "יד", "\xe9\xe3"},
		{"ISO_IR 144", "Орлов^Ян", "\xbe\xe0\xdb\xde\xd2^\xcf\xdd", "Кисть", "\xba\xd8\xe1\xe2\xec"},
		{"ISO_IR 148", "Işık^İnci", "I\xfe\xfdk^\xddnci", "Göğüs", "G\xf6\xf0\xfcs"},
		{"ISO_IR 166", "ทอง^แก้ว", "\xb7\xcd\xa7^\xe1\xa1\xe9\xc7", "มือ", "\xc1\xd7\xcd"},
		{"GB18030", "Liu^Xia=刘^夏", "Liu^Xia=\xc1\xf5^\xcf\xc4", "肺𠂇", "\xb7\xce\x95\x32\x90\x31"},
		{"ISO_IR 192", "Kato^Aki=加藤^秋=カトウ^アキ", "Kato^Aki=加藤^秋=カトウ^アキ", "上腕", "上腕"},
	}
	for _, tc := range tests {
		for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
			t.Run(tc.charset+"/"+syntax.Name, func(t *testing.T) {
				elements := []core.Element{
					textElement(tagSpecificCharacterSet, core.VRCS, tc.charset),
					textElement(descriptionTag, core.VRLO, tc.description),
					textElement(nameTag, core.VRPN, tc.name),
				}
				file := &object.File{Dataset: object.FromElements(elements, std.Dictionary), TransferSyntax: syntax}
				file.Dataset.Put(textElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"))
				file.Dataset.Put(textElement(core.NewTag(0x0008, 0x0018), core.VRUI, "2.25.8591"))
				var wire bytes.Buffer
				if err := object.WriteFile(&wire, file); err != nil {
					t.Fatal(err)
				}
				read, err := object.ReadFile(bytes.NewReader(wire.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				defer read.Close()
				if read.TransferSyntax.UID != syntax.UID {
					t.Fatalf("file syntax = %s, want %s", read.TransferSyntax.UID, syntax.UID)
				}
				assertEncodedText(t, read.Dataset, descriptionTag, core.VRLO, tc.encodedDescription)
				assertEncodedText(t, read.Dataset, nameTag, core.VRPN, tc.encodedName)
				gotPN, ok := read.GetPersonName(nameTag)
				if !ok || gotPN.ToDICOMString() != tc.name {
					t.Fatalf("structured PN = %q, present %v, want %q", gotPN.ToDICOMString(), ok, tc.name)
				}
				if got, err := read.Dataset.LookupString(descriptionTag); err != nil || got != tc.description {
					t.Fatalf("description = %q, error %v, want %q", got, err, tc.description)
				}
				report, err := read.ValidateFile(context.Background(), validation.Options{Mode: validation.ModeStrict})
				if err != nil || len(report.Findings) != 0 {
					t.Fatalf("file validation report = %#v, error %v", report, err)
				}
			})
		}
	}
}

func assertEncodedText(t *testing.T, obj *object.Object, tag core.Tag, vr core.VR, want string) {
	t.Helper()
	element, ok := obj.Get(tag)
	raw, isRaw := element.Value.(core.RawValue)
	if wantBytes := vr.PadToEvenLength([]byte(want)); !ok || element.VR() != vr || !isRaw || !bytes.Equal(raw.Bytes(), wantBytes) {
		t.Fatalf("element %s = %s % X, want %s % X", tag, element.VR(), raw.Bytes(), vr, wantBytes)
	}
}

func TestJISRomanTextWireDistinguishesYenByVR(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		for _, spec := range []struct {
			tag core.Tag
			vr  core.VR
		}{
			{core.NewTag(0x0010, 0x4000), core.VRLT}, // Patient Comments
			{core.NewTag(0x0008, 0x2111), core.VRST}, // Derivation Description
			{core.NewTag(0x0040, 0xa160), core.VRUT}, // Text Value
		} {
			t.Run(syntax.Name+"/"+spec.vr.String(), func(t *testing.T) {
				const text = "Fee ¥25"
				obj := object.FromElements([]core.Element{
					textElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 13"),
					textElement(spec.tag, spec.vr, text),
				}, std.Dictionary)
				var wire bytes.Buffer
				if _, err := object.WriteDataSetWithValidation(context.Background(), &wire, obj, syntax, validation.Options{Mode: validation.ModeStrict}); err != nil {
					t.Fatal(err)
				}
				decoded, report, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire.Bytes()), syntax,
					object.ReadFileOptions{Dictionary: std.Dictionary}, validation.Options{Mode: validation.ModeStrict})
				if err != nil || len(report.Findings) != 0 {
					t.Fatalf("read report = %#v, error %v", report, err)
				}
				assertEncodedText(t, decoded, spec.tag, spec.vr, `Fee \25`)
				if got, err := decoded.LookupString(spec.tag); got != text || err != nil {
					t.Fatalf("read text = %q, error %v, want %q", got, err, text)
				}
			})
		}
	}
}

func TestValidatedUTF8WireCountsCodePoints(t *testing.T) {
	for _, spec := range []struct {
		vr     core.VR
		max    int
		prefix string
	}{
		{core.VRSH, 16, ""},
		{core.VRLO, 64, ""},
		{core.VRPN, 64, "LEE^JIN="},
	} {
		for _, sample := range []struct {
			name string
			unit string
			size int
		}{
			{"precomposed", "é", 1},
			{"base plus combining mark", "e\u0301", 2},
		} {
			for _, over := range []int{0, 1} {
				name := spec.vr.String() + "/" + sample.name + "/increments-over-limit=" + strconv.Itoa(over)
				t.Run(name, func(t *testing.T) {
					tag := core.NewTag(0x0011, 0x4010)
					text := spec.prefix + strings.Repeat(sample.unit, spec.max/sample.size+over)
					syntax := transfer.ExplicitVRLittleEndian
					wire := dicomtest.EncodeElements(syntax,
						textElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 192"),
						core.NewRawElement(tag, spec.vr, spec.vr.PadToEvenLength([]byte(text))))
					decoded, report, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
						object.ReadFileOptions{}, validation.Options{Mode: validation.ModeStrict})
					if over == 1 {
						if !errors.Is(err, validation.ErrValidationFailed) || len(report.Findings) != 1 ||
							report.Findings[0].Code != validation.CodeValueLength || report.Findings[0].Tag != tag {
							t.Fatalf("over-limit report = %#v, error %v, want one length finding", report, err)
						}
						return
					}
					if err != nil || len(report.Findings) != 0 {
						t.Fatalf("at-limit report = %#v, error %v", report, err)
					}
					if got, err := decoded.LookupString(tag); got != text || err != nil {
						t.Fatalf("decoded text = %q, error %v, want %q", got, err, text)
					}
				})
			}
		}
	}
}

func TestMultibyteTextWireDoesNotSplitTrailByteAsDelimiter(t *testing.T) {
	tag := core.NewTag(0x0011, 0x4010)
	syntax := transfer.ExplicitVRLittleEndian
	// GBK and GB18030 encode 乗 and 俓 using 0x5c as a trail byte.
	// PS3.5 6.1.2.3 treats only the separately encoded 0x5c as a separator.
	for _, charset := range []string{"GBK", "GB18030"} {
		for _, tc := range []struct {
			vr      core.VR
			encoded string
			values  []string
		}{
			{core.VRLO, "\x81\x5c\\\x82\x5c", []string{"乗", "俓"}},
			{core.VRPN, "X=\x81\x5c\\Y=\x82\x5c", []string{"X=乗", "Y=俓"}},
		} {
			t.Run(charset+"/"+tc.vr.String(), func(t *testing.T) {
				wire := dicomtest.EncodeElements(syntax,
					textElement(tagSpecificCharacterSet, core.VRCS, charset),
					core.NewRawElement(tag, tc.vr, tc.vr.PadToEvenLength([]byte(tc.encoded))))
				decoded, report, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
					object.ReadFileOptions{}, validation.Options{Mode: validation.ModeStrict})
				if err != nil || len(report.Findings) != 0 {
					t.Fatalf("read report = %#v, error %v", report, err)
				}
				if got, err := decoded.LookupStrings(tag); err != nil || !slices.Equal(got, tc.values) {
					t.Fatalf("decoded = %q, error %v, want %q", got, err, tc.values)
				}
				decoded.Put(textElement(tag, tc.vr, tc.values...))
				var rewritten bytes.Buffer
				if err := object.WriteDataSet(&rewritten, decoded, syntax); err != nil || !bytes.Equal(rewritten.Bytes(), wire) {
					t.Fatalf("write = % X, want % X, error %v", rewritten.Bytes(), wire, err)
				}
			})
		}
	}
}

func TestAuthoredMetadataRejectsUnrepresentableText(t *testing.T) {
	for _, tc := range []struct {
		charset string
		text    string
	}{
		{"ISO_IR 6", "Róża"},
		{"ISO_IR 100", "初診"},
		{"ISO_IR 144", "ΑΒΓ"},
		{"ISO_IR 126", "Первый"},
		{"ISO_IR 138", "مرحبا"},
		{"ISO_IR 13", "診療"},
	} {
		t.Run(tc.charset, func(t *testing.T) {
			obj := object.FromElements([]core.Element{
				textElement(tagSpecificCharacterSet, core.VRCS, tc.charset),
				textElement(core.NewTag(0x0008, 0x1030), core.VRLO, "SYNTHETIC"),
				textElement(core.NewTag(0x0010, 0x0010), core.VRPN, tc.text),
			}, std.Dictionary)
			var destination bytes.Buffer
			if err := object.WriteDataSet(&destination, obj, transfer.ExplicitVRLittleEndian); !errors.Is(err, dicomenc.ErrUnrepresentableCharacter) {
				t.Fatalf("write error = %v, want ErrUnrepresentableCharacter", err)
			}
			if bytes.Contains(destination.Bytes(), []byte(tc.text)) {
				t.Fatal("rejected name was written in UTF-8")
			}
		})
	}
}

func TestUnsupportedOrUndecodableMetadataRetainsOriginalOctets(t *testing.T) {
	nameTag := core.NewTag(0x0010, 0x0010)
	syntax := transfer.ExplicitVRLittleEndian
	for _, tc := range []struct {
		name         string
		charset      string
		rawName      string
		fallbackName string
	}{
		{"unknown declaration with ASCII bytes", "ISO_IR 999", "TEST^NAME", "TEST^NAME"},
		{"unknown declaration with high bytes", "ISO_IR 999", "TEST^\xe9", "TEST^é"},
		{"malformed UTF-8 sequence", "ISO_IR 192", "TEST^\xc3(", ""},
		{"high byte outside default repertoire", "", "TEST^\xab", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var elements []core.Element
			if tc.charset != "" {
				elements = append(elements, textElement(tagSpecificCharacterSet, core.VRCS, tc.charset))
			}
			raw := core.VRPN.PadToEvenLength([]byte(tc.rawName))
			elements = append(elements, core.NewRawElement(nameTag, core.VRPN, raw))
			wire := dicomtest.EncodeElements(syntax, elements...)
			decoded, report, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(wire), syntax,
				object.ReadFileOptions{}, validation.Options{Mode: validation.ModeWarn})
			if err != nil || len(report.Findings) != 1 || report.Findings[0].Code != validation.CodeValueRepertoire ||
				report.Findings[0].Severity != validation.SeverityWarning || report.Findings[0].Tag != nameTag {
				t.Fatalf("warn report = %#v, error %v", report, err)
			}
			if got, err := decoded.LookupString(nameTag); got != "" || err == nil {
				t.Fatalf("strict text lookup returned %q, error %v, want an encoding error", got, err)
			}
			assertEncodedText(t, decoded, nameTag, core.VRPN, tc.rawName)
			var copied bytes.Buffer
			if count, err := decoded.CopyValueTo(nameTag, &copied); err != nil || count != int64(len(raw)) || !bytes.Equal(copied.Bytes(), raw) {
				t.Fatalf("copy returned %d bytes % X, want % X, error %v", count, copied.Bytes(), raw, err)
			}
			if tc.fallbackName != "" {
				decoded.SetTextOptions(object.TextOptions{AllowUnsupportedCharsetFallback: true, FallbackCharacterSet: dicomenc.ISOIR100})
				if got, err := decoded.LookupString(nameTag); got != tc.fallbackName || err != nil {
					t.Fatalf("explicit fallback returned %q, error %v, want %q", got, err, tc.fallbackName)
				}
				return
			}
			var replay bytes.Buffer
			if err := object.WriteDataSet(&replay, decoded, syntax); err != nil || !bytes.Equal(replay.Bytes(), wire) {
				t.Fatalf("raw replay = % X, want % X, error %v", replay.Bytes(), wire, err)
			}
		})
	}
}
