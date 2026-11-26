package quirkcorpus

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image/color"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	jpegcodec "github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/render"
)

type Result struct {
	ID, Mode                                string
	Stages                                  Stages
	MetadataChecked                         bool
	SourceSHA256, PixelSHA256, RenderSHA256 string
	Recovery                                []string
	Diagnostic                              string
}

func ReadOptions() object.ReadFileOptions {
	return object.ReadFileOptions{MaxTotalBytes: 1 << 20, MaxElementBytes: 64 << 10, MaxPixelDataBytes: 64 << 10, MaxElements: 256, MaxFragments: 32, MaxSequenceDepth: 8}
}

// Scan uses a finite whole-file/metadata/codec budget. It deliberately keeps
// parse, preservation, metadata semantics, decoded samples and 2D rendering
// separate. A parseable input is not automatically a valid image.
func Scan(ctx context.Context, path string, f Fixture, recover bool) (Result, error) {
	r := Result{ID: f.ID, Mode: "strict", Stages: rejected()}
	if recover {
		r.Mode = "recover"
	}
	if e := ctx.Err(); e != nil {
		return r, e
	}
	// This scanner admits only the small authored fixtures, never arbitrary
	// vendor/clinical files. Bound the read even if the path changes size.
	input, e := os.Open(path)
	if e != nil {
		return r, e
	}
	wire, e := io.ReadAll(io.LimitReader(input, MaxFixtureBytes+1))
	closeErr := input.Close()
	if e != nil {
		return r, e
	}
	if closeErr != nil {
		return r, closeErr
	}
	if len(wire) > MaxFixtureBytes || !bytes.Equal(wire, f.Wire) {
		return r, errors.New("input differs from bounded authored fixture")
	}
	r.SourceSHA256 = Hash(wire)
	o := ReadOptions()
	if recover {
		switch f.Policy {
		case "incomplete":
			o.AllowIncompletePixelData = true
		case "delimiter":
			o.RecoverDelimiterAnomalies = true
		case "sequence":
			o.SequenceRecovery.ReplaceWrongDelimiter = true
		case "odd":
			o.OddLengthPolicy = parser.AcceptOddLength
		}
	}
	var file *object.File
	if recover && f.Policy == "syntax" {
		var resolution object.TransferSyntaxResolution
		file, resolution, e = object.ReadFileWithTransferSyntaxRecovery(bytes.NewReader(wire), o, object.TransferSyntaxRecoveryOptions{AllowMissingPreamble: true, AllowMissingFileMeta: true, AllowDeclaredMismatch: true})
		if e == nil {
			r.Recovery = append(r.Recovery, "transfer-syntax:"+resolution.Syntax.UID)
		}
	} else {
		file, e = object.ReadFileWithOptions(bytes.NewReader(wire), o)
	}
	if file != nil {
		defer file.Close()
	}
	if e != nil {
		r.Diagnostic = e.Error()
		if file != nil && file.Dataset != nil {
			if _, partial := file.Dataset.IncompletePixelData(); partial {
				r.Stages = Stages{"partial", "partial", "not-run", "not-run"}
				if e = CheckMetadata(file, f.Oracle); e != nil {
					return r, e
				}
				r.MetadataChecked = true
			}
		}
		return r, nil
	}
	r.Stages = undecoded()
	if e = CheckMetadata(file, f.Oracle); e != nil {
		return r, e
	}
	r.MetadataChecked = true
	if e = CheckRetainedValues(file, f); e != nil {
		r.Stages.Preserve = "mismatch"
		return r, e
	}
	before, e := semanticSnapshot(file.Dataset, file.TransferSyntax.ByteOrder)
	if e != nil {
		return r, e
	}
	metaBefore, e := semanticSnapshot(file.Meta, binary.LittleEndian)
	if e != nil {
		return r, e
	}
	original := file.Dataset
	if recover && f.Policy == "un" {
		var report object.UNRecoveryReport
		file.Dataset, report, e = file.Dataset.RecoverUN(ctx, object.UNRecoveryOptions{})
		if e != nil {
			return r, e
		}
		if report.Recovered > 0 {
			r.Recovery = append(r.Recovery, "known-un")
		}
	}
	if list, _ := file.Dataset.BoundaryRecoveries(); len(list) > 0 {
		for _, b := range list {
			r.Recovery = append(r.Recovery, string(b.Reason))
		}
	}
	if f.Oracle.SequenceItems > 0 || (recover && f.ID == "TestVRSQUN1") {
		items, ok := file.Dataset.GetSequence(tag(0x0008, 0x1115))
		count := f.Oracle.SequenceItems
		if f.ID == "TestVRSQUN1" {
			count = 1
		}
		if !ok || len(items) != count {
			return r, fmt.Errorf("%s: sequence items %d, expected %d", f.ID, len(items), count)
		}
		if uid, _ := items[len(items)-1].GetString(tag(0x0020, 0x000e)); uid != "2.25.141704" {
			return r, fmt.Errorf("%s: nested UID oracle mismatch", f.ID)
		}
	}
	registry := pixeldata.NewMemoryRegistry()
	if e = jpegcodec.Register(registry); e != nil {
		return r, e
	}
	opts := pixeldata.DecompressOptions{Registry: registry, Limits: pixeldata.DecompressLimits{MaxFrames: 4, MaxPixels: 4096, MaxNativeBytes: 64 << 10, MaxInputBytes: 64 << 10, MaxExpansionRatio: 256}}
	if recover {
		switch f.Policy {
		case "native-under-compressed":
			opts.Recovery.NativePixelDataUnderEncapsulated = true
		case "byte-swap":
			opts.Recovery.NativeOWByteSwap = true
		}
	}
	var report pixeldata.DecompressReport
	opts.Report = &report
	decoded, syntax, de := pixeldata.DecompressDataSetContext(ctx, file.Dataset, file.TransferSyntax, opts)
	r.Recovery = append(r.Recovery, report.Diagnostics...)
	// Decoding/recovery must never alter the retained source object, including
	// private blobs, overlay metadata, group lengths and original VRs.
	after, se := semanticSnapshot(original, file.TransferSyntax.ByteOrder)
	metaAfter, me := semanticSnapshot(file.Meta, binary.LittleEndian)
	if se != nil || me != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(metaBefore, metaAfter) {
		r.Stages.Preserve = "mismatch"
		return r, fmt.Errorf("%s: source semantics changed: dataset=%v meta=%v", f.ID, se, me)
	}
	if de != nil {
		r.Diagnostic = de.Error()
		return r, nil
	}
	reader, re := pixeldata.NewFrameReader(ctx, &object.File{Dataset: decoded, TransferSyntax: syntax}, nil, pixeldata.FrameReaderOptions{MaxFrames: 4, MaxFragments: 32})
	if re != nil {
		r.Diagnostic = re.Error()
		return r, nil
	}
	defer reader.Close()
	pcm, metadata, re := reader.Frame(ctx, 0)
	if re != nil {
		r.Diagnostic = re.Error()
		return r, nil
	}
	r.Stages.Decode = "ok"
	r.PixelSHA256 = Hash(pcm)
	want, he := hex.DecodeString(f.Oracle.PixelHex)
	if he != nil {
		return r, he
	}
	if !bytes.Equal(pcm, want) {
		r.Stages.Decode = "mismatch"
		r.Diagnostic = "decoded samples disagree with authored oracle"
		return r, nil
	}
	if int(metadata.Rows) != f.Oracle.Rows || int(metadata.Columns) != f.Oracle.Columns || int(metadata.SamplesPerPixel) != f.Oracle.Samples || int(metadata.BitsAllocated) != f.Oracle.BitsAllocated || int(metadata.BitsStored) != f.Oracle.BitsStored || int(metadata.HighBit) != f.Oracle.HighBit {
		r.Stages.Decode = "mismatch"
		return r, fmt.Errorf("%s: decoded metadata/layout oracle mismatch: %+v", f.ID, metadata)
	}
	if f.Oracle.PixelRepresentation == nil || int(metadata.PixelRepresentation) != *f.Oracle.PixelRepresentation {
		return r, errors.New("decoded signedness oracle mismatch")
	}
	if metadata.PhotometricInterpretation != f.Oracle.Photometric && !(f.ID == "ACUSON-8-YBR_FULL-JPEG-TrailingInfo" && metadata.PhotometricInterpretation == "RGB") {
		return r, fmt.Errorf("%s: decoded photometric oracle mismatch", f.ID)
	}
	image, e := render.RenderFrame(&render.Frame{Metadata: metadata, PixelBytes: pcm, ByteOrder: binary.LittleEndian, Rescale: render.Rescale{Slope: 1}}, render.WindowLevel{Center: 128, Width: 256})
	if e != nil {
		r.Stages.Render = "reject"
		r.Diagnostic = e.Error()
		return r, nil
	}
	if image.Bounds().Dx() != f.Oracle.Columns || image.Bounds().Dy() != f.Oracle.Rows {
		return r, fmt.Errorf("%s: rendered geometry mismatch", f.ID)
	}
	var rgba []byte
	for y := 0; y < image.Bounds().Dy(); y++ {
		for x := 0; x < image.Bounds().Dx(); x++ {
			c := color.NRGBAModel.Convert(image.At(x, y)).(color.NRGBA)
			rgba = append(rgba, c.R, c.G, c.B, c.A)
		}
	}
	r.RenderSHA256 = Hash(rgba)
	r.Stages.Render = "ok"
	// Hand-specified sample values plus the DICOM LINEAR 128/256 window map
	// scalar 0..255 directly; the neutral color JPEG is also exact grey.
	var wantRGBA []byte
	for i := 0; i < f.Oracle.Rows*f.Oracle.Columns; i++ {
		var value byte
		if f.Oracle.Samples == 3 {
			value = want[3*i]
		} else if f.Oracle.BitsAllocated == 16 {
			value = byte(binary.LittleEndian.Uint16(want[2*i:]))
		} else {
			value = want[i]
		}
		wantRGBA = append(wantRGBA, value, value, value, 255)
	}
	if !bytes.Equal(rgba, wantRGBA) {
		r.Stages.Render = "mismatch"
		r.Diagnostic = "rendered pixels disagree with independent LINEAR-window oracle"
	}
	return r, nil
}

func Hash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func semanticSnapshot(ds *object.Object, order binary.ByteOrder) ([]dicomtest.SemanticElement, error) {
	elements := ds.Elements()
	for i, e := range elements {
		if fragments, ok := e.Value.(core.FragmentSequence); ok {
			// Length-prefix every retained carrier part; boundaries, empty items and
			// BOT bytes matter even when concatenated codestream bytes are equal.
			var b bytes.Buffer
			_ = binary.Write(&b, binary.LittleEndian, uint32(len(fragments.OffsetTable)))
			b.Write(fragments.OffsetTable)
			_ = binary.Write(&b, binary.LittleEndian, uint32(len(fragments.Fragments)))
			for _, part := range fragments.Fragments {
				_ = binary.Write(&b, binary.LittleEndian, uint32(len(part)))
				b.Write(part)
			}
			e.Value = core.RawValue(b.Bytes())
			elements[i] = e
		}
	}
	return dicomtest.SemanticDataSet(core.DataSet{Elements: elements}, order)
}

// CheckMetadata deliberately does not use a pixel hash or decoder metadata as
// its oracle. A dimensions/photometric/geometry regression cannot hide behind
// unchanged Pixel Data bytes. Presence of signedness is checked independently.
func CheckMetadata(f *object.File, o Oracle) error {
	if f == nil || f.Dataset == nil {
		return errors.New("metadata: no dataset")
	}
	ds := f.Dataset
	for _, v := range []struct {
		g, e uint16
		want int
	}{{0x0028, 0x0010, o.Rows}, {0x0028, 0x0011, o.Columns}, {0x0028, 0x0002, o.Samples}, {0x0028, 0x0100, o.BitsAllocated}, {0x0028, 0x0101, o.BitsStored}, {0x0028, 0x0102, o.HighBit}} {
		n, err := ds.GetInt(tag(v.g, v.e))
		if err != nil || int(n) != v.want {
			return fmt.Errorf("metadata %04x,%04x: got %d error=%v expected %d", v.g, v.e, n, err, v.want)
		}
	}
	count, _ := ds.GetString(tag(0x0028, 0x0008))
	n, e := strconv.Atoi(count)
	if e != nil || n != o.Frames {
		return errors.New("metadata: frame count mismatch")
	}
	value, repErr := ds.GetInt(tag(0x0028, 0x0103))
	ok := ds.Has(tag(0x0028, 0x0103)) && repErr == nil
	if (o.PixelRepresentation == nil && ok) || (o.PixelRepresentation != nil && (!ok || int(value) != *o.PixelRepresentation)) {
		return errors.New("metadata: signedness/presence mismatch")
	}
	photo, _ := ds.GetString(tag(0x0028, 0x0004))
	if photo != o.Photometric {
		return errors.New("metadata: photometric mismatch")
	}
	for _, v := range []struct {
		t    core.Tag
		want []float64
	}{{tag(0x0028, 0x0030), o.Spacing}, {tag(0x0020, 0x0037), o.Orientation}, {tag(0x0020, 0x0032), o.Position}} {
		value, e := ds.GetFloats(v.t)
		if e != nil || !reflect.DeepEqual(value, v.want) {
			return fmt.Errorf("metadata: geometry %s mismatch", v.t)
		}
	}
	class, _ := ds.GetString(tag(0x0008, 0x0016))
	if class != o.DataSOP {
		return errors.New("metadata: dataset SOP mismatch")
	}
	metaClass := ""
	if f.Meta != nil {
		metaClass, _ = f.Meta.GetString(tag(0x0002, 0x0002))
		if metaClass == "" {
			if element, present := f.Meta.Get(tag(0x0002, 0x0002)); present {
				raw, _ := element.RawBytes()
				metaClass = strings.Trim(string(raw), " \x00")
			}
		}
	}
	if metaClass != o.MetaSOP {
		return fmt.Errorf("metadata: File Meta SOP mismatch: %q vs %q", metaClass, o.MetaSOP)
	}
	overlays := 0
	for g := uint16(0x6000); g <= 0x601e; g += 2 {
		if ds.Has(tag(g, 0x3000)) {
			overlays++
			for _, a := range []struct {
				e uint16
				n int
			}{{0x0010, 2}, {0x0011, 2}, {0x0100, 1}, {0x0102, 0}} {
				n, err := ds.GetInt(tag(g, a.e))
				if err != nil || int(n) != a.n {
					return errors.New("metadata: overlay layout mismatch")
				}
			}
		}
	}
	if overlays != o.OverlayGroups {
		return errors.New("metadata: overlay count mismatch")
	}
	return nil
}
