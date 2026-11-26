package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type testAVCBackend struct {
	payload []byte
	size    int64
	err     error
}

func (b testAVCBackend) DecodeAVCMP4(ctx context.Context, _ []byte, p AVCProfile, dst io.Writer) error {
	if b.err != nil {
		return b.err
	}
	size := p.frameBytes() * int64(p.Frames)
	if b.size != 0 {
		size = b.size
	}
	_, err := io.CopyN(dst, infiniteZeroReader{}, size)
	return err
}

func (b testAVCBackend) EncodeAVCMP4(ctx context.Context, _ []byte, _ AVCProfile, _ AVCEncodeOptions, dst io.Writer) error {
	if b.err != nil {
		return b.err
	}
	_, err := dst.Write(b.payload)
	return err
}

type infiniteZeroReader struct{}

func (infiniteZeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func avcSource(t *testing.T) (*object.File, []byte, AVCProfile) {
	t.Helper()
	payload, err := os.ReadFile("testdata/avc-high41.mp4")
	if err != nil {
		t.Fatal(err)
	}
	limits, err := (ImportLimits{}).normalize()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := inspectAVC(context.Background(), payload, limits)
	if err != nil {
		t.Fatal(err)
	}
	file := videoFile(t, transfer.MPEG4HP41, payload)
	for _, item := range []struct {
		tag   core.Tag
		vr    core.VR
		value []string
	}{
		{tags.SOPInstanceUID, core.VRUI, []string{"1.2.3.4"}},
		{tags.StudyInstanceUID, core.VRUI, []string{"1.2.3"}},
		{tags.SeriesInstanceUID, core.VRUI, []string{"1.2.3.1"}},
		{tags.NumberOfFrames, core.VRIS, []string{"6"}},
		{tags.FrameTime, core.VRDS, []string{"40"}},
		{tags.PhotometricInterpretation, core.VRCS, []string{"YBR_PARTIAL_420"}},
		{tagAVCLossy, core.VRCS, []string{"01"}},
		{tagAVCLossyRatio, core.VRDS, []string{"10"}},
		{tagAVCLossyMethod, core.VRCS, []string{avcCompressionMethod}},
	} {
		if err := file.Dataset.SetStrings(item.tag, item.vr, item.value...); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		tag   core.Tag
		value uint16
	}{
		{tags.Rows, 64}, {tags.Columns, 96}, {tags.SamplesPerPixel, 3},
		{tags.BitsAllocated, 8}, {tags.BitsStored, 8}, {tags.HighBit, 7},
		{tags.PixelRepresentation, 0}, {tags.PlanarConfiguration, 0},
	} {
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: item.tag, VR: core.VRUS}, Value: core.Uint16Value{item.value}})
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagAVCFrameIncrement, VR: core.VRAT}, Value: core.TagValue{tags.FrameTime}})
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	return file, payload, profile
}

func TestAVCDecodeAndTranscodeContract(t *testing.T) {
	source, mp4, profile := avcSource(t)
	if profile.Rows != 64 || profile.Columns != 96 || profile.Frames != 6 ||
		profile.RateNumerator != 25 || profile.RateDenominator != 1 || profile.Colour != [3]uint8{2, 2, 1} {
		t.Fatalf("profile %#v", profile)
	}
	var before bytes.Buffer
	if err := object.WriteFile(&before, source); err != nil {
		t.Fatal(err)
	}
	backend := testAVCBackend{payload: mp4}
	decoded, err := DecodeAVCMP4(context.Background(), source, backend, AVCCodecLimits{})
	if err != nil || decoded.Profile != profile || int64(len(decoded.Samples)) != profile.frameBytes()*6 {
		t.Fatalf("decode %#v, %v", decoded.Profile, err)
	}
	out, report, err := TranscodeAVCMP4(context.Background(), source, backend, backend,
		AVCTranscodeOptions{AllowLossy: true, CRF: 18})
	if err != nil {
		t.Fatal(err)
	}
	if report.Profile != profile || report.SOPInstanceUID == report.SourceSOPInstanceUID ||
		report.SourceSOPInstanceUID != "1.2.3.4" || out.TransferSyntax.UID != transfer.MPEG4HP41.UID {
		t.Fatalf("report %#v", report)
	}
	if methods, _ := out.Dataset.GetStrings(tagAVCLossyMethod); len(methods) != 2 {
		t.Fatalf("compression history %v", methods)
	}
	if ratios, err := out.Dataset.GetFloats(tagAVCLossyRatio); err != nil || len(ratios) != 2 || ratios[0] != 10 {
		t.Fatalf("compression ratios %v, %v", ratios, err)
	}
	if derived, _ := out.Dataset.GetStrings(tagAVCImageType); len(derived) != 2 || derived[0] != "DERIVED" {
		t.Fatalf("image type %v", derived)
	}
	refs, ok := out.Dataset.GetSequence(core.NewTag(0x0008, 0x2112))
	if !ok || len(refs) != 1 {
		t.Fatalf("source references %v", refs)
	}
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, out); err != nil {
		t.Fatal(err)
	}
	reopened, err := object.ReadFile(bytes.NewReader(wire.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAVCMP4(context.Background(), reopened, backend, AVCCodecLimits{}); err != nil {
		t.Fatalf("reopened %v", err)
	}
	var after bytes.Buffer
	if err := object.WriteFile(&after, source); err != nil || !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("source changed")
	}
}

func TestAVCCodecRejectsLimitsFailuresAndMismatches(t *testing.T) {
	source, mp4, profile := avcSource(t)
	backend := testAVCBackend{payload: mp4}
	t.Run("missing backend", func(t *testing.T) {
		if _, err := DecodeAVCMP4(context.Background(), source, nil, AVCCodecLimits{}); !errors.Is(err, ErrCodecBackend) {
			t.Fatal(err)
		}
	})
	for name, limits := range map[string]AVCCodecLimits{
		"frames":   {ImportLimits: ImportLimits{MaxFrames: 5}},
		"bytes":    {ImportLimits: ImportLimits{MaxBytes: 32}},
		"pixels":   {ImportLimits: ImportLimits{MaxPixels: 1}},
		"samples":  {ImportLimits: ImportLimits{MaxDecodedBytes: 1}},
		"negative": {ImportLimits: ImportLimits{MaxFrames: -1}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAVCMP4(context.Background(), source, backend, limits); err == nil {
				t.Fatal("accepted limits")
			}
		})
	}
	for _, size := range []int64{1, profile.frameBytes()*6 + 1} {
		if _, err := DecodeAVCMP4(context.Background(), source, testAVCBackend{size: size}, AVCCodecLimits{}); err == nil {
			t.Fatal("accepted decoded size", size)
		}
	}
	for _, options := range []AVCTranscodeOptions{{}, {AllowLossy: true}, {AllowLossy: true, CRF: 52}} {
		if _, _, err := TranscodeAVCMP4(context.Background(), source, backend, backend, options); !errors.Is(err, ErrLossyPolicy) {
			t.Fatal(err)
		}
	}
	if _, _, err := TranscodeAVCMP4(context.Background(), source, backend, testAVCBackend{payload: mp4[:100]},
		AVCTranscodeOptions{AllowLossy: true, CRF: 18}); err == nil {
		t.Fatal("accepted truncated output")
	}
	source.TransferSyntax = transfer.MPEG2MPML
	if _, err := DecodeAVCMP4(context.Background(), source, backend, AVCCodecLimits{}); !errors.Is(err, ErrCodecProfile) {
		t.Fatal(err)
	}
	source.TransferSyntax = transfer.MPEG4HP41
	source.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tags.Rows, VR: core.VRUS}, Value: core.Uint16Value{48}})
	if _, err := DecodeAVCMP4(context.Background(), source, backend, AVCCodecLimits{}); !errors.Is(err, ErrCodecProfile) {
		t.Fatal(err)
	}
}

func TestAVCTranscodePathPreservesDestination(t *testing.T) {
	source, mp4, _ := avcSource(t)
	backend := testAVCBackend{payload: mp4}
	dir := t.TempDir()
	input, target := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "target.dcm")
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, source); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	options := AVCTranscodeOptions{AllowLossy: true, CRF: 18}
	for name, current := range map[string]struct {
		ctx context.Context
		enc AVCEncoder
	}{
		"backend":   {context.Background(), testAVCBackend{err: errors.New("encode failed")}},
		"truncated": {context.Background(), testAVCBackend{payload: mp4[:100]}},
		"canceled":  {func() context.Context { c, cancel := context.WithCancel(context.Background()); cancel(); return c }(), backend},
		"deadline": {func() context.Context {
			c, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			return c
		}(), backend},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(target, []byte("existing output"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := TranscodeAVCMP4Path(current.ctx, input, target, backend, current.enc, options); err == nil {
				t.Fatal("expected refusal")
			}
			got, err := os.ReadFile(target)
			if err != nil || string(got) != "existing output" {
				t.Fatal("destination changed", err)
			}
		})
	}
	if _, err := TranscodeAVCMP4Path(context.Background(), input, input, backend, backend, options); err == nil {
		t.Fatal("source overwrite accepted")
	}
	if _, err := TranscodeAVCMP4Path(context.Background(), input, target, backend, backend, options); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(input); err != nil || !bytes.Equal(after, encoded.Bytes()) {
		t.Fatal("source bytes changed", err)
	}
}

func TestAVCTranscodeCancellationDuringBackend(t *testing.T) {
	source, mp4, _ := avcSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := cancelAVCBackend{cancel: cancel, payload: mp4}
	result, _, err := TranscodeAVCMP4(ctx, source, backend, backend, AVCTranscodeOptions{AllowLossy: true, CRF: 18})
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("cancellation result %v, %v", result, err)
	}
}

type cancelAVCBackend struct {
	cancel  context.CancelFunc
	payload []byte
}

func (b cancelAVCBackend) DecodeAVCMP4(ctx context.Context, p []byte, info AVCProfile, w io.Writer) error {
	return testAVCBackend{payload: b.payload}.DecodeAVCMP4(ctx, p, info, w)
}

func (b cancelAVCBackend) EncodeAVCMP4(ctx context.Context, _ []byte, _ AVCProfile, _ AVCEncodeOptions, w io.Writer) error {
	b.cancel()
	_, err := w.Write(b.payload)
	return err
}

func TestAVCTranscodePreservesUnknownPriorCompression(t *testing.T) {
	source, mp4, _ := avcSource(t)
	source.Dataset.Remove(tagAVCLossyRatio)
	source.Dataset.Remove(tagAVCLossyMethod)
	backend := testAVCBackend{payload: mp4}
	result, _, err := TranscodeAVCMP4(context.Background(), source, backend, backend,
		AVCTranscodeOptions{AllowLossy: true, CRF: 18})
	if err != nil {
		t.Fatal(err)
	}
	methods, _ := result.Dataset.GetStrings(tagAVCLossyMethod)
	if len(methods) != 2 || result.Dataset.Has(tagAVCLossyRatio) {
		t.Fatalf("prior unknown ratio overwritten: methods %v", methods)
	}
}

func TestAVCCodecInputBoundaries(t *testing.T) {
	source, mp4, _ := avcSource(t)
	backend := testAVCBackend{payload: mp4}
	cases := map[string]func(*object.File){
		"frame count":   func(f *object.File) { _ = f.Dataset.SetStrings(tags.NumberOfFrames, core.VRIS, "5") },
		"frame time":    func(f *object.File) { _ = f.Dataset.SetStrings(tags.FrameTime, core.VRDS, "42") },
		"display rate":  func(f *object.File) { _ = f.Dataset.SetStrings(tags.CineRate, core.VRIS, "30") },
		"syntax":        func(f *object.File) { _ = f.Meta.SetStrings(tags.TransferSyntaxUID, core.VRUI, transfer.HEVCMP51.UID) },
		"native colour": func(f *object.File) { _ = f.Dataset.SetStrings(tags.PhotometricInterpretation, core.VRCS, "RGB") },
		"truncation": func(f *object.File) {
			f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength},
				Value: core.FragmentSequence{Fragments: [][]byte{mp4[:len(mp4)-10]}}})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			copy, err := object.CloneFile(source)
			if err != nil {
				t.Fatal(err)
			}
			mutate(copy)
			if _, err := DecodeAVCMP4(context.Background(), copy, backend, AVCCodecLimits{}); err == nil {
				t.Fatal("malformed input accepted")
			}
		})
	}
}

func TestAVCPresentationTiming(t *testing.T) {
	ctts := func(entries [][2]uint32) []byte {
		body := make([]byte, 8+len(entries)*8)
		binary.BigEndian.PutUint32(body[4:], uint32(len(entries)))
		for i, entry := range entries {
			binary.BigEndian.PutUint32(body[8+i*8:], entry[0])
			binary.BigEndian.PutUint32(body[12+i*8:], entry[1])
		}
		return body
	}
	for _, tc := range []struct {
		name    string
		entries [][2]uint32
		valid   bool
	}{
		{"constant", [][2]uint32{{3, 0}}, true},
		{"offset", [][2]uint32{{3, 40}}, true},
		{"reordered", [][2]uint32{{1, 40}, {1, 80}, {1, 0}}, true},
		{"duplicate", [][2]uint32{{1, 40}, {2, 0}}, false},
		{"incomplete", [][2]uint32{{1, 40}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := ctts(tc.entries)
			p := mp4Import{ctx: context.Background(), data: data}
			err := p.inspectAVCPresentation([]importBox{{kind: "ctts", payload: 0, end: len(data)}}, 3, 40)
			if (err == nil) != tc.valid {
				t.Fatalf("valid %v, error %v", tc.valid, err)
			}
		})
	}
}
