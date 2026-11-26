package object

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var objectCapacity1301Sink *Object
var elementCapacity1301Sink int

// The same stable element slices are used in both source snapshots. Value
// creation is outside timing; this isolates Object's scan and storage costs.
func BenchmarkObjectCapacity1301(b *testing.B) {
	for _, count := range []int{8, 128, 1024} {
		for _, order := range []string{"sorted", "unsorted", "dense_duplicates"} {
			elements := benchmarkCapacityElements1301(count, order)
			b.Run(fmt.Sprintf("%s/attributes_%d", order, count), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					objectCapacity1301Sink = FromElements(elements, std.Dictionary)
				}
			})
		}
	}
}

func BenchmarkElementCapacityScan1301(b *testing.B) {
	for _, count := range []int{8, 128, 1024} {
		for _, order := range []string{"sorted", "unsorted", "dense_duplicates"} {
			elements := benchmarkCapacityElements1301(count, order)
			b.Run(fmt.Sprintf("%s/attributes_%d", order, count), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					elementCapacity1301Sink = elementCapacityHint(elements)
				}
			})
		}
	}
}

func BenchmarkReadMetadata1301(b *testing.B) {
	for _, count := range []int{8, 128, 1024} {
		elements := benchmarkCapacityElements1301(count, "sorted")
		wire, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(fmt.Sprintf("attributes_%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(wire)))
			for i := 0; i < b.N; i++ {
				file, err := ReadFile(bytes.NewReader(wire))
				if err != nil {
					b.Fatal(err)
				}
				objectCapacity1301Sink = file.Dataset
			}
		})
	}
}

func benchmarkCapacityElements1301(count int, order string) []core.Element {
	elements := make([]core.Element, count)
	for i := range elements {
		index := i
		switch order {
		case "unsorted":
			index = count - i - 1
		case "dense_duplicates":
			index = i % 4
		}
		elements[i] = core.NewRawElement(core.NewTag(8, 0x1000+uint16(index)), core.VRLO, []byte("VALUE "))
	}
	return elements
}
