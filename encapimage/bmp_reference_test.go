package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// bmpV3Reference builds a synthetic BITMAPINFOHEADER fixture. Rows and palette
// entries are already in BMP storage order, including row alignment bytes.
func bmpV3Reference(width, height int32, bits uint16, palette, gap, rows []byte) []byte {
	const headerSize = 54
	offset := headerSize + len(palette) + len(gap)
	data := make([]byte, offset+len(rows))
	copy(data, "BM")
	binary.LittleEndian.PutUint32(data[2:], uint32(len(data)))
	binary.LittleEndian.PutUint32(data[10:], uint32(offset))
	binary.LittleEndian.PutUint32(data[14:], 40)
	binary.LittleEndian.PutUint32(data[18:], uint32(width))
	binary.LittleEndian.PutUint32(data[22:], uint32(height))
	binary.LittleEndian.PutUint16(data[26:], 1)
	binary.LittleEndian.PutUint16(data[28:], bits)
	binary.LittleEndian.PutUint32(data[34:], uint32(len(rows)))
	binary.LittleEndian.PutUint32(data[46:], uint32(len(palette)/4))
	copy(data[headerSize:], palette)
	copy(data[headerSize+len(palette):], gap)
	copy(data[offset:], rows)
	return data
}

// DCMTK dcmdata/tests/ti2dbmp.cc:267-296 tests RGB ordering and row alignment.
// Distinct sample values also detect transposed axes and reversed channels.
func TestFromImagesDCMTKBMPLayouts(t *testing.T) {
	fullPalette := make([]byte, 256*4)
	copy(fullPalette[255*4:], []byte{7, 11, 19, 0})
	defaultPalette := bmpV3Reference(1, 1, 8, fullPalette, nil, []byte{255, 9, 9, 9})
	binary.LittleEndian.PutUint32(defaultPalette[46:], 0) // default 256 colors

	for _, tc := range []struct {
		name          string
		data, want    []byte
		rows, columns int
	}{
		{
			name: "bottom-up-24-bit-distinct-rows",
			data: bmpV3Reference(3, 2, 24, nil, []byte{0xde, 0xad}, []byte{
				12, 11, 10, 15, 14, 13, 18, 17, 16, 0xcf, 0xcf, 0xcf,
				3, 2, 1, 6, 5, 4, 9, 8, 7, 0xcf, 0xcf, 0xcf,
			}),
			want: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18},
			rows: 2, columns: 3,
		},
		{
			name: "top-down-32-bit-ignores-reserved-byte",
			data: bmpV3Reference(1, -2, 32, nil, nil, []byte{2, 17, 240, 0, 201, 14, 9, 127}),
			want: []byte{240, 17, 2, 9, 14, 201},
			rows: 2, columns: 1,
		},
		{
			name: "top-down-indexed-gap-and-explicit-palette",
			data: bmpV3Reference(3, -1, 8, []byte{45, 19, 8, 0, 38, 14, 92, 0, 77, 61, 32, 0},
				[]byte{99, 99, 99, 99}, []byte{2, 0, 1, 0xff}),
			want: []byte{32, 61, 77, 8, 19, 45, 92, 14, 38, 0}, // last byte is DICOM padding
			rows: 1, columns: 3,
		},
		{
			name: "default-palette-includes-index-255",
			data: defaultPalette,
			want: []byte{19, 11, 7, 0},
			rows: 1, columns: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := bytes.Clone(tc.data)
			file, report := imported(t, [][]byte{tc.data}, ImageOptions{})
			if report.Format != BMP || !report.Decoded || report.Encapsulated ||
				report.Rows != tc.rows || report.Columns != tc.columns || report.Frames != 1 || report.BitsStored != 8 {
				t.Fatalf("BMP import report = %+v", report)
			}
			written := part10Roundtrip(t, file)
			if got := getString(written.Dataset, tagPhotometric); got != "RGB" {
				t.Fatalf("Photometric Interpretation = %q, want RGB", got)
			}
			if got := getUS(t, written.Dataset, tagPlanarConfiguration); got != 0 {
				t.Fatalf("Planar Configuration = %d, want interleaved", got)
			}
			if got := nativeBytes(t, written); !bytes.Equal(got, tc.want) {
				t.Fatalf("Pixel Data = %v, want %v", got, tc.want)
			}
			if !bytes.Equal(tc.data, original) {
				t.Fatal("import modified the BMP source")
			}
		})
	}
}

// DCMTK dcmdata/tests/ti2dbmp.cc:245-265,310-316 exercises malformed dimensions,
// bit depth, physical truncation, and color table bounds.
func TestFromImagesDCMTKBMPAdmission(t *testing.T) {
	word := func(offset int, value uint16) func([]byte) []byte {
		return func(data []byte) []byte {
			binary.LittleEndian.PutUint16(data[offset:], value)
			return data
		}
	}
	dword := func(offset int, value uint32) func([]byte) []byte {
		return func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[offset:], value)
			return data
		}
	}
	valid := bmpV3Reference(2, 1, 8, []byte{12, 34, 56, 0, 78, 90, 123, 0}, nil, []byte{1, 0, 0xfe, 0xfe})
	for _, tc := range []struct {
		name   string
		change func([]byte) []byte
		limits ImportLimits
		want   error
	}{
		{name: "zero-width", change: dword(18, 0), want: ErrUnsupportedImage},
		{name: "zero-height", change: dword(22, 0), want: ErrUnsupportedImage},
		{name: "width-outside-DICOM-US", change: dword(18, 65536), want: ErrUnsupportedImage},
		{name: "minimum-signed-height", change: dword(22, 1<<31), want: ErrUnsupportedImage},
		{name: "unknown-high-bit-depth", change: word(28, 0xef00), want: ErrUnsupportedImage},
		{name: "unknown-compression", change: dword(30, 0xdead), want: ErrUnsupportedImage},
		{name: "two-planes", change: word(26, 2), want: ErrMalformedImage},
		{name: "offset-into-header", change: dword(10, 53), want: ErrMalformedImage},
		{name: "offset-past-EOF", change: dword(10, ^uint32(0)), want: ErrMalformedImage},
		{name: "palette-count-overflow", change: dword(46, 1<<31), want: ErrMalformedImage},
		{name: "declared-palette-enters-pixels", change: dword(46, 3), want: ErrMalformedImage},
		{name: "implicit-palette-is-incomplete", change: dword(46, 0), want: ErrMalformedImage},
		{name: "pixel-index-absent-from-palette", change: dword(46, 1), want: ErrMalformedImage},
		{name: "truncation-disagrees-with-file-length", change: func(data []byte) []byte {
			return data[:len(data)-1]
		}, want: ErrMalformedImage},
		{name: "truncated-row-with-consistent-file-length", change: func(data []byte) []byte {
			data = data[:len(data)-1]
			return dword(2, uint32(len(data)))(data)
		}, want: ErrMalformedImage},
		{name: "decoded-RGB-budget", limits: ImportLimits{MaxDecodedBytes: 5}, want: ErrImportLimit},
		{name: "pixel-budget", limits: ImportLimits{MaxPixels: 1}, want: ErrImportLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := bytes.Clone(valid)
			if tc.change != nil {
				data = tc.change(data)
			}
			before := bytes.Clone(data)
			file, report, err := FromImages(context.Background(), [][]byte{data}, ImageOptions{Limits: tc.limits})
			if !errors.Is(err, tc.want) || file != nil || report.Frames != 0 || report.Decoded {
				t.Fatalf("import gave file=%t report=%+v error=%v, want %v", file != nil, report, err, tc.want)
			}
			if !bytes.Equal(data, before) {
				t.Fatal("rejected import modified the BMP source")
			}
		})
	}
}
