package pixeldata_test

import (
	"context"
	"fmt"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// Storage, modality and presentation are separate stages. Stored samples are
// interpreted once (bit field and sign), the modality transform is applied
// once to them, and only presentation windows the result to display bytes.
func ExampleStoredSamples() {
	// One row of signed 13-bit samples: BitsAllocated 16, HighBit 12.
	pixels := []byte{0x30, 0x18, 0x28, 0x04, 0xFF, 0x0F, 0xE8, 0x03}
	format := pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 13, HighBit: 12, Signed: true}

	stored, err := pixeldata.StoredSamples[int16](context.Background(), pixels, format, 0)
	if err != nil {
		panic(err)
	}
	fmt.Println("stored:", stored)

	frame := display.Frame{
		Rows: 1, Columns: 4, Pixels: pixels,
		Format:   display.PixelFormat{BitsAllocated: 16, BitsStored: 13, HighBit: 12, Signed: true},
		Modality: display.ModalityLUT{Slope: 1, Intercept: -1024},
		VOI:      display.VOILUT{Center: 40, Width: 400},
	}
	modality, err := display.DecodeModality(frame)
	if err != nil {
		panic(err)
	}
	fmt.Println("modality:", modality)

	image, err := display.RenderGray(frame)
	if err != nil {
		panic(err)
	}
	fmt.Println("presentation:", image.Pix)
	// Output:
	// stored: [-2000 1064 4095 1000]
	// modality: [-3024 40 3071 -24]
	// presentation: [0 128 255 87]
}
