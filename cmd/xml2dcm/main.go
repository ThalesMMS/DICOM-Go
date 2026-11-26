// Command xml2dcm converts Native DICOM Model XML (PS3.19 Annex A) to a DICOM Part 10 file.
package main

import (
	"os"

	"github.com/ThalesMMS/dicom-go/internal/dicomconv"
)

func main() {
	os.Exit(dicomconv.Run(dicomconv.XML2DCM, os.Args[1:], os.Stdout, os.Stderr))
}
