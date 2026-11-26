package clireadopts

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func crShort(g, e uint16, vr string, v []byte) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b, g)
	binary.LittleEndian.PutUint16(b[2:], e)
	copy(b[4:], vr)
	binary.LittleEndian.PutUint16(b[6:], uint16(len(v)))
	return append(b, v...)
}
func crLong(g, e uint16, vr string, length uint32, v []byte) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint16(b, g)
	binary.LittleEndian.PutUint16(b[2:], e)
	copy(b[4:], vr)
	binary.LittleEndian.PutUint32(b[8:], length)
	return append(b, v...)
}
func crImplicit(g, e uint16, length uint32, v []byte) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b, g)
	binary.LittleEndian.PutUint16(b[2:], e)
	binary.LittleEndian.PutUint32(b[4:], length)
	return append(b, v...)
}
func crControl(e uint16, length uint32) []byte { return crImplicit(0xfffe, e, length, nil) }
func crPart10(s transfer.Syntax, body []byte) []byte {
	return append(append(make([]byte, 128), "DICM"...), append(dicomtest.NewFileMetaBuilder().WithTransferSyntax(s.UID).Encode(), body...)...)
}
func crReasons(r Report) string {
	var reasons []string
	for _, e := range r.Events {
		reasons = append(reasons, e.Reason)
	}
	return strings.Join(reasons, ",")
}

// Independent byte layouts exercise each recovery switch through the public object reader.
func TestReadRecoveryByteMatrix(t *testing.T) {
	short := crShort(0x0010, 0x0020, "LO", []byte("SAFE"))
	item := append(crControl(0xe000, 0xffffffff), short...)
	wrong := crLong(0x0008, 0x1111, "SQ", 0xffffffff, append(append(item, crControl(0xe00d, 0)...), crControl(0xe00d, 0)...))
	ob := crLong(0x0009, 0x1001, "OB", 0xffffffff, append(append(item, crControl(0xe00d, 0)...), crControl(0xe0dd, 0)...))
	voiItem := append(crControl(0xe000, uint32(len(short))), short...)
	voi := crLong(0x0028, 0x3010, "OW", uint32(len(voiItem)), voiItem)
	badItem := append(crControl(0xe000, 10), []byte{0x10, 0, 0x20, 0, 'L', 'O', 0, 0x16, 'x', 'x'}...)
	private := crLong(0x2001, 0x105f, "SQ", uint32(len(badItem)), badItem)
	ul := crShort(0x0009, 0x1113, "UL", []byte{1, 0, 0, 0})
	binary.LittleEndian.PutUint16(ul[6:], 6)
	ul = append(ul, short...)
	container := crLong(0x0008, 0x1111, "SQ", uint32(len(item)), item)
	ge := append(crImplicit(0x0008, 0x103e, 13, []byte("SYNTHETIC ")), crImplicit(0x0008, 0x1060, 8, []byte("NO^PHI  "))...)
	cases := []struct {
		name     string
		flags    []string
		data     []byte
		syntax   transfer.Syntax
		reason   string
		strictOK bool
	}{
		{"wrong delimiter", []string{"-read-replace-wrong-delimiter"}, wrong, transfer.ExplicitVRLittleEndian, "wrong-delimiter-replaced", false},
		{"OB sequence", []string{"-read-undefined-length-obow-as-sq"}, ob, transfer.ExplicitVRLittleEndian, "undefined-length-ob-ow-read-as-sq", false},
		{"VOI sequence", []string{"-read-voi-lut-ow-as-sq"}, voi, transfer.ExplicitVRLittleEndian, "voi-lut-sequence-read-as-sq", true},
		{"private bytes", []string{"-read-private-sequence-as-bytes"}, private, transfer.ExplicitVRLittleEndian, "private-sequence-kept-as-bytes", false},
		{"dictionary VR", []string{"-read-prefer-dictionary-vr"}, crShort(0x0010, 0x0010, "LO", []byte("SAFE")), transfer.ExplicitVRLittleEndian, "dictionary-vr-preferred", true},
		{"dictionary width", []string{"-read-prefer-dictionary-vr", "-read-prefer-dictionary-length-field-size"}, crLong(0x0042, 0x0011, "LO", 2, []byte{1, 2}), transfer.ExplicitVRLittleEndian, "dictionary-length-field-size", false},
		{"private UL", []string{"-read-correct-private-ul-length"}, ul, transfer.ExplicitVRLittleEndian, "private-ul-length-6-read-as-4", false},
		{"OX", []string{"-read-map-ox-to-ow"}, crLong(0x7fe0, 0x0010, "OX", 2, []byte{1, 2}), transfer.ExplicitVRLittleEndian, "ox-pixel-data-read-as-ow", false},
		{"short UN", []string{"-read-short-length-un"}, append(crShort(0x0019, 0x1010, "UN", []byte("12")), short...), transfer.ExplicitVRLittleEndian, "short-length-un", false},
		{"GE text", []string{"-read-correct-ge-text-length"}, ge, transfer.ImplicitVRLittleEndian, "ge-text-length-13-read-as-10", false},
		{"implicit element", []string{"-read-recover-implicit-vr-elements"}, append(crImplicit(0x0009, 0x1001, 2, []byte{1, 2}), short...), transfer.ExplicitVRLittleEndian, "implicit-vr-element", false},
		{"container", []string{"-read-recover-container-overruns"}, container, transfer.ExplicitVRLittleEndian, "missing-delimiter-at-container-end", false},
		{"orphan", []string{"-read-recover-delimiter-anomalies"}, append(crControl(0xe00d, 0), short...), transfer.ExplicitVRLittleEndian, "orphan-delimiter-skipped", false},
		{"odd", []string{"-read-odd-length", "accept"}, crShort(0x0010, 0x0020, "LO", []byte("ABC")), transfer.ExplicitVRLittleEndian, "odd-length-accepted", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, strictErr := object.ReadDataSetWithOptions(bytes.NewReader(c.data), c.syntax, object.ReadFileOptions{})
			if (strictErr == nil) != c.strictOK {
				t.Fatalf("strict outcome=%v", strictErr)
			}
			o := crOptions(t, true, c.flags...)
			obj, err := object.ReadDataSetWithOptions(bytes.NewReader(c.data), c.syntax, o.Apply(object.ReadFileOptions{}))
			if err != nil {
				t.Fatal(err)
			}
			report, err := CollectObject(context.Background(), obj, o, "dataset")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(crReasons(report), c.reason) {
				t.Fatalf("missing reason %s: %s", c.reason, crReasons(report))
			}
			var output bytes.Buffer
			if err := report.WriteTo(&output, "test"); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SAFE", "SYNTHETIC", "NO^PHI"} {
				if strings.Contains(output.String(), secret) {
					t.Fatal("value leaked in diagnostics")
				}
			}
		})
	}
}

func TestReadMetaAndDeflateRecovery(t *testing.T) {
	meta := dicomtest.NewFileMetaBuilder().Build()
	implicitRest := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, meta[1:]...)
	var groupLength [4]byte
	binary.LittleEndian.PutUint32(groupLength[:], uint32(len(implicitRest)))
	implicitMeta := append(crImplicit(2, 0, 4, groupLength[:]), implicitRest...)
	body := crShort(0x0010, 0x0020, "LO", []byte("SAFE"))
	tests := []struct {
		name   string
		flags  []string
		data   []byte
		reason string
	}{
		{"implicit meta", []string{"-read-allow-implicit-file-meta"}, append(append(make([]byte, 128), "DICM"...), append(implicitMeta, body...)...), "implicit-vr-file-meta"},
		{"missing group length", []string{"-read-allow-missing-meta-group-length"}, append(append(make([]byte, 128), "DICM"...), append(dicomtest.NewFileMetaBuilder().Encode()[12:], body...)...), "missing-meta-group-length-accepted"},
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(body)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name   string
		flags  []string
		data   []byte
		reason string
	}{"zlib", []string{"-read-allow-zlib-deflate"}, crPart10(transfer.DeflatedExplicitVRLittleEndian, compressed.Bytes()), "zlib-deflate-wrapper"})
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			if _, err := object.ReadFile(bytes.NewReader(c.data)); err == nil {
				t.Fatal("strict read accepted malformed container")
			}
			o := crOptions(t, true, c.flags...)
			f, err := object.ReadFileWithOptions(bytes.NewReader(c.data), o.Apply(object.ReadFileOptions{}))
			if err != nil {
				t.Fatal(err)
			}
			r, err := CollectFile(context.Background(), f, o)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(crReasons(r), c.reason) {
				t.Fatalf("reasons=%s", crReasons(r))
			}
		})
	}
}

func TestReadExplicitMetaRecoveryReported(t *testing.T) {
	data := crPart10(transfer.ExplicitVRLittleEndian, crShort(0x0010, 0x0020, "LO", []byte("SAFE")))
	at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	if at < 132 {
		t.Fatal("fixture lacks File Meta version")
	}
	copy(data[at+4:at+6], "UN")
	opts := crOptions(t, true, "-read-prefer-dictionary-vr")
	file, err := object.ReadFileWithOptions(bytes.NewReader(data), opts.Apply(object.ReadFileOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	version, ok := file.Meta.Get(core.NewTag(2, 1))
	if !ok || version.VR() != core.VROB {
		t.Fatal("fixture did not apply the Meta VR repair")
	}
	replay := MetaReplay{Open: func() (io.Reader, error) { return bytes.NewReader(data), nil }, Check: func() error { return nil }}
	report, err := CollectFileReplay(context.Background(), file, opts, opts.Apply(object.ReadFileOptions{}), replay)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range report.Events {
		if event.Scope == "meta" && event.Path == "(0002,0001)" && event.Reason == "dictionary-vr-preferred" {
			return
		}
	}
	t.Fatal("applied File Meta VR repair was not reported")
}

func TestReadMetaReplayAbsoluteTotalByteLimit(t *testing.T) {
	body := crShort(0x0010, 0x0020, "LO", []byte("SAFE"))
	explicit := dicomtest.NewFileMetaBuilder().Encode()
	at := bytes.Index(explicit, []byte{2, 0, 1, 0, 'O', 'B'})
	if at < 0 {
		t.Fatal("fixture lacks Meta version")
	}
	copy(explicit[at+4:at+6], "UN")
	meta := dicomtest.NewFileMetaBuilder().Build()
	implicitRest := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, meta[1:]...)
	var groupLength [4]byte
	binary.LittleEndian.PutUint32(groupLength[:], uint32(len(implicitRest)))
	implicit := append(crImplicit(2, 0, 4, groupLength[:]), implicitRest...)
	for _, tc := range []struct {
		name   string
		meta   []byte
		flags  []string
		reason string
	}{
		{"explicit group length", explicit, []string{"-read-prefer-dictionary-vr"}, "dictionary-vr-preferred"},
		{"explicit missing group length", explicit[12:], []string{"-read-prefer-dictionary-vr", "-read-allow-missing-meta-group-length"}, "dictionary-vr-preferred"},
		{"implicit group length", implicit, []string{"-read-allow-implicit-file-meta"}, "implicit-vr-file-meta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := append(append(append(make([]byte, 128), "DICM"...), tc.meta...), body...)
			// Leave eight bytes for the original reader's EOF/header probe.
			// The extent is just beyond the file, not a remaining-byte budget.
			limit := int64(len(data) + 8)
			opts := crOptions(t, true, tc.flags...)
			opts.Read.MaxTotalBytes = limit
			read := opts.Apply(object.ReadFileOptions{})
			file, err := object.ReadFileWithOptions(bytes.NewReader(data), read)
			if err != nil {
				t.Fatalf("original read refused independently sufficient total=%d: %v", limit, err)
			}
			replay := MetaReplay{Open: func() (io.Reader, error) { return bytes.NewReader(data), nil }, Check: func() error { return nil }}
			report, err := CollectFileReplay(context.Background(), file, opts, read, replay)
			if err != nil {
				t.Fatalf("replay refused original accepted total=%d: %v", limit, err)
			}
			found := false
			for _, event := range report.Events {
				if event.Scope == "meta" && event.Reason == tc.reason {
					found = true
					if event.Offset < 132 {
						t.Fatal("Meta diagnostic lost its absolute BaseOffset")
					}
				}
			}
			if !found {
				t.Fatal("replay omitted the enabled Meta repair")
			}
			read.MaxTotalBytes = int64(132 + len(tc.meta) - 1)
			report, err = CollectFileReplay(context.Background(), file, opts, read, replay)
			if err == nil || len(report.Events) != 0 {
				t.Fatal("replay exceeded the configured absolute input limit")
			}
		})
	}
}

func TestReadMetaReplayOwnershipConsistencyAndJointCap(t *testing.T) {
	data := crPart10(transfer.ExplicitVRLittleEndian, crShort(0x0010, 0x0010, "LO", []byte("SAFE")))
	at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
	copy(data[at+4:at+6], "UN")
	path := filepath.Join(t.TempDir(), "meta.dcm")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	replay, err := FileMetaReplay(source)
	if err != nil {
		t.Fatal(err)
	}
	opts := crOptions(t, true, "-read-prefer-dictionary-vr")
	read := opts.Apply(object.ReadFileOptions{})
	file, err := object.ReadFileWithOptions(source, read)
	if err != nil {
		t.Fatal(err)
	}
	position, _ := source.Seek(0, io.SeekCurrent)
	report, err := CollectFileReplay(context.Background(), file, opts, read, replay)
	if err != nil || len(report.Events) != 2 {
		t.Fatalf("complete report events=%d error=%v", len(report.Events), err)
	}
	after, seekErr := source.Seek(0, io.SeekCurrent)
	if seekErr != nil || after != position {
		t.Fatal("Meta replay changed cursor or closed borrowed source")
	}
	var diagnostics bytes.Buffer
	if err := report.WriteTo(&diagnostics, "test"); err != nil || strings.Contains(diagnostics.String(), "SAFE") {
		t.Fatal("metadata diagnostics exposed a value")
	}
	capOpts := crOptions(t, true, "-read-prefer-dictionary-vr", "-read-max-boundary-recoveries", "1")
	report, err = CollectFileReplay(context.Background(), file, capOpts, capOpts.Apply(object.ReadFileOptions{}), replay)
	if !errors.Is(err, ErrDiagnosticLimit) || len(report.Events) != 0 {
		t.Fatal("joint Meta/dataset cap accepted incomplete report")
	}
	if _, err := CollectFileReplay(context.Background(), file, opts, read, MetaReplay{}); !errors.Is(err, errMetaReplay) {
		t.Fatal("unreplayable source accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CollectFileReplay(ctx, file, opts, read, replay); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled replay accepted")
	}
	changed := append([]byte(nil), data...)
	changed[at+12] ^= 1
	inconsistent := MetaReplay{Open: func() (io.Reader, error) { return bytes.NewReader(changed), nil }, Check: func() error { return nil }}
	if _, err := CollectFileReplay(context.Background(), file, opts, read, inconsistent); !errors.Is(err, errMetaReplay) {
		t.Fatal("different recovered metadata accepted")
	}
	if err := os.Truncate(path, int64(len(data)-2)); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectFileReplay(context.Background(), file, opts, read, replay); !errors.Is(err, errMetaReplay) {
		t.Fatal("changed source accepted")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectFileReplay(context.Background(), file, opts, read, replay); !errors.Is(err, errMetaReplay) {
		t.Fatal("closed source accepted")
	}
}

func TestReadReservedBytesRemainPermissiveUntilOptIn(t *testing.T) {
	raw := crLong(0x7777, 0x1001, "OB", 2, []byte{1, 2})
	raw[6] = 1
	if _, err := object.ReadDataSetWithOptions(bytes.NewReader(raw), transfer.ExplicitVRLittleEndian, crOptions(t, true).Apply(object.ReadFileOptions{})); err != nil {
		t.Fatal(err)
	}
	if _, err := object.ReadDataSetWithOptions(bytes.NewReader(raw), transfer.ExplicitVRLittleEndian, crOptions(t, true, "-read-strict-reserved-bytes").Apply(object.ReadFileOptions{})); err == nil {
		t.Fatal("strict reserved bytes accepted")
	}
}

func TestReadDiagnosticsRefuseTruncationAndJointCap(t *testing.T) {
	raw := append(crControl(0xe00d, 0), append(crControl(0xe00d, 0), crShort(0x0010, 0x0020, "LO", []byte("SAFE"))...)...)
	o := crOptions(t, true, "-read-recover-delimiter-anomalies", "-read-max-boundary-recoveries", "1")
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(raw), transfer.ExplicitVRLittleEndian, o.Apply(object.ReadFileOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	r, err := CollectObject(context.Background(), obj, o, "dataset")
	if !errors.Is(err, ErrDiagnosticLimit) || len(r.Events) != 0 {
		t.Fatalf("truncated report: %v", err)
	}
	one := append(crControl(0xe00d, 0), crShort(0x0010, 0x0020, "LO", []byte("SAFE"))...)
	obj, err = object.ReadDataSetWithOptions(bytes.NewReader(one), transfer.ExplicitVRLittleEndian, o.Apply(object.ReadFileOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	r, err = CollectFile(context.Background(), &object.File{Meta: obj, Dataset: obj}, o)
	if !errors.Is(err, ErrDiagnosticLimit) || len(r.Events) != 0 {
		t.Fatal("joint cap was not enforced")
	}
	if _, err := CollectFile(nil, &object.File{}, o); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CollectObject(ctx, obj, o, "dataset"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var out bytes.Buffer
	sink := Sink{Writer: &out}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sink.Emit("test", Report{Coordinates: "dataset-logical", Events: []Event{{Scope: "dataset", Path: core.NewTag(0x0010, 0x0020).String(), Reason: "fixed"}}})
		}()
	}
	wg.Wait()
	if strings.Count(out.String(), "\n") != 8 {
		t.Fatal("diagnostic sink interleaved lines")
	}
}
