package object_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestBulkDescriptorReferencePart10AndRawPrefixes(t *testing.T) {
	tag := core.NewTag(0x0042, 0x0011)
	raw := []byte{1, 2, 3, 4, 5, 6}
	elements := append(dicomtest.MinimalDataset(), core.NewRawElement(tag, core.VROB, raw))
	dataset := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, elements...)
	part10, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	opts := object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB}}}}
	for _, fileMode := range []bool{false, true} {
		data := dataset
		if fileMode {
			data = part10
		}
		source := bytes.NewReader(append(make([]byte, 71), data...))
		source.Seek(71, io.SeekStart)
		var obj *object.Object
		if fileMode {
			f, e := object.ReadFileWithOptions(source, opts)
			if e != nil {
				t.Fatal(e)
			}
			obj = f.Dataset
			for _, elem := range f.Meta.Elements() {
				if elem.Value == nil {
					t.Fatalf("deferred File Meta %s", elem.Tag())
				}
			}
		} else {
			obj, err = object.ReadDataSetWithOptions(source, transfer.ExplicitVRLittleEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
		}
		path := []parser.BulkDataPathStep{{Tag: tag, ItemIndex: -1}}
		ref, ok, err := obj.DeferredBulkDataReference(path)
		if err != nil || !ok {
			t.Fatalf("reference %v %v", ok, err)
		}
		wantOffset := int64(71 + bytes.Index(data, raw))
		if ref.Offset != wantOffset || ref.Length != 6 {
			t.Fatalf("offset=%d want=%d length=%d", ref.Offset, wantOffset, ref.Length)
		}
		before, _ := source.Seek(0, io.SeekCurrent)
		var got bytes.Buffer
		if n, err := obj.CopyBulkDataValueTo(context.Background(), path, &got); err != nil || n != 6 || !bytes.Equal(raw, got.Bytes()) {
			t.Fatalf("copy n=%d err=%v", n, err)
		}
		after, _ := source.Seek(0, io.SeekCurrent)
		if before != after {
			t.Fatal("replay changed source cursor")
		}
		got.Reset()
		if n, err := obj.CopyBulkDataRangeTo(context.Background(), path, 2, 3, &got); err != nil || n != 3 || !bytes.Equal(raw[2:5], got.Bytes()) {
			t.Fatalf("range replay n=%d err=%v value=%v", n, err, got.Bytes())
		}
		after, _ = source.Seek(0, io.SeekCurrent)
		if before != after {
			t.Fatal("range replay changed source cursor")
		}
		for _, invalid := range [][2]int64{{-1, 1}, {0, -1}, {5, 2}, {7, 0}} {
			got.Reset()
			if n, err := obj.CopyBulkDataRangeTo(context.Background(), path, invalid[0], invalid[1], &got); err == nil || n != 0 || got.Len() != 0 {
				t.Fatalf("invalid range %v copied n=%d bytes=%d err=%v", invalid, n, got.Len(), err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := obj.CopyBulkDataValueTo(ctx, path, io.Discard); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}

type bulkUnreadable struct{ reads int }

func (r *bulkUnreadable) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("unexpected read")
}

func TestBulkDescriptorAlternateReadersRejectBeforeRead(t *testing.T) {
	opts := object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{}}}}
	for _, read := range []func(io.Reader) error{
		func(r io.Reader) error { _, err := object.ReadFileWithOptions(r, opts); return err },
		func(r io.Reader) error {
			_, err := object.ReadDataSetWithOptions(r, transfer.ExplicitVRLittleEndian, opts)
			return err
		},
		func(r io.Reader) error {
			_, _, err := object.ReadDataSetWithValidation(context.Background(), r, transfer.ExplicitVRLittleEndian, opts, validation.Options{})
			return err
		},
		func(r io.Reader) error {
			_, _, err := object.ReadDataSetWithTransferSyntaxRecovery(r, opts, object.TransferSyntaxRecoveryOptions{})
			return err
		},
		func(r io.Reader) error {
			_, _, err := object.ReadFileWithTransferSyntaxRecovery(r, opts, object.TransferSyntaxRecoveryOptions{})
			return err
		},
	} {
		source := &bulkUnreadable{}
		if err := read(source); !errors.Is(err, object.ErrDeferredValueRequiresSeekable) || source.reads != 0 {
			t.Fatalf("err=%v reads=%d", err, source.reads)
		}
	}
	for _, read := range []func(io.ReadSeeker) error{
		func(r io.ReadSeeker) error {
			_, err := object.ReadDataSetWithOptions(r, transfer.DeflatedExplicitVRLittleEndian, opts)
			return err
		},
		func(r io.ReadSeeker) error {
			_, _, err := object.ReadDataSetWithValidation(context.Background(), r, transfer.DeflatedExplicitVRLittleEndian, opts, validation.Options{})
			return err
		},
	} {
		source := bytes.NewReader([]byte("not a compressed data set"))
		if err := read(source); !errors.Is(err, object.ErrDeferredValueRequiresSeekable) {
			t.Fatal(err)
		}
		pos, _ := source.Seek(0, io.SeekCurrent)
		if pos != 0 {
			t.Fatal("read deflated payload")
		}
	}
}

func TestBulkDescriptorFileOwnsSourceAndRefusesClosedOrDuplicate(t *testing.T) {
	tag := core.NewTag(0x0042, 0x0011)
	elem := core.NewRawElement(tag, core.VROB, []byte{1, 2, 3, 4})
	wire, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, append(dicomtest.MinimalDataset(), elem)...)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "synthetic.dcm")
	if err := os.WriteFile(filename, wire, 0600); err != nil {
		t.Fatal(err)
	}
	opts := object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB}}}}
	f, err := object.OpenFileWithOptions(filename, opts)
	if err != nil {
		t.Fatal(err)
	}
	path := []parser.BulkDataPathStep{{Tag: tag, ItemIndex: -1}}
	if _, ok, err := f.Dataset.DeferredBulkDataReference(path); err != nil || !ok {
		t.Fatalf("ref %v %v", ok, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Dataset.DeferredBulkDataReference(path); err == nil {
		t.Fatal("closed source accepted")
	}
	if _, err := f.Dataset.CopyBulkDataValueTo(context.Background(), path, io.Discard); err == nil {
		t.Fatal("closed source replayed")
	}
	duplicate := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, elem, elem)
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(duplicate), transfer.ExplicitVRLittleEndian, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := obj.DeferredBulkDataReference(path); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestBulkReferenceRejectsForeignSequenceWithSameOffsets(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	leaf := core.NewTag(0x0042, 0x0011)
	opts := object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB}}}}
	read := func(value byte) *object.Object {
		wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.NewSequenceElement(seq,
			core.DataSet{}, core.DataSet{Elements: []core.Element{core.NewRawElement(leaf, core.VROB, []byte{value, value, value, value})}}))
		obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, opts)
		if err != nil {
			t.Fatal(err)
		}
		return obj
	}
	a, b := read(1), read(2)
	path := []parser.BulkDataPathStep{{Tag: seq, ItemIndex: 1}, {Tag: leaf, ItemIndex: -1}}
	// Repeated facades and COW changes to one facade retain the original root.
	for i := 0; i < 3; i++ {
		items, _ := a.GetSequence(seq)
		items[1].Put(core.NewRawElement(core.NewTag(0x0011, 0x1002), core.VROB, []byte{9, 9}))
		if _, ok, err := a.DeferredBulkDataReference(path); err != nil || !ok {
			t.Fatalf("legitimate facade %v %v", ok, err)
		}
	}
	wrongPath := []parser.BulkDataPathStep{{Tag: seq, ItemIndex: 0}, {Tag: leaf, ItemIndex: -1}}
	if _, ok, err := a.DeferredBulkDataReference(wrongPath); err != nil || ok {
		t.Fatalf("empty item reference %v %v", ok, err)
	}
	foreign, _ := b.Get(seq)
	a.Put(foreign)
	if _, _, err := a.DeferredBulkDataReference(path); err == nil {
		t.Fatal("foreign SQ reference accepted")
	}
	if _, err := a.CopyBulkDataValueTo(context.Background(), path, io.Discard); err == nil {
		t.Fatal("foreign SQ replayed old bytes")
	}
	// Renaming the original sequence breaks its full recorded path as well.
	c := read(3)
	original, _ := c.Get(seq)
	renamed := core.NewTag(0x0008, 0x1140)
	original.Header.Tag = renamed
	c.Put(original)
	changedPath := []parser.BulkDataPathStep{{Tag: renamed, ItemIndex: 1}, {Tag: leaf, ItemIndex: -1}}
	if _, _, err := c.DeferredBulkDataReference(changedPath); err == nil {
		t.Fatal("changed path accepted")
	}
}
