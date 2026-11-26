// Command dcm2xml converts a DICOM Part 10 file to the Native DICOM Model XML (PS3.19 Annex A).
package main

import (
	"os"

	"github.com/ThalesMMS/dicom-go/internal/dicomconv"
)

func main() {
	os.Exit(dicomconv.Run(dicomconv.DCM2XML, os.Args[1:], os.Stdout, os.Stderr))
}
