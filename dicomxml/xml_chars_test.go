package dicomxml

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func rawText(group, element uint16, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.RawValue(value)}
}

// TestMarshalRejectsCharactersXMLCannotHold checks that text XML 1.0 cannot
// represent fails with its path instead of becoming U+FFFD. Trailing NUL
// padding of a text value is padding since #2032, removed when the value is
// read, so it exports without an error or a RecoverText report.
func TestMarshalRejectsCharactersXMLCannotHold(t *testing.T) {
	const root = "/NativeDicomModel/DicomAttribute[@tag='"
	utf8Charset := rawText(0x0008, 0x0005, core.VRCS, "ISO_IR 192")
	tests := []struct {
		name  string
		elems []core.Element
		// path is the strict Error.Path; empty when the value is valid.
		path string
		// recovered lists RecoverText's reports; nil when recovery must
		// fail too, at path.
		recovered []TextRecovery
		// tag and values, when set, are checked after reading the XML back.
		tag    core.Tag
		values []string
	}{
		{
			name:  "LO NUL padding",
			elems: []core.Element{rawText(0x0008, 0x0080, core.VRLO, "???\x00")},
			tag:   core.NewTag(0x0008, 0x0080), values: []string{"???"},
		},
		{
			name:  "NUL after space padding",
			elems: []core.Element{rawText(0x0010, 0x0020, core.VRLO, "Pixel \x00")},
			tag:   core.NewTag(0x0010, 0x0020), values: []string{"Pixel"},
		},
		{
			name:  "last of several values",
			elems: []core.Element{rawText(0x0028, 0x0030, core.VRDS, "0.5\\0.25\x00")},
			tag:   core.NewTag(0x0028, 0x0030), values: []string{"0.5", "0.25"},
		},
		{
			name:  "PN component",
			elems: []core.Element{rawText(0x0008, 0x0090, core.VRPN, "Doe^John\x00")},
			tag:   core.NewTag(0x0008, 0x0090), values: []string{"Doe^John"},
		},
		{
			name:  "PN last component group",
			elems: []core.Element{rawText(0x0008, 0x0090, core.VRPN, "Doe=Roe=Poe\x00")},
			tag:   core.NewTag(0x0008, 0x0090), values: []string{"Doe=Roe=Poe"},
		},
		{
			name: "private LO in a sequence item",
			elems: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1115), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				rawText(0x0009, 0x0010, core.VRLO, "ACME"),
				rawText(0x0009, 0x1012, core.VRLO, "2DMovie\x00"),
			}}}}}},
		},
		{
			name:  "embedded NUL",
			elems: []core.Element{rawText(0x0008, 0x0080, core.VRLO, "A\x00B")},
			path:  root + "00080080']/Value[1]",
		},
		{
			name:  "NUL ending a value that is not the last",
			elems: []core.Element{rawText(0x0008, 0x0008, core.VRCS, "ORIGINAL\x00\\PRIMARY")},
			tag:   core.NewTag(0x0008, 0x0008), values: []string{"ORIGINAL", "PRIMARY"},
		},
		{
			name:  "control character before the padding",
			elems: []core.Element{rawText(0x0008, 0x0080, core.VRLO, "AB\x01\x00")},
			path:  root + "00080080']/Value[1]",
		},
		{
			name:  "form feed",
			elems: []core.Element{rawText(0x0008, 0x4000, core.VRLT, "page\x0cbreak")},
			path:  root + "00084000']/Value[1]",
		},
		{
			name:  "U+FFFF",
			elems: []core.Element{utf8Charset, rawText(0x0008, 0x0080, core.VRLO, "A￿")},
			path:  root + "00080080']/Value[1]",
		},
		{
			name:  "BulkData URI",
			elems: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x2000), VR: core.VROB}, Value: core.BulkDataValue{URI: "https://example.invalid/\x01"}}},
			path:  root + "00282000']/BulkData",
		},
		{
			name:   "TAB, LF and CR are kept",
			elems:  []core.Element{rawText(0x0008, 0x4000, core.VRLT, "a\tb\nc\r\nd")},
			tag:    core.NewTag(0x0008, 0x4000),
			values: []string{"a\tb\nc\r\nd"},
		},
		{
			name:   "U+FFFD is kept",
			elems:  []core.Element{utf8Charset, rawText(0x0008, 0x0080, core.VRLO, "A�")},
			tag:    core.NewTag(0x0008, 0x0080),
			values: []string{"A�"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := object.FromElements(tt.elems, std.Dictionary)
			data, err := Marshal(obj, DefaultOptions())
			if tt.path != "" {
				assertPathError(t, err, tt.path)
			} else if err != nil {
				t.Fatal(err)
			}
			opts := DefaultOptions()
			opts.RecoverText = true
			var got []TextRecovery
			opts.TextRecovered = func(r TextRecovery) { got = append(got, r) }
			recovered, err := Marshal(obj, opts)
			if tt.path != "" && tt.recovered == nil {
				assertPathError(t, err, tt.path)
				if len(got) != 0 {
					t.Fatalf("failed recovery reported %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.recovered) {
				t.Fatalf("recoveries %+v, want %+v", got, tt.recovered)
			}
			if tt.path == "" && string(recovered) != string(data) {
				t.Fatal("RecoverText changed valid output")
			}
			back, err := Unmarshal(recovered, std.Dictionary)
			if err != nil {
				t.Fatal(err)
			}
			if tt.values != nil {
				values, err := back.LookupStrings(tt.tag)
				if err != nil || !reflect.DeepEqual(values, tt.values) {
					t.Fatalf("read back %q (%v), want %q", values, err, tt.values)
				}
			}
		})
	}
}

func assertPathError(t *testing.T, err error, path string) {
	t.Helper()
	var xerr *Error
	if !errors.Is(err, ErrInvalidXML) || !errors.As(err, &xerr) || xerr.Path != path {
		t.Fatalf("error %v, want ErrInvalidXML at %s", err, path)
	}
}

func TestRecoverTextRequiresReport(t *testing.T) {
	opts := DefaultOptions()
	opts.RecoverText = true
	if _, err := Marshal(object.New(std.Dictionary), opts); !errors.Is(err, ErrInvalidXML) {
		t.Fatalf("RecoverText without a report: %v", err)
	}
}

// TestNULPaddedCreatorNeedsRecoverPrivate checks that the privateCreator
// attribute never carries a NUL. Trailing NUL padding is padding since
// #2032, so a padded creator and value export without any recovery.
func TestNULPaddedCreatorNeedsRecoverPrivate(t *testing.T) {
	obj := object.FromElements([]core.Element{
		rawText(0x0029, 0x0010, core.VRLO, "GEIIS\x00"),
		rawText(0x0029, 0x1019, core.VRLO, "1.3\x00"),
	}, std.Dictionary)
	opts := DefaultOptions()
	opts.RecoverText = true
	var text []TextRecovery
	opts.TextRecovered = func(r TextRecovery) { text = append(text, r) }
	opts.RecoverPrivate = true
	var private []PrivateRecovery
	opts.PrivateRecovered = func(r PrivateRecovery) { private = append(private, r) }
	data, err := Marshal(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(private) != 0 || len(text) != 0 {
		t.Fatalf("recoveries for trailing NUL padding: private %+v, text %+v", private, text)
	}
	plain, err := Marshal(obj, DefaultOptions())
	if err != nil || string(plain) != string(data) {
		t.Fatalf("strict export: %v", err)
	}
	if !strings.Contains(string(data), `privateCreator="GEIIS" tag="00290019"`) || strings.Contains(string(data), "\x00") {
		t.Fatalf("XML:\n%s", data)
	}
}

func TestCheckXMLText(t *testing.T) {
	for _, tt := range []struct {
		text string
		want string
	}{
		{"plain \t\n\r text", ""},
		{"é中\U0001F600�", ""},
		{"a\x00", "character U+0000 at byte 1"},
		{"\x1b$B", "character U+001B at byte 0"},
		{"a￾", "character U+FFFE at byte 1"},
		{"ab\xff", "invalid UTF-8 at byte 2"},
		{"\xed\xa0\x80", "invalid UTF-8 at byte 0"}, // UTF-8 encoded surrogate
	} {
		err := checkXMLText(tt.text)
		if tt.want == "" && err != nil || tt.want != "" && (!errors.Is(err, ErrInvalidXML) || !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("checkXMLText(%q) = %v, want %q", tt.text, err, tt.want)
		}
	}
}
