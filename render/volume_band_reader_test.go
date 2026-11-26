package render

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

// writeRecorder records each write's length.
type writeRecorder struct {
	bytes.Buffer
	writes []int
}

func (w *writeRecorder) Write(data []byte) (int, error) {
	w.writes = append(w.writes, len(data))
	return w.Buffer.Write(data)
}

// TestVolumeBandReaderWritesWholeSlices checks that an even-slice band
// streams each slice in one write, with the bytes of the row encoding
// (#1626), for float32 modality and 16-bit stored samples.
func TestVolumeBandReaderWritesWholeSlices(t *testing.T) {
	const cols, rows, depth = 5, 3, 7
	count := cols * rows * depth
	floats := make([]float32, count)
	signed := make([]int16, count)
	unsigned := make([]uint16, count)
	for i := range count {
		floats[i] = float32(i)*1.5 - 40
		signed[i] = int16(i*37 - 300)
		unsigned[i] = uint16(i*911 + 7)
	}
	evenSlices := func(sampleBytes int, encode func(index int, out []byte)) []byte {
		var want []byte
		for z := 0; z < depth; z += 2 {
			for i := z * cols * rows; i < (z+1)*cols*rows; i++ {
				sample := make([]byte, sampleBytes)
				encode(i, sample)
				want = append(want, sample...)
			}
		}
		return want
	}
	cases := []struct {
		name   string
		reader VolumeBandReader
		stored bool
		want   []byte
	}{
		{"float32", VolumeBandReader{cols: cols, rows: rows, start: 0, end: depth, step: 2, float32Data: floats}, false,
			evenSlices(4, func(i int, out []byte) { binary.LittleEndian.PutUint32(out, math.Float32bits(floats[i])) })},
		{"signed", VolumeBandReader{cols: cols, rows: rows, start: 0, end: depth, step: 2, packed: &packed16VolumePayload{signed: signed}}, true,
			evenSlices(2, func(i int, out []byte) { binary.LittleEndian.PutUint16(out, uint16(signed[i])) })},
		{"unsigned", VolumeBandReader{cols: cols, rows: rows, start: 0, end: depth, step: 2, packed: &packed16VolumePayload{unsigned: unsigned}}, true,
			evenSlices(2, func(i int, out []byte) { binary.LittleEndian.PutUint16(out, unsigned[i]) })},
	}
	for _, test := range cases {
		var output writeRecorder
		var err error
		if test.stored {
			err = test.reader.WriteStored16To(&output)
		} else {
			err = test.reader.WriteModalityF32To(&output)
		}
		if err != nil || !bytes.Equal(output.Bytes(), test.want) {
			t.Fatalf("%s: %v, bytes differ from the row encoding", test.name, err)
		}
		if hostLittleEndian() && len(output.writes) != (depth+1)/2 {
			t.Fatalf("%s: %d writes, want one per even slice", test.name, len(output.writes))
		}
		retained, err := test.reader.Retain()
		if err != nil {
			t.Fatal(err)
		}
		output = writeRecorder{}
		if test.stored {
			err = retained.WriteStored16To(&output)
		} else {
			err = retained.WriteModalityF32To(&output)
		}
		if err != nil || !bytes.Equal(output.Bytes(), test.want) {
			t.Fatalf("%s retained: %v, bytes differ", test.name, err)
		}
	}
}
