package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// countingCollector records every write, so tests can tell a slice-sized
// write from a row-sized one.
type countingCollector struct {
	data   []byte
	writes []int
}

func (w *countingCollector) Write(data []byte) (int, error) {
	w.data = append(w.data, data...)
	w.writes = append(w.writes, len(data))
	return len(data), nil
}

func float32TestSamples(count int) []float32 {
	values := make([]float32, count)
	for index := range values {
		values[index] = float32(index)*1.25 - 7
	}
	values[0] = float32(math.NaN())
	values[1] = float32(math.Inf(-1))
	values[2] = float32(math.Copysign(0, -1))
	return values
}

// packedFloat32Rows is the stream the per-sample loop produces: every row of
// the descriptor, little-endian, without its padding.
func packedFloat32Rows(values []float32, descriptor VolumeDescriptor) []byte {
	rowSamples := int(descriptor.Dimensions[0]) * int(descriptor.Components)
	var packed []byte
	for z := 0; z < int(descriptor.Dimensions[2]); z++ {
		for y := 0; y < int(descriptor.Dimensions[1]); y++ {
			first := (z*int(descriptor.SliceStrideBytes) + y*int(descriptor.RowStrideBytes)) / 4
			for _, value := range values[first : first+rowSamples] {
				packed = binary.LittleEndian.AppendUint32(packed, math.Float32bits(value))
			}
		}
	}
	return packed
}

func TestFloat32PayloadStreamsTightSlicesDirectly(t *testing.T) {
	if !hostLittleEndian() {
		t.Skip("the direct stream needs a little-endian host")
	}
	store := NewVolumeStore()
	t.Cleanup(func() { _ = store.Close() })
	descriptor := testVolumeDescriptor(5, 3, 4)
	values := float32TestSamples(5 * 3 * 4)
	if _, err := store.ReplaceFloat32(descriptor, values); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireCurrent()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release() //nolint:errcheck
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := snapshot.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	var output countingCollector
	if err := snapshot.WriteModalityF32Context(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if want := packedFloat32Rows(values, stored); !bytes.Equal(output.data, want) {
		t.Fatalf("direct stream differs from the per-sample conversion:\n got %x\nwant %x", output.data, want)
	}
	if len(output.writes) != 4 {
		t.Fatalf("writes = %v, want one per slice", output.writes)
	}
	for _, size := range output.writes {
		if size != 5*3*4 {
			t.Fatalf("writes = %v, want whole 60-byte slices", output.writes)
		}
	}
}

func TestFloat32PayloadDirectStreamSkipsRowAndSlicePadding(t *testing.T) {
	if !hostLittleEndian() {
		t.Skip("the direct stream needs a little-endian host")
	}
	// Two components, three columns: 24-byte rows in 32-byte strides, two
	// rows in a 72-byte slice stride.
	descriptor := VolumeDescriptor{
		Dimensions: [3]uint32{3, 2, 2}, Components: 2, ScalarFormat: VolumeScalarF32ModalityLE,
		RowStrideBytes: 32, SliceStrideBytes: 72,
	}
	values := float32TestSamples(2 * 72 / 4)
	payload := &float32VolumePayload{data: values}
	var output countingCollector
	done, err := payload.writeModalityDirect(context.Background(), &output, descriptor)
	if !done || err != nil {
		t.Fatalf("direct stream = %v, %v, want done", done, err)
	}
	if want := packedFloat32Rows(values, descriptor); !bytes.Equal(output.data, want) {
		t.Fatalf("padded stream:\n got %x\nwant %x", output.data, want)
	}
	if len(output.writes) != 4 || output.writes[0] != 24 {
		t.Fatalf("writes = %v, want four 24-byte rows", output.writes)
	}
}

func TestFloat32PayloadDirectStreamDeclinesWhatItCannotServe(t *testing.T) {
	tight := VolumeDescriptor{
		Dimensions: [3]uint32{4, 2, 2}, Components: 1, ScalarFormat: VolumeScalarF32ModalityLE,
		RowStrideBytes: 16, SliceStrideBytes: 32,
	}
	for name, test := range map[string]struct {
		payload    *float32VolumePayload
		descriptor func(VolumeDescriptor) VolumeDescriptor
	}{
		"nil payload":   {nil, func(d VolumeDescriptor) VolumeDescriptor { return d }},
		"short payload": {&float32VolumePayload{data: make([]float32, 15)}, func(d VolumeDescriptor) VolumeDescriptor { return d }},
		"stored format": {&float32VolumePayload{data: make([]float32, 16)}, func(d VolumeDescriptor) VolumeDescriptor {
			d.ScalarFormat = VolumeScalarI16StoredLE
			return d
		}},
		"unaligned row stride": {&float32VolumePayload{data: make([]float32, 64)}, func(d VolumeDescriptor) VolumeDescriptor {
			d.RowStrideBytes, d.SliceStrideBytes = 18, 36
			return d
		}},
		"empty volume": {&float32VolumePayload{data: make([]float32, 16)}, func(d VolumeDescriptor) VolumeDescriptor {
			d.Dimensions[2] = 0
			return d
		}},
		"slice stride overflow": {&float32VolumePayload{data: make([]float32, 16)}, func(d VolumeDescriptor) VolumeDescriptor {
			d.SliceStrideBytes = math.MaxUint64 &^ 3
			return d
		}},
	} {
		t.Run(name, func(t *testing.T) {
			var output countingCollector
			done, err := test.payload.writeModalityDirect(context.Background(), &output, test.descriptor(tight))
			if done || err != nil || len(output.writes) != 0 {
				t.Fatalf("direct stream = %v, %v after %d writes, want declined with nothing written", done, err, len(output.writes))
			}
		})
	}
}

func TestFloat32PayloadDirectStreamStopsBetweenWrites(t *testing.T) {
	if !hostLittleEndian() {
		t.Skip("the direct stream needs a little-endian host")
	}
	descriptor := VolumeDescriptor{
		Dimensions: [3]uint32{4, 2, 3}, Components: 1, ScalarFormat: VolumeScalarF32ModalityLE,
		RowStrideBytes: 16, SliceStrideBytes: 32,
	}
	payload := &float32VolumePayload{data: make([]float32, 24)}
	ctx, cancel := context.WithCancel(context.Background())
	writer := &cancelWriter{cancel: cancel}
	done, err := payload.writeModalityDirect(ctx, writer, descriptor)
	if !done || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled direct stream = %v, %v", done, err)
	}
	if writer.writes != 1 {
		t.Fatalf("writes after cancellation = %d, want 1", writer.writes)
	}
	if done, err := payload.writeModalityDirect(context.Background(), shortWriter{}, descriptor); !done || err == nil {
		t.Fatalf("short writer = %v, %v, want a reported short write", done, err)
	}
}
