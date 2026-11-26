// Command json2dcm converts DICOM JSON (PS3.18 Annex F) to a DICOM Part 10 file.
package main

import (
	"os"

	"github.com/ThalesMMS/dicom-go/internal/dicomconv"
)

func main() {
	os.Exit(dicomconv.Run(dicomconv.JSON2DCM, os.Args[1:], os.Stdout, os.Stderr))
}
