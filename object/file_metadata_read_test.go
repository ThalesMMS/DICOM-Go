package object_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type metadataReadSpan struct{ start, end int64 }

type metadataTestSource struct {
	*bytes.Reader
	forbidden []metadataReadSpan
	cancel    context.CancelFunc
}

func (r *metadataTestSource) Read(p []byte) (int, error) {
	pos, _ := r.Reader.Seek(0, io.SeekCurrent)
	for _, span := range r.forbidden {
		if pos < span.end && pos+int64(len(p)) > span.start {
			return 0, errors.New("read attempted to touch forbidden value bytes")
		}
	}
	n, err := r.Reader.Read(p)
	if r.cancel != nil && n > 0 {
		r.cancel()
	}
	return n, err
}

func TestReadFileMetadataSkipsNestedAndFloatPixels(t *testing.T) {
	iconTag := core.NewTag(0x0088, 0x0200)
	iconPixel := bytes.Repeat([]byte{0xf3, 0x72}, 100)
	rootPixel := bytes.Repeat([]byte{0x89, 0xf9}, 100)
	syntax := transfer.ExplicitVRBigEndian
	for _, test := range []struct {
		name string
		tag  core.Tag
		vr   core.VR
	}{
		{"native", core.TagPixelData, core.VROW},
		{"float", core.NewTag(0x7FE0, 0x0008), core.VROF},
		{"double_float", core.NewTag(0x7FE0, 0x0009), core.VROD},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := dicomtest.Part10File(syntax,
				core.NewRawElement(tags.PatientID, core.VRLO, []byte("META-1")),
				dicomtest.NewSequenceElement(iconTag, core.DataSet{Elements: []core.Element{
					core.NewRawElement(tags.Rows, core.VRUS, []byte{0, 4}),
					core.NewRawElement(core.TagPixelData, core.VROB, iconPixel),
				}}),
				core.NewRawElement(test.tag, test.vr, rootPixel),
			)
			if err != nil {
				t.Fatal(err)
			}
			const prefixLength = 11
			data = append(make([]byte, prefixLength), data...)
			source := &metadataTestSource{Reader: bytes.NewReader(data)}
			for _, payload := range [][]byte{iconPixel, rootPixel} {
				start := bytes.Index(data, payload)
				if start < 0 {
					t.Fatal("fixture payload missing")
				}
				source.forbidden = append(source.forbidden, metadataReadSpan{int64(start), int64(start + len(payload))})
			}
			if _, err := source.Seek(prefixLength, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			limits := object.MetadataReadLimits{SourceBytes: int64(len(data) - prefixLength), LogicalBytes: 2048, MetadataBytes: 1024}
			file, err := object.ReadFileMetadata(context.Background(), source, object.ReadFileOptions{}, limits)
			if err != nil {
				t.Fatal(err)
			}
			clear(data) // losing the source cannot affect the detached metadata
			if file.TransferSyntax.UID != syntax.UID || file.Dataset.ValueByteOrder() != binary.BigEndian {
				t.Fatal("metadata lost the resolved transfer syntax or byte order")
			}
			if id, _ := file.Dataset.GetString(tags.PatientID); id != "META-1" {
				t.Fatalf("Patient ID = %q", id)
			}
			icons, _ := file.Dataset.GetSequence(iconTag)
			if len(icons) != 1 || icons[0].ValueByteOrder() != binary.BigEndian {
				t.Fatalf("icon sequence or byte order was lost: %#v", icons)
			}
			if rows, ok := icons[0].Get(tags.Rows); !ok || !bytes.Equal(rows.Value.(core.RawValue), []byte{0, 4}) {
				t.Fatalf("icon Rows = %#v", rows)
			}
			for _, want := range []struct {
				data *object.Object
				tag  core.Tag
			}{{file.Dataset, test.tag}, {icons[0], core.TagPixelData}} {
				if pixel, ok := want.data.Get(want.tag); !ok || pixel.Value != nil {
					t.Fatalf("metadata pixel header = %#v, present=%t", pixel, ok)
				}
			}
		})
	}
}

func TestReadFileMetadataDeflatedByteLimits(t *testing.T) {
	var encoded bytes.Buffer
	encoded.Write(make([]byte, 128))
	encoded.WriteString("DICM")
	encoded.Write(dicomtest.NewFileMetaBuilder().WithTransferSyntax(transfer.DeflatedExplicitVRLittleEndian.UID).Encode())
	compressor, err := flate.NewWriter(&encoded, flate.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compressor.Write(dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		core.NewRawElement(tags.PatientID, core.VRLO, []byte("COMPRESSED")),
		core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 2<<20)),
	)); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	if encoded.Len()%2 != 0 {
		encoded.WriteByte(0)
	}
	data := encoded.Bytes()
	limits := object.MetadataReadLimits{SourceBytes: int64(len(data)), LogicalBytes: 3 << 20, MetadataBytes: 1024}
	file, err := object.ReadFileMetadata(context.Background(), bytes.NewReader(data), object.ReadFileOptions{}, limits)
	if err != nil {
		t.Fatal(err)
	}
	if pixel, ok := file.Dataset.Get(core.TagPixelData); !ok || pixel.Value != nil {
		t.Fatalf("deflated Pixel Data = %#v, present=%t", pixel, ok)
	}
	header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Both a complete pixel value and the file meta fit individually. Their
	// extent plus the dataset element headers exceeds the logical limit.
	limits.LogicalBytes = header.DataSetOffset + 2<<20
	file, err = object.ReadFileMetadata(context.Background(), bytes.NewReader(data), object.ReadFileOptions{}, limits)
	if file != nil || !errors.Is(err, object.ErrMetadataReadLimit) || !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
		t.Fatalf("logical ceiling returned file=%v err=%v", file != nil, err)
	}
}

func TestReadFileMetadataCancelsAndPreservesParseErrors(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	data, err := dicomtest.Part10File(syntax, core.NewRawElement(tags.PatientID, core.VRLO, []byte("META-2")))
	if err != nil {
		t.Fatal(err)
	}
	limits := object.MetadataReadLimits{SourceBytes: 2048, LogicalBytes: 2048, MetadataBytes: 2048}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &metadataTestSource{Reader: bytes.NewReader(data), cancel: cancel}
	file, err := object.ReadFileMetadata(ctx, source, object.ReadFileOptions{}, limits)
	if file != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read returned file=%v err=%v", file != nil, err)
	}

	file, err = object.ReadFileMetadata(context.Background(), bytes.NewReader(data[:len(data)-2]), object.ReadFileOptions{}, limits)
	var failure *parser.ParseError
	if file != nil || !errors.As(err, &failure) || !errors.Is(err, io.ErrUnexpectedEOF) || failure.Tag != tags.PatientID {
		t.Fatalf("truncated read returned file=%v err=%v", file != nil, err)
	}
}
