package nrrd_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/nrrd"
)

// TestNRRD0006 reads the NRRD0006 magic (NrrdIO in ITK 9f0a5d3888) when the
// encoding and space are supported, and refuses the features it adds.
func TestNRRD0006(t *testing.T) {
	want := []int16{1, -2, 300, -400}
	var little []byte
	for _, v := range want {
		little = binary.LittleEndian.AppendUint16(little, uint16(v))
	}
	for _, tc := range []struct {
		name, file string
	}{
		{"raw", strings.Replace(encodedHeader("raw", "little", ""), "NRRD0005", "NRRD0006", 1) + string(little)},
		{"gzip", strings.Replace(encodedHeader("gzip", "little", ""), "NRRD0005", "NRRD0006", 1) + string(gzipNRRDTestPayload(t, little))},
		{"text", strings.Replace(encodedHeader("text", "", ""), "NRRD0005", "NRRD0006", 1) + "1 -2 300 -400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, err := nrrd.Read(context.Background(), strings.NewReader(tc.file), nrrd.ReadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if image.Header.Version != 6 || image.Volume.Dimensions != [4]int{2, 2, 1, 1} {
				t.Fatalf("version %d, dimensions %v", image.Header.Version, image.Volume.Dimensions)
			}
			for i, value := range want {
				if got := image.Volume.Stored(i); got != float64(value) {
					t.Errorf("sample %d = %v, want %d", i, got, value)
				}
			}
		})
	}

	const rightUp = "NRRD0006\ntype: uint8\ndimension: 2\nspace: right-up\nsizes: 2 1\nspace directions: (1,0) (0,1)\nencoding: raw\n\n\x01\x02"
	for _, tc := range []struct {
		name    string
		file    string
		options nrrd.ReadOptions
		want    error
	}{
		{"NRRD0007", strings.Replace(encodedHeader("raw", "little", ""), "NRRD0005", "NRRD0007", 1) + string(little), nrrd.ReadOptions{}, nrrd.ErrNotNRRD},
		{"zrl encoding", strings.Replace(encodedHeader("zrl", "little", ""), "NRRD0005", "NRRD0006", 1) + "\x01\x00\x00\x06", nrrd.ReadOptions{}, nrrd.ErrUnsupported},
		{"right-up space", rightUp, nrrd.ReadOptions{}, nrrd.ErrNoPatientSpace},
		{"right-up space allowed", rightUp, nrrd.ReadOptions{AllowNonAnatomicalSpace: true}, nrrd.ErrUnsupported},
		{"right down space", strings.Replace(rightUp, "right-up", "right down", 1), nrrd.ReadOptions{}, nrrd.ErrNoPatientSpace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, err := nrrd.Read(context.Background(), strings.NewReader(tc.file), tc.options)
			if image != nil || !errors.Is(err, tc.want) {
				t.Fatalf("image %v, error %v, want %v", image, err, tc.want)
			}
		})
	}

	// SKIPLIST (and LIST) end the header with one data file per line.
	directory := t.TempDir()
	for _, list := range []string{"SKIPLIST\n0 slice0.raw\n4 slice1.raw\n", "LIST\nslice0.raw\nslice1.raw\n"} {
		header := strings.Replace(encodedHeader("raw", "little", "data file: "+list), "NRRD0005", "NRRD0006", 1)
		if _, err := nrrd.ParseHeader(bufio.NewReader(strings.NewReader(header))); !errors.Is(err, nrrd.ErrUnsupported) {
			t.Errorf("ParseHeader %q: %v", list, err)
		}
		path := filepath.Join(directory, "list.nhdr")
		if err := os.WriteFile(path, []byte(header), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := nrrd.ReadFile(context.Background(), path, nrrd.ReadOptions{}); !errors.Is(err, nrrd.ErrUnsupported) {
			t.Errorf("ReadFile %q: %v", list, err)
		}
	}
}

// TestTwoNonSpatialAxesRefused checks the documented refusal of ITK's 5D
// NRRD layout (ITK 10389fb01d: a component axis and a list axis, such as a
// time series of displacement fields) with a sentinel callers can test.
func TestTwoNonSpatialAxesRefused(t *testing.T) {
	payload := strings.Repeat("\x00\x00", 3*2*2)
	for _, tc := range []struct {
		name    string
		header  string
		options nrrd.ReadOptions
	}{
		{"vector and list", "NRRD0006\ntype: short\ndimension: 5\nspace: LPS\nsizes: 3 2 1 1 2\n" +
			"space directions: none (1,0,0) (0,1,0) (0,0,1) none\nkinds: vector domain domain domain list\nendian: little\nencoding: raw\n\n", nrrd.ReadOptions{}},
		{"RGB sequence NRRD0005", "NRRD0005\ntype: short\ndimension: 5\nspace: RAS\nsizes: 3 2 1 1 2\n" +
			"space directions: none (1,0,0) (0,1,0) (0,0,1) none\nkinds: RGB-color domain domain domain list\nendian: little\nencoding: raw\n\n", nrrd.ReadOptions{}},
		{"kinds without directions", "NRRD0006\ntype: short\ndimension: 5\nsizes: 3 2 1 1 2\n" +
			"kinds: vector domain domain domain list\nendian: little\nencoding: raw\n\n", nrrd.ReadOptions{AllowNonAnatomicalSpace: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image, err := nrrd.Read(context.Background(), strings.NewReader(tc.header+payload), tc.options)
			if image != nil || !errors.Is(err, nrrd.ErrMultipleNonSpatialAxes) || !errors.Is(err, nrrd.ErrUnsupported) {
				t.Fatalf("image %v, error %v, want %v", image, err, nrrd.ErrMultipleNonSpatialAxes)
			}
		})
	}
}
