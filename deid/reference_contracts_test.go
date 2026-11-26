package deid

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestBasicProfileSafePrivateReservationsAreLocalToEachDataset(t *testing.T) {
	const verifiedCreator = "SYNTHETIC_VERIFIED_BLOCK"
	const foreignCreator = "SYNTHETIC_UNVERIFIED_BLOCK"
	creatorTag := core.NewTag(0x0043, 0x0010)
	valueTag := core.NewTag(0x0043, 0x1025)
	relocatedCreatorTag := core.NewTag(0x0043, 0x00ff)
	relocatedValueTag := core.NewTag(0x0043, 0xff25)
	siblingsTag := core.NewTag(0x0008, 0x1115) // Referenced Series Sequence
	childTag := core.NewTag(0x0008, 0x114a)    // Referenced Instance Sequence
	registry, err := NewSafePrivateRegistry(SafePrivateProvenance{
		Version: "synthetic-v1", Checksum: strings.Repeat("a", 64),
	}, []SafePrivateRule{{Tag: valueTag, Creator: verifiedCreator, VR: core.VRDS, VM: 1, Action: ProfileActionKeep}})
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultBasicProfileOptions()
	options.SelectedOptions = []ProfileOption{ProfileOptionRetainSafePrivate}
	options.SafePrivateRegistry = registry
	for _, tc := range []struct {
		name      string
		root      []core.Element
		first     []core.Element
		second    []core.Element
		wantFirst bool
	}{
		{
			name:  "root reservation does not authorize a child",
			root:  []core.Element{dicomtest.NewStringElement(creatorTag, core.VRLO, verifiedCreator)},
			first: []core.Element{dicomtest.NewStringElement(valueTag, core.VRDS, "227.625")},
		},
		{
			name: "child reservation does not authorize an ancestor or following sibling",
			root: []core.Element{dicomtest.NewStringElement(valueTag, core.VRDS, "227.625")},
			first: []core.Element{
				dicomtest.NewStringElement(creatorTag, core.VRLO, verifiedCreator),
				dicomtest.NewStringElement(valueTag, core.VRDS, "1.75"),
			},
			second:    []core.Element{dicomtest.NewStringElement(valueTag, core.VRDS, "227.625")},
			wantFirst: true,
		},
		{
			name: "relocated reservation survives while a competing block does not",
			first: []core.Element{
				dicomtest.NewStringElement(creatorTag, core.VRLO, foreignCreator),
				dicomtest.NewStringElement(valueTag, core.VRDS, "227.625"),
				dicomtest.NewStringElement(relocatedCreatorTag, core.VRLO, verifiedCreator),
				dicomtest.NewStringElement(relocatedValueTag, core.VRDS, "1.75"),
			},
			second: []core.Element{
				dicomtest.NewStringElement(relocatedCreatorTag, core.VRLO, foreignCreator),
				dicomtest.NewStringElement(relocatedValueTag, core.VRDS, "227.625"),
			},
			wantFirst: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				subtest := "listed order"
				if reverse {
					subtest = "reversed order"
				}
				t.Run(subtest, func(t *testing.T) {
					// An unreserved grandchild must be cleaned even when its parent has an active block.
					grandchild := core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(valueTag, core.VRDS, "227.625")}}
					first := core.DataSet{Elements: append(append([]core.Element(nil), tc.first...), profileSequence(childTag, grandchild))}
					second := core.DataSet{Elements: tc.second}
					items := []core.DataSet{first, second}
					if reverse {
						items[0], items[1] = items[1], items[0]
					}
					obj := object.FromElements(append(append([]core.Element(nil), tc.root...), profileSequence(siblingsTag, items...)), nil)
					report, err := ApplyBasicProfile(context.Background(), obj, options, nil)
					if err != nil {
						t.Fatal(err)
					}
					got, ok := obj.GetSequence(siblingsTag)
					if !ok || len(got) != 2 {
						t.Fatalf("expected two retained sequence items, got %d", len(got))
					}
					if reverse {
						got[0], got[1] = got[1], got[0]
					}
					nested, ok := got[0].GetSequence(childTag)
					if !ok || len(nested) != 1 {
						t.Fatal("expected the descendant sequence item to survive")
					}
					for _, ds := range []*object.Object{obj, got[1], nested[0]} {
						for _, tag := range []core.Tag{creatorTag, valueTag, relocatedCreatorTag, relocatedValueTag} {
							if ds.Has(tag) {
								t.Errorf("private element %s escaped its owning dataset", tag)
							}
						}
					}
					verifiedBlocks := 0
					for _, block := range [][2]core.Tag{{creatorTag, valueTag}, {relocatedCreatorTag, relocatedValueTag}} {
						creator, _ := got[0].GetString(block[0])
						value, _ := got[0].GetString(block[1])
						if creator == verifiedCreator && value == "1.75" {
							verifiedBlocks++
						} else if got[0].Has(block[0]) || got[0].Has(block[1]) {
							t.Errorf("unexpected private block at %s: %q, %q", block[0], creator, value)
						}
					}
					wantBlocks := 0
					if tc.wantFirst {
						wantBlocks = 1
					}
					if verifiedBlocks != wantBlocks || report.Summary.PrivateRetained != 2*wantBlocks {
						t.Errorf("retained %d complete blocks and reported %d private elements, want %d and %d", verifiedBlocks, report.Summary.PrivateRetained, wantBlocks, 2*wantBlocks)
					}
					encoded, err := json.Marshal(report)
					if err != nil || bytes.Contains(encoded, []byte(verifiedCreator)) || bytes.Contains(encoded, []byte(foreignCreator)) || bytes.Contains(encoded, []byte("227.625")) {
						t.Fatalf("private report contains input values or is not serializable: %v", err)
					}
				})
			}
		})
	}
}

func TestDeidentifyPathMetadataOnlyTransferSyntaxContracts(t *testing.T) {
	const sourcePerson = "Álvarez^Lúcia"
	const sourceSOP = "2.25.841.63.417.1"
	const sopClass = "1.2.840.10008.5.1.4.1.1.7"
	charsetTag := core.NewTag(0x0008, 0x0005)
	for _, ts := range []transfer.Syntax{
		transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian,
		transfer.DeflatedExplicitVRLittleEndian, transfer.EncapsulatedUncompressedExplicitVRLittleEndian,
	} {
		t.Run(ts.Name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.dcm")
			var pixelValue core.Element
			var bits uint16 = 16
			if ts.Encapsulated {
				bits = 8
				offsets := binary.LittleEndian.AppendUint32(nil, 0)
				offsets = binary.LittleEndian.AppendUint32(offsets, 10) // 8-byte item header plus 2 bytes of frame data
				pixelValue = dicomtest.NewFragmentSequenceElement(tags.PixelData, offsets, []byte{0x17, 0xff}, []byte{0x80, 0x91})
			} else {
				packed := make([]byte, 8)
				for i, n := range []uint16{0xab12, 0x01cd, 0x0080, 0xfe42} {
					ts.ByteOrder.PutUint16(packed[2*i:], n)
				}
				pixelValue = core.NewRawElement(tags.PixelData, core.VROW, packed)
			}
			data := object.FromElements([]core.Element{
				dicomtest.NewUIElement(tags.SOPClassUID, sopClass), dicomtest.NewUIElement(tags.SOPInstanceUID, sourceSOP),
				dicomtest.NewUIElement(tags.StudyInstanceUID, "2.25.841.63.100"), dicomtest.NewUIElement(tags.SeriesInstanceUID, "2.25.841.63.100.1"),
				dicomtest.NewPNElement(tags.PatientName, sourcePerson), dicomtest.NewStringElement(charsetTag, core.VRCS, "ISO_IR 100"),
				dicomtest.NewStringElement(tags.BurnedInAnnotation, core.VRCS, "NO"),
				dicomtest.NewStringElement(tags.PhotometricInterpretation, core.VRCS, "MONOCHROME2"),
				dicomtest.NewStringElement(tags.NumberOfFrames, core.VRIS, "2"),
				dicomtest.Uint16Element(tags.Rows, core.VRUS, ts.ByteOrder, 1), dicomtest.Uint16Element(tags.Columns, core.VRUS, ts.ByteOrder, 2),
				dicomtest.Uint16Element(tags.SamplesPerPixel, core.VRUS, ts.ByteOrder, 1), dicomtest.Uint16Element(tags.BitsAllocated, core.VRUS, ts.ByteOrder, bits),
				dicomtest.Uint16Element(tags.BitsStored, core.VRUS, ts.ByteOrder, bits), dicomtest.Uint16Element(tags.HighBit, core.VRUS, ts.ByteOrder, bits-1),
				dicomtest.Uint16Element(tags.PixelRepresentation, core.VRUS, ts.ByteOrder, 0), pixelValue,
			}, nil)
			data.SetValueByteOrder(ts.ByteOrder)
			var input bytes.Buffer
			if err := object.WriteFile(&input, &object.File{Dataset: data, TransferSyntax: ts, Preamble: bytes.Repeat([]byte{0xA5}, 128)}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, input.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			original, err := object.ReadFile(bytes.NewReader(input.Bytes()))
			if err != nil {
				t.Fatalf("read source fixture: %v", err)
			}
			defer original.Close()
			if got, err := original.Dataset.LookupString(tags.PatientName); err != nil || got != sourcePerson {
				t.Fatalf("source fixture Patient Name = %q, %v", got, err)
			}
			for _, method := range []string{"compatibility", "basic profile"} {
				t.Run(method, func(t *testing.T) {
					dest := filepath.Join(dir, method+".dcm")
					uids := NewKeyedUIDRemapper([]byte("synthetic file test key"))
					var err error
					if method == "compatibility" {
						_, err = AnonymizePath(context.Background(), source, dest, Options{}, uids)
					} else {
						_, err = BasicProfilePath(context.Background(), source, dest, DefaultBasicProfileOptions(), uids)
					}
					after, readErr := os.ReadFile(source)
					if readErr != nil || !bytes.Equal(after, input.Bytes()) {
						t.Fatalf("source changed or disappeared after de-identification: %v", readErr)
					}
					if err != nil {
						t.Fatal(err)
					}
					output, err := object.OpenFile(dest)
					if err != nil {
						t.Fatal(err)
					}
					defer output.Close()
					gotPixels, ok := output.Dataset.Get(tags.PixelData)
					if !ok || !reflect.DeepEqual(gotPixels.Value, pixelValue.Value) {
						t.Errorf("Pixel Data representation changed from %#v to %#v", pixelValue.Value, gotPixels.Value)
					}
					if got := output.TransferSyntax.UID; got != ts.UID {
						t.Errorf("transfer syntax changed to %s, want %s", got, ts.UID)
					}
					if got, _ := output.Dataset.GetUID(tags.SOPInstanceUID); got != uids.Map(sourceSOP) || got == sourceSOP || got != trimmedUID(output.Meta, tags.MediaStorageSOPInstanceUID) {
						t.Errorf("reopened SOP Instance UID differs from the shared mapping or File Meta: %q", got)
					}
					if got, _ := output.Meta.GetUID(tags.MediaStorageSOPClassUID); got != sopClass || got != trimmedUID(output.Dataset, tags.SOPClassUID) {
						t.Errorf("File Meta SOP Class mismatch: %q", got)
					}
					if got, _ := output.Dataset.LookupString(tags.PatientName); got == sourcePerson {
						t.Error("source patient name survived")
					}
					if got, _ := output.Dataset.GetString(charsetTag); got != "ISO_IR 100" {
						t.Errorf("Specific Character Set changed to %q", got)
					}
					if !bytes.Equal(output.Preamble, make([]byte, 128)) {
						t.Error("output leaked the source preamble")
					}
				})
			}
		})
	}
}

func TestBasicProfileMalformedModifiedDatesRefuseWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		tag         core.Tag
		vr          core.VR
	}{
		{"non-leap February 29", "20230229", tags.StudyDate, core.VRDA},
		{"day zero", "20251200", tags.StudyDate, core.VRDA},
		{"nonnumeric month", "2024AB01", tags.StudyDate, core.VRDA},
		{"hour 24", "20250413240000", tags.AcquisitionDateTime, core.VRDT},
		{"invalid offset", "20250413030405+1260", tags.AcquisitionDateTime, core.VRDT},
		{"fraction without seconds", "202504130304.1-0200", tags.AcquisitionDateTime, core.VRDT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := object.FromElements([]core.Element{
				dicomtest.NewPNElement(tags.PatientName, "Synthetic^Date"),
				dicomtest.NewStringElement(tc.tag, tc.vr, tc.value),
			}, nil)
			before := ds.ToDataSet()
			opts := DefaultBasicProfileOptions()
			opts.SelectedOptions = []ProfileOption{ProfileOptionRetainModifiedDates}
			opts.DateShiftPolicy = func(context.Context, DateShiftContext) (int, error) { return -3, nil }
			if _, err := ApplyBasicProfile(context.Background(), ds, opts, NewUIDRemapper()); !errors.Is(err, ErrProfileCallback) || strings.Contains(err.Error(), tc.value) {
				t.Fatalf("modified dates should fail with a redacted profile error, got %v", err)
			}
			if !reflect.DeepEqual(before, ds.ToDataSet()) {
				t.Fatal("rejected date shift changed input data")
			}
		})
	}
}
