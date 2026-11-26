package roi

import (
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

var attributeBenchmarkResult *imagefilter.Volume
var attributeBenchmarkOracle []float32

func BenchmarkGrayscaleAttribute(b *testing.B) {
	for _, shape := range [][3]int{{64, 64, 1}, {24, 24, 12}} {
		name, conn := "2D", 8
		if shape[2] > 1 {
			name, conn = "3D", 26
		}
		v := imagefilter.NewVolume(shape, [3]float64{.7, 1.3, 2.5})
		for i := range v.Data {
			v.Data[i] = float32((i*37+i/17)%16) / 4
		}
		for _, closing := range []bool{false, true} {
			mode := "Open"
			if closing {
				mode = "Close"
			}
			want := attributeLevelOracle(v, 12, 1, conn, closing)
			operation := GrayscaleAttributeOpen
			if closing {
				operation = GrayscaleAttributeClose
			}
			got, err := operation(nil, v, 12, GrayscaleAttributeOptions{Connectivity: conn})
			if err != nil || !reflect.DeepEqual(got.Data, want) {
				b.Fatal("oracle equivalence before timing", err)
			}
			b.Run(name+"/"+mode+"/Tree", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(v.Data)) * 4)
				for i := 0; i < b.N; i++ {
					var err error
					attributeBenchmarkResult, err = operation(nil, v, 12, GrayscaleAttributeOptions{Connectivity: conn})
					if err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(name+"/"+mode+"/ThresholdBFS", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					attributeBenchmarkOracle = attributeLevelOracle(v, 12, 1, conn, closing)
				}
			})
		}
	}
	for _, shape := range [][3]int{{256, 256, 1}, {64, 64, 32}} {
		name, conn := "2D", 8
		if shape[2] > 1 {
			name, conn = "3D", 26
		}
		for _, continuous := range []bool{false, true} {
			v := imagefilter.NewVolume(shape, [3]float64{.7, 1.3, 2.5})
			kind := "32levels"
			if continuous {
				kind = "NativeFloat32"
			}
			for i := range v.Data {
				if continuous {
					v.Data[i] = float32((uint64(i)*2654435761)%1048573) / 1048573
				} else {
					v.Data[i] = float32((i*37 + i/17) % 32)
				}
			}
			for _, closing := range []bool{false, true} {
				operation := GrayscaleAttributeOpen
				mode := "Open"
				if closing {
					operation = GrayscaleAttributeClose
					mode = "Close"
				}
				b.Run(name+"/"+kind+"/"+mode, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(v.Data)) * 4)
					for i := 0; i < b.N; i++ {
						var err error
						attributeBenchmarkResult, err = operation(nil, v, 12, GrayscaleAttributeOptions{Connectivity: conn})
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}
