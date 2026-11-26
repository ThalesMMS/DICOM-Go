package nrrd_test

import (
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nrrd"
)

func TestReadVerifiesCompressedPayload(t *testing.T) {
	voxels := []byte{1, 0, 254, 255, 44, 1, 112, 254} // int16: 1, -2, 300, -400
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	if _, err := gz.Write(voxels); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	bz, err := hex.DecodeString("425a68393141592653597ee83263000000d080e000000440000001a00022186830056100c2ee48a70a120fdd064c60")
	if err != nil {
		t.Fatal(err)
	}
	for _, codec := range []struct {
		name          string
		data          []byte
		corruptOffset int // position of file checksum, relative to the end
		decompress    func(io.Reader) (io.Reader, error)
	}{
		{"gzip", compressed.Bytes(), 8, func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }},
		{"bzip2", bz, 3, func(r io.Reader) (io.Reader, error) { return bzip2.NewReader(r), nil }},
	} {
		corrupt := bytes.Clone(codec.data)
		corrupt[len(corrupt)-codec.corruptOffset] ^= 1
		for _, variant := range []struct {
			name string
			data []byte
		}{
			{"intact", codec.data},
			{"corrupt", corrupt},
			{"truncated", codec.data[:len(codec.data)-2]},
		} {
			r, err := codec.decompress(bytes.NewReader(variant.data))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(r)
			if !bytes.Equal(decoded, voxels) || (err == nil) != (variant.name == "intact") {
				t.Fatalf("%s %s fixture decoded %v, %v", codec.name, variant.name, decoded, err)
			}
			for _, layout := range []struct {
				name   string
				header string
				want   []float64
			}{
				{"scalar", encodedHeader(codec.name, "little", ""), []float64{1, -2, 300, -400}},
				{"interleaved", fmt.Sprintf("NRRD0005\ntype: short\ndimension: 4\nspace: LPS\nsizes: 2 2 1 1\nspace directions: none (1,0,0) (0,1,0) (0,0,1)\nencoding: %s\nendian: little\n\n", codec.name), []float64{1, 300, -2, -400}},
			} {
				t.Run(codec.name+"/"+layout.name+"/"+variant.name, func(t *testing.T) {
					input := io.MultiReader(strings.NewReader(layout.header), bytes.NewReader(variant.data))
					image, err := nrrd.Read(context.Background(), input, nrrd.ReadOptions{})
					if variant.name != "intact" {
						if err == nil || image != nil {
							t.Fatalf("Read returned image present = %t, error = %v; want nil image and a trailer error", image != nil, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					for i, want := range layout.want {
						if got := image.Volume.Stored(i); got != want {
							t.Fatalf("voxel %d = %g, want %g", i, got, want)
						}
					}
				})
			}
		}
	}
}
