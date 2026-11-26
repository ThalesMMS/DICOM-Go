package object

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	encTagCharset     = core.NewTag(0x0008, 0x0005)
	encTagSOPInstance = core.NewTag(0x0008, 0x0018)
	encTagSequence    = core.NewTag(0x0008, 0x1115)
	encTagPatientName = core.NewTag(0x0010, 0x0010)
	encTagComments    = core.NewTag(0x0010, 0x4000)
	encTagInstitution = core.NewTag(0x0008, 0x0080)
	encTagCreator     = core.NewTag(0x0011, 0x0010)
)

func encRaw(tag core.Tag, vr core.VR, text string) core.Element {
	value := core.RawValue(vr.PadToEvenLength([]byte(text)))
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr, Length: core.Length(len(value))}, Value: value}
}

// encSource is a UTF-8 data set with a Latin-1 item.
func encSource(name, comments string) *Object {
	latin1Item := core.DataSet{Elements: []core.Element{
		encRaw(encTagCharset, core.VRCS, "ISO_IR 100"),
		encRaw(encTagInstitution, core.VRLO, "Cl\xednica S\xe3o Jo\xe3o"),
	}}
	inherits := core.DataSet{Elements: []core.Element{encRaw(encTagInstitution, core.VRLO, "Straße ①")}}
	return FromElements([]core.Element{
		encRaw(encTagCharset, core.VRCS, "ISO_IR 192"),
		encRaw(encTagSOPInstance, core.VRUI, "1.2.826.0.1.3680043.10.1.984"),
		{Header: core.ElementHeader{Tag: encTagSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{latin1Item, inherits}}},
		encRaw(encTagPatientName, core.VRPN, name),
		encRaw(encTagComments, core.VRLT, comments),
		encRaw(encTagCreator, core.VRLO, "CREATOR ÜBER"),
	}, std.Dictionary)
}

func decodedText(t *testing.T, o *Object, tag core.Tag) string {
	t.Helper()
	values, err := o.LookupStrings(tag)
	if err != nil {
		t.Fatalf("decode %s: %v", tag, err)
	}
	return strings.Join(values, `\`)
}

func TestConvertTextToCharacterSetPolicies(t *testing.T) {
	ctx := context.Background()
	source := encSource("Müller^José=山田^太郎", "Größe: 5 mm – “ok” …  ")
	cases := []struct {
		target, policy string
		want           map[core.Tag]string
		item0, item1   string
	}{
		{"", "transliterate", map[core.Tag]string{encTagPatientName: "Muller^Jose=??^??", encTagComments: `Grosse: 5 mm - "ok" ...`}, "Clinica Sao Joao", "Strasse (1)"},
		{"", "discard", map[core.Tag]string{encTagPatientName: "Mller^Jos=^", encTagComments: "Gre: 5 mm  ok"}, "Clnica So Joo", "Strae"},
		{"ISO_IR 100", "transliterate", map[core.Tag]string{encTagPatientName: "Müller^José=??^??", encTagComments: `Größe: 5 mm - "ok" ...`}, "Clínica São João", "Straße (1)"},
		{"ISO_IR 101", "transliterate", map[core.Tag]string{encTagPatientName: "Müller^José=??^??"}, "Clínica Sao Joao", "Straße (1)"},
		{"ISO_IR 126", "transliterate", map[core.Tag]string{encTagPatientName: "Muller^Jose=??^??"}, "Clinica Sao Joao", "Strasse (1)"},
		{"ISO_IR 144", "discard", map[core.Tag]string{encTagPatientName: "Mller^Jos=^"}, "Clnica So Joo", "Strae"},
		// The EUC-JP tables include the NEC circled digits.
		{`\ISO 2022 IR 87`, "transliterate", map[core.Tag]string{encTagPatientName: "Muller^Jose=山田^太郎"}, "Clinica Sao Joao", "Strasse ①"},
	}
	for _, c := range cases {
		policy := UnrepresentableTransliterate
		if c.policy == "discard" {
			policy = UnrepresentableDiscard
		}
		out, report, err := source.ConvertTextToCharacterSet(ctx, c.target, CharsetEncodeOptions{Unrepresentable: policy})
		name := c.target + "/" + c.policy
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		declared, _ := out.LookupStrings(encTagCharset)
		if strings.Join(declared, `\`) != c.target {
			t.Fatalf("%s: declared %q", name, declared)
		}
		for tag, want := range c.want {
			if got := decodedText(t, out, tag); got != want {
				t.Errorf("%s: %s = %q, want %q", name, tag, got, want)
			}
		}
		items, _ := out.GetSequence(encTagSequence)
		if got := decodedText(t, items[0], encTagInstitution); got != c.item0 {
			t.Errorf("%s: own-declaration item %q, want %q", name, got, c.item0)
		}
		if got := decodedText(t, items[1], encTagInstitution); got != c.item1 {
			t.Errorf("%s: inheriting item %q, want %q", name, got, c.item1)
		}
		for _, tag := range []core.Tag{encTagSOPInstance, encTagCreator} {
			before, _ := source.GetRaw(tag)
			if after, _ := out.GetRaw(tag); !bytes.Equal(after, before) {
				t.Errorf("%s: %s changed from %q to %q", name, tag, before, after)
			}
		}
		counted := report.TransliteratedCharacters + report.DiscardedCharacters
		if counted == 0 || report.EncodedValues == 0 {
			t.Errorf("%s: report %+v", name, report)
		}
		for _, diagnostic := range report.Diagnostics {
			if strings.ContainsAny(diagnostic.Path, "üéÜ山") || strings.Contains(diagnostic.Path, "Jos") {
				t.Errorf("%s: diagnostic leaks text: %+v", name, diagnostic)
			}
		}
		// The result writes and reads back.
		var buf bytes.Buffer
		if err := WriteDataSet(&buf, out, transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatalf("%s: write: %v", name, err)
		}
		if _, err := ReadDataSet(bytes.NewReader(buf.Bytes()), transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatalf("%s: read back: %v", name, err)
		}
	}
}

func TestConvertTextToCharacterSetStrictAndSafety(t *testing.T) {
	ctx := context.Background()
	source := encSource("Müller^José", "plain")
	_, report, err := source.ConvertTextToCharacterSet(ctx, "", CharsetEncodeOptions{})
	if !errors.Is(err, ErrUnrepresentableText) {
		t.Fatalf("strict: %v", err)
	}
	if len(report.Diagnostics) != 1 || report.Diagnostics[0].Reason != CharsetUnrepresentable || report.Diagnostics[0].Path == "" {
		t.Fatalf("strict failure not reported: %+v", report.Diagnostics)
	}
	// The strict default converts representable text losslessly.
	plain := FromElements([]core.Element{
		encRaw(encTagCharset, core.VRCS, "ISO_IR 192"),
		encRaw(encTagPatientName, core.VRPN, "Müller^José"),
	}, std.Dictionary)
	out, _, err := plain.ConvertTextToCharacterSet(ctx, "ISO_IR 100", CharsetEncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := out.GetRaw(encTagPatientName); !bytes.Equal(raw, []byte("M\xfcller^Jos\xe9 ")) {
		t.Fatalf("Latin-1 bytes %q", raw)
	}
	if _, _, err := source.ConvertTextToCharacterSet(ctx, "ISO_IR 999", CharsetEncodeOptions{}); !errors.Is(err, dicomenc.ErrUnsupportedCharset) && err == nil {
		t.Fatalf("unknown target accepted")
	}
	// The source object is not modified.
	if raw, _ := source.GetRaw(encTagPatientName); !bytes.Contains(raw, []byte("Müller")) {
		t.Fatal("source modified")
	}
}

// TestConvertTextToCharacterSetMatchesDCMTK cross-checks dcmconv: --discard-
// illegal with its built-in ASCII and Latin-1 converters must give the same
// bytes, and --transliterate through oficonv the same text except where
// oficonv writes ASCII accent marks (`"u`, `'e`, `~N`) for a letter we reduce
// to its base, or "?" where we find an approximation. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestConvertTextToCharacterSetMatchesDCMTK(t *testing.T) {
	dcmconv := dcmtkoracle.Tool(t, "dcmconv")
	characters := []rune("ÀÁÂÃÄÅÆÇÈÉÊËÌÍÎÏÐÑÒÓÔÕÖØÙÚÛÜÝÞßàáâãäåæçèéêëìíîïðñòóôõöøùúûüýþÿĀāĂăĄąĆćČčĎďĐđĒēĘęĚěĞğĦħİıĹĺĽľŁłŃńŇňŐőŒœŔŕŘřŚśŞşŠšŢţŤťŰűŸŹźŻżŽžƒ‘’‚“”„–—…•€™©®°±×÷¢£¥§¶¿¡«»ﬁﬂⅠⅡⅣ①②ΑΒΓαβγАБВабв")
	elements := []core.Element{encRaw(encTagCharset, core.VRCS, "ISO_IR 192"), encRaw(encTagSOPInstance, core.VRUI, "1.2.3")}
	for i, r := range characters {
		elements = append(elements, encRaw(core.NewTag(0x0011, uint16(0x1000+i)), core.VRLO, string(r)))
	}
	elements = append(elements, encRaw(encTagCreator, core.VRLO, "PROBE"))
	source := FromElements(elements, std.Dictionary)
	dir := t.TempDir()
	input := filepath.Join(dir, "utf8.dcm")
	file := &File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}
	file.Dataset.Put(encRaw(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"))
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	accentMarks := strings.NewReplacer(`"`, "", "'", "", "´", "", "~", "", "^", "", "`", "", ",", "")
	for _, c := range []struct {
		args   []string
		target string
		policy UnrepresentablePolicy
		exact  bool
	}{
		{[]string{"+A7", "-Cd"}, "", UnrepresentableDiscard, true},
		{[]string{"+L1", "-Cd"}, "ISO_IR 100", UnrepresentableDiscard, true},
		{[]string{"+C", "ISO_IR 101", "-Ct"}, "ISO_IR 101", UnrepresentableTransliterate, false},
		{[]string{"+C", "ISO_IR 109", "-Ct"}, "ISO_IR 109", UnrepresentableTransliterate, false},
		{[]string{"+C", "ISO_IR 110", "-Ct"}, "ISO_IR 110", UnrepresentableTransliterate, false},
		{[]string{"+C", "ISO_IR 126", "-Ct"}, "ISO_IR 126", UnrepresentableTransliterate, false},
		{[]string{"+C", "ISO_IR 144", "-Ct"}, "ISO_IR 144", UnrepresentableTransliterate, false},
		{[]string{"+C", "ISO_IR 148", "-Ct"}, "ISO_IR 148", UnrepresentableTransliterate, false},
	} {
		output := filepath.Join(dir, "dcmtk.dcm")
		tolerance := dcmtkoracle.Exact
		if !c.exact {
			tolerance = "same text except oficonv ASCII accent marks and \"?\" approximations"
		}
		dcmtkoracle.Run(t, tolerance, dcmconv, append(append([]string{}, c.args...), input, output)...)
		theirs, err := OpenFile(output)
		if err != nil {
			t.Fatal(err)
		}
		ours, _, err := source.ConvertTextToCharacterSet(context.Background(), c.target, CharsetEncodeOptions{Unrepresentable: c.policy})
		if err != nil {
			t.Fatal(err)
		}
		mismatches := 0
		for i, r := range characters {
			tag := core.NewTag(0x0011, uint16(0x1000+i))
			if c.exact {
				a, _ := ours.GetRaw(tag)
				b, _ := theirs.Dataset.GetRaw(tag)
				if !bytes.Equal(a, b) {
					mismatches++
					t.Errorf("%v %q: ours %q, dcmconv %q", c.args, r, a, b)
				}
				continue
			}
			a := decodedText(t, ours, tag)
			b := decodedText(t, theirs.Dataset, tag)
			if a == b || b == "?" || accentMarks.Replace(b) == accentMarks.Replace(a) {
				continue
			}
			mismatches++
			t.Errorf("%v %q: ours %q, dcmconv %q", c.args, r, a, b)
		}
		_ = theirs.Close()
		if mismatches > 0 {
			t.Errorf("%v: %d of %d characters differ", c.args, mismatches, len(characters))
		}
	}
}

// TestConvertTextToCharacterSetDecodedStringValues covers values held as
// decoded strings (as DICOM JSON and XML produce), which the policy must
// reach like raw values.
func TestConvertTextToCharacterSetDecodedStringValues(t *testing.T) {
	ctx := context.Background()
	source := FromElements([]core.Element{
		encRaw(encTagCharset, core.VRCS, "ISO_IR 100"),
		{Header: core.ElementHeader{Tag: encTagPatientName, VR: core.VRPN}, Value: core.StringValue{"Müller^José"}},
		{Header: core.ElementHeader{Tag: encTagInstitution, VR: core.VRLO}, Value: core.StringValue{"Clínica", "Zweite"}},
	}, std.Dictionary)
	if _, _, err := source.ConvertTextToCharacterSet(ctx, "ISO_IR 126", CharsetEncodeOptions{}); !errors.Is(err, ErrUnrepresentableText) {
		t.Fatalf("strict conversion of string values: %v", err)
	}
	out, report, err := source.ConvertTextToCharacterSet(ctx, "ISO_IR 126", CharsetEncodeOptions{Unrepresentable: UnrepresentableTransliterate})
	if err != nil || report.EncodedValues != 2 {
		t.Fatalf("transliterate: %v, %+v", err, report)
	}
	if name, _ := out.GetRaw(encTagPatientName); !bytes.Equal(name, []byte("Muller^Jose ")) {
		t.Fatalf("name %q", name)
	}
	if institution, _ := out.GetRaw(encTagInstitution); !bytes.Equal(institution, []byte(`Clinica\Zweite`)) {
		t.Fatalf("institution %q", institution)
	}
}
