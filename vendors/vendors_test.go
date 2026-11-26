package vendors

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// geBlock returns a Protocol Data Block holding text, cut short by drop
// bytes of the gzip stream.
func geBlock(t testing.TB, name, text string, drop int) []byte {
	t.Helper()
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	writer.Name = name
	if _, err := writer.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	stream := compressed.Bytes()[:compressed.Len()-drop]
	out := append(u32(uint32(len(stream))), stream...)
	if len(out)%2 != 0 {
		out = append(out, 0)
	}
	return out
}

func TestParseGEProtocolDataBlock(t *testing.T) {
	block, err := ParseGEProtocolDataBlock(geBlock(t, "Ex1Ser2Rx", "ENTRY \"Head First\"\r\nTE \"102.0\"\n\nIOPT \"FC, EDR\"\n", 0), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if block.Name != "Ex1Ser2Rx" || block.XML || block.Truncated || len(block.Entries) != 3 {
		t.Fatalf("block %+v", block)
	}
	if v, ok := block.Value("IOPT"); !ok || v != "FC, EDR" {
		t.Fatalf("IOPT = %q", v)
	}
	if _, ok := block.Value("TR"); ok {
		t.Fatal("found an absent key")
	}
	// A stream without its trailer decodes, reported as truncated.
	xml := `<?xml version="1.0"?><monarch version="1.2"></monarch>`
	block, err = ParseGEProtocolDataBlock(geBlock(t, "", xml, 8), Limits{})
	if err != nil || !block.XML || !block.Truncated || block.Text != xml || block.Entries != nil {
		t.Fatalf("truncated XML: %+v %v", block, err)
	}
	for name, tc := range map[string]struct {
		data   []byte
		limits Limits
		want   error
	}{
		"empty":            {nil, Limits{}, ErrInvalid},
		"length too large": {append(u32(100), 1, 2), Limits{}, ErrInvalid},
		"not gzip":         {append(u32(4), 1, 2, 3, 4), Limits{}, ErrInvalid},
		"decoded limit":    {geBlock(t, "", strings.Repeat("A \"1\"\n", 100), 0), Limits{MaxDecodedBytes: 64}, ErrLimit},
		"entry limit":      {geBlock(t, "", "A \"1\"\nB \"2\"\n", 0), Limits{MaxEntries: 1}, ErrLimit},
		"blob limit":       {geBlock(t, "", "A \"1\"\n", 0), Limits{MaxBytes: 8}, ErrLimit},
	} {
		if _, err := ParseGEProtocolDataBlock(tc.data, tc.limits); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func cstr(s string) []byte { return append([]byte(s), 0) }

// elscintBlob builds an ELSCINT1 value with a single group and a multiple
// group of two items, the second empty.
func elscintBlob() []byte {
	var b []byte
	b = append(b, u32(elscintMagic)...)
	b = append(b, u32(1)...)
	b = append(b, u32(2)...)
	b = append(b, u32(elscintSingle)...)
	b = append(b, cstr("")...)
	b = append(b, u32(2)...)
	b = append(b, cstr("protocol-name")...)
	b = append(b, u32(1)...)
	b = append(b, cstr("Chest")...)
	b = append(b, cstr("kv")...)
	b = append(b, u32(2)...)
	b = append(b, cstr("120")...)
	b = append(b, cstr("140")...)
	b = append(b, u32(elscintMultiple)...)
	b = append(b, cstr("scans")...)
	b = append(b, u32(2)...)
	b = append(b, u32(1)...)
	b = append(b, cstr("mas")...)
	b = append(b, u32(1)...)
	b = append(b, cstr("200")...)
	b = append(b, u32(0)...)
	return b
}

func TestParseElscintProtocol(t *testing.T) {
	protocol, err := ParseElscintProtocol(elscintBlob(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if len(protocol.Groups) != 2 {
		t.Fatalf("groups %+v", protocol.Groups)
	}
	single, multiple := protocol.Groups[0], protocol.Groups[1]
	if single.Multiple || len(single.Items) != 1 || len(single.Items[0]) != 2 || single.Items[0][1].Key != "kv" || strings.Join(single.Items[0][1].Values, `\`) != `120\140` {
		t.Fatalf("single group %+v", single)
	}
	if !multiple.Multiple || multiple.Name != "scans" || len(multiple.Items) != 2 || len(multiple.Items[1]) != 0 || multiple.Items[0][0].Values[0] != "200" {
		t.Fatalf("multiple group %+v", multiple)
	}
	blob := elscintBlob()
	for name, tc := range map[string]struct {
		data   []byte
		limits Limits
		want   error
	}{
		"bad magic":    {append(u32(1), blob[4:]...), Limits{}, ErrInvalid},
		"truncated":    {blob[:len(blob)-3], Limits{}, ErrInvalid},
		"huge groups":  {append(append(u32(elscintMagic), u32(1)...), u32(1<<30)...), Limits{}, ErrInvalid},
		"bad kind":     {append(append(append(u32(elscintMagic), u32(1)...), u32(1)...), append(u32(7), 0, 0, 0, 0, 0)...), Limits{}, ErrInvalid},
		"entry budget": {blob, Limits{MaxEntries: 3}, ErrLimit},
	} {
		if _, err := ParseElscintProtocol(tc.data, tc.limits); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	// The compressed text variant.
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte("protocol text"))
	_ = writer.Close()
	text := append(make([]byte, elscintGzipOffset), compressed.Bytes()...)
	if protocol, err := ParseElscintProtocol(text, Limits{}); err != nil || protocol.Text != "protocol text" || protocol.Groups != nil {
		t.Fatalf("text variant: %+v %v", protocol, err)
	}
}

func veproBlob() []byte {
	data := bytes.Repeat([]byte{' '}, 2048)
	put := func(offset int, value string) { copy(data[offset:], value) }
	put(0, "VIM5.0")
	put(38, "Name of Institution")
	put(198, "20260926")
	put(206, "101500")
	put(276, "DICOM Distributor")
	put(340, "PID-1")
	put(404, "Synthetic Name")
	put(1642, "1.2.3.4\x00\x00")
	put(1706, "1.2.3.4.5")
	put(1770, "XA")
	return data
}

func TestParseVEPROData(t *testing.T) {
	data, err := ParseVEPROData(veproBlob(), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if data.ID != "VIM" || data.Version != "5.0" || data.UserName != "" || data.UserAddress[0] != "Name of Institution" || data.RecordingDate != "20260926" ||
		data.RecordingTime != "101500" || data.RecordingSource != "DICOM Distributor" || data.DataFields[0] != "PID-1" || data.DataFields[1] != "Synthetic Name" ||
		data.StudyInstanceUID != "1.2.3.4" || data.SeriesUID != "1.2.3.4.5" || data.Modality != "XA" {
		t.Fatalf("data %+v", data)
	}
	if _, err := ParseVEPROData(make([]byte, veproSize-1), Limits{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short: err = %v", err)
	}
}

func TestBlobLookupThroughCreators(t *testing.T) {
	dataset := object.New(std.Dictionary)
	dataset.Put(core.NewRawElement(core.NewTag(0x0055, 0x0011), core.VRLO, []byte("VEPRO VIM 5.0 DATA")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0055, 0x1120), core.VROB, veproBlob()))
	dataset.Put(core.NewRawElement(core.NewTag(0x0025, 0x0010), core.VRLO, []byte("GEMS_SERS_01")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0025, 0x101B), core.VROB, geBlock(t, "", "TE \"1\"\n", 0)))
	if data, err := VEPRODataOf(dataset, Limits{}); err != nil || data.Modality != "XA" {
		t.Fatalf("VEPRO: %+v %v", data, err)
	}
	if block, err := GEProtocolDataBlockOf(dataset, Limits{}); err != nil || len(block.Entries) != 1 {
		t.Fatalf("GE: %+v %v", block, err)
	}
	if _, err := ElscintProtocolOf(dataset, Limits{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ELSCINT: err = %v", err)
	}
	if _, err := VEPRODataOf(dataset, Limits{MaxBytes: 100}); !errors.Is(err, ErrLimit) {
		t.Fatalf("VEPRO limit: err = %v", err)
	}
	if _, err := GEProtocolDataBlockOf(nil, Limits{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil dataset: err = %v", err)
	}
}

func TestDetectManufacturer(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Manufacturer
	}{
		{"SIEMENS", ManufacturerSiemens}, {"ACUSON", ManufacturerSiemens}, {"G.E. Medical Systems", ManufacturerGE},
		{"GE_MEDICAL_SYSTEMS", ManufacturerGE}, {"GE Healthcare", ManufacturerGE}, {"Philips Medical Systems", ManufacturerPhilips},
		{"TOSHIBA", ManufacturerCanon}, {"CANON_MEC", ManufacturerCanon}, {"FUJI", ManufacturerFujifilm}, {"ALOKA CO., LTD.", ManufacturerFujifilm},
		{"Hitachi Medical Corporation", ManufacturerFujifilm}, {"GENESIS", ManufacturerUnknown}, {"Acme Products", ManufacturerUnknown}, {"", ManufacturerUnknown},
	} {
		if got := ManufacturerOfName(tc.value); got != tc.want {
			t.Errorf("ManufacturerOfName(%q) = %q, want %q", tc.value, got, tc.want)
		}
	}
	dataset := object.New(std.Dictionary)
	dataset.Put(core.NewRawElement(core.NewTag(0x0009, 0x0010), core.VRLO, []byte("GEMS_IDEN_01")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0019, 0x0010), core.VRLO, []byte("GEMS_ACQU_01")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0029, 0x0010), core.VRLO, []byte("SIEMENS MEDCOM HEADER")))
	detection := DetectManufacturer(dataset)
	if detection.Manufacturer != ManufacturerGE || !detection.Conflict || len(detection.Evidence) != 3 {
		t.Fatalf("creators only: %+v", detection)
	}
	dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0070), core.VRLO, []byte("SIEMENS ")))
	detection = DetectManufacturer(dataset)
	if detection.Manufacturer != ManufacturerSiemens || detection.Evidence[0].Source != EvidenceManufacturer || detection.Evidence[0].Value != "SIEMENS" {
		t.Fatalf("with Manufacturer: %+v", detection)
	}
	if detection := DetectManufacturer(nil); detection.Manufacturer != ManufacturerUnknown || detection.Conflict {
		t.Fatalf("nil: %+v", detection)
	}
}

func FuzzVendorBlobs(f *testing.F) {
	f.Add(geBlock(f, "n", "A \"1\"\n", 0))
	f.Add(elscintBlob())
	f.Add(veproBlob())
	f.Fuzz(func(t *testing.T, data []byte) {
		limits := Limits{MaxBytes: 1 << 16, MaxDecodedBytes: 1 << 16, MaxEntries: 1000}
		if block, err := ParseGEProtocolDataBlock(data, limits); err == nil && len(block.Text) > limits.MaxDecodedBytes {
			t.Fatalf("decoded %d bytes", len(block.Text))
		}
		if protocol, err := ParseElscintProtocol(data, limits); err == nil {
			values := 0
			for _, group := range protocol.Groups {
				for _, item := range group.Items {
					for _, entry := range item {
						values += 1 + len(entry.Values)
					}
				}
			}
			if values > limits.MaxEntries {
				t.Fatalf("%d entries and values", values)
			}
		}
		_, _ = ParseVEPROData(data, limits)
	})
}
