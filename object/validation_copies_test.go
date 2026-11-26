package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestValidatedObjectBorrowingPreservesDeferredSourceAndLifetime(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline} {
		el := dicomtest.NewOBElement(core.TagPixelData, []byte{1, 2, 3, 4})
		if syntax.Encapsulated {
			el = dicomtest.NewFragmentSequenceElement(core.TagPixelData, []byte{0, 0, 0, 0}, []byte{0xff, 0xd8, 0xff, 0xd9})
		}
		data := dicomtest.EncodeElements(syntax, el)
		obj, _, err := ReadDataSetWithValidation(context.Background(), bytes.NewReader(data), syntax, ReadFileOptions{DeferPixelData: true}, validation.Options{})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		result, err := WriteDataSetWithValidation(context.Background(), &out, obj, syntax, validation.Options{WriteLimits: validation.WriteLimits{MaxCopiedValueBytes: 1}})
		if err != nil || !result.Complete || !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("syntax=%s result=%#v err=%v", syntax.UID, result, err)
		}
		deferred, _ := obj.Get(core.TagPixelData)
		if deferred.Value != nil {
			t.Fatal("writer materialized deferred payload")
		}
		var copy bytes.Buffer
		if _, err = obj.CopyValueTo(core.TagPixelData, &copy); err != nil {
			t.Fatal(err)
		}
		if copy.Len() == 0 {
			t.Fatal("writer invalidated reusable source")
		}
		obj.Close()
	}
}
func TestValidatedObjectRejectsStaleUIDBeforeDeflatePublication(t *testing.T) {
	tag := core.NewTag(0x0008, 0x0018)
	obj := FromElements([]core.Element{core.NewRawElement(tag, core.VRUI, []byte("1.20"))}, std.Dictionary)
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "replace", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
		el := core.NewRawElement(tag, core.VRUI, []byte("1..2"))
		return validation.HookDecision{Element: &el}, nil
	})})
	var out bytes.Buffer
	result, err := WriteDataSetWithValidation(context.Background(), &out, obj, transfer.DeflatedExplicitVRLittleEndian, validation.Options{Mode: validation.ModeStrict, Hooks: chain})
	if !errors.Is(err, validation.ErrValidationFailed) || out.Len() != 0 || result.Complete || result.BytesWritten != 0 {
		t.Fatalf("result=%#v err=%v bytes=%d", result, err, out.Len())
	}
	el, _ := obj.Get(tag)
	if string(el.Value.(core.RawValue)) != "1.20" {
		t.Fatal("source changed")
	}
}
func TestValidatedObjectPreflightAndCancellationDoNotPublish(t *testing.T) {
	obj := FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("AB"))}, std.Dictionary)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		ctx  context.Context
		opts validation.Options
		want error
	}{{ctx, validation.Options{}, context.Canceled}, {context.Background(), validation.Options{WriteLimits: validation.WriteLimits{MaxWorkBytes: 1}}, validation.ErrValidationLimit}} {
		result, err := WriteDataSetWithValidation(test.ctx, io.Discard, obj, transfer.ExplicitVRLittleEndian, test.opts)
		if !errors.Is(err, test.want) || result.BytesWritten != 0 || result.Complete {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	}
}
