package registration

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestTransformFileAggregateBudget(t *testing.T) {
	a, b := NewTranslation3D(), NewTranslation3D()
	composite, err := NewComposite(a, b)
	if err != nil {
		t.Fatal(err)
	}
	for name, writer := range map[string]func(*bytes.Buffer) error{
		"text": func(out *bytes.Buffer) error { return WriteITKText(out, composite) },
		"hdf5": func(out *bytes.Buffer) error { return WriteITKHDF5(out, composite) },
	} {
		t.Run(name, func(t *testing.T) {
			var data bytes.Buffer
			if err := writer(&data); err != nil {
				t.Fatal(err)
			}
			// Each component fits; their sum does not. Check admission before
			// the second parameter payload is instantiated.
			got, err := ReadTransformFileContext(context.Background(), data.Bytes(), TransformFileReadOptions{MaxParameters: 5})
			if got != nil || !errors.Is(err, ErrTransformFile) {
				t.Fatalf("%v %v", got, err)
			}
			if _, err := ReadTransformFileContext(context.Background(), data.Bytes(), TransformFileReadOptions{MaxParameters: 6}); err != nil {
				t.Fatal(err)
			}
		})
	}
	// Repeated parameter lines cannot evade the cumulative text cap.
	data := []byte(ITKTextHeader + "\nTransform: TranslationTransform_double_3_3\nParameters: 0 0 0\nParameters: 1 2 3\nFixedParameters:\n")
	if got, err := ReadTransformFileContext(context.Background(), data, TransformFileReadOptions{MaxParameters: 5}); got != nil || err == nil {
		t.Fatal("duplicate lines bypassed budget")
	}
}

func TestTransformFileCancellationBeforeAllocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := ReadTransformFileContext(ctx, nil, TransformFileReadOptions{}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := TransformDisplacementField(ctx, nil, Grid{Size: [3]int{1 << 20, 1 << 20, 1 << 20}}); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("%v %v", got, err)
	}
	r := &transformContextReader{ctx: ctx, Reader: bytes.NewReader([]byte("abc"))}
	if n, err := r.Read(make([]byte, 3)); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
	if n, err := r.ReadAt(make([]byte, 3), 0); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal(n, err)
	}
}
