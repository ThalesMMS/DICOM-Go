package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This microbenchmark compares field I/O and coalesced header I/O inside one
// process. It does not measure syscalls, physical disk I/O or process startup.
func headerInputMetadataFixture() []byte {
	var wire []byte
	for i := 0; i < 1024; i++ {
		wire = append(wire, headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0011, uint16(0x1000+i)), "LO", 2)...)
		wire = append(wire, 'A', 'B')
	}
	return wire
}

func BenchmarkHeaderInputFile(b *testing.B) {
	wire := headerInputMetadataFixture()
	path := filepath.Join(b.TempDir(), "metadata.dcm")
	if err := os.WriteFile(path, wire, 0600); err != nil {
		b.Fatal(err)
	}
	for _, batch := range []bool{false, true} {
		name := "fields"
		if batch {
			name = "coalesced"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				f, err := os.Open(path)
				if err != nil {
					b.Fatal(err)
				}
				r := NewReader(f, transfer.ExplicitVRLittleEndian, ReaderOptions{MaxElementBytes: 32})
				r.headerInputEnabled = batch
				_, readErr := r.ReadDataSet()
				closeErr := f.Close()
				if readErr != nil || closeErr != nil {
					b.Fatalf("read=%v close=%v", readErr, closeErr)
				}
			}
		})
	}
}
