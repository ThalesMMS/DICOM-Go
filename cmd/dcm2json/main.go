// Command dcm2json converts a DICOM Part 10 file to DICOM JSON (PS3.18 Annex F).
package main

import (
	"os"

	"github.com/ThalesMMS/dicom-go/internal/dicomconv"
)

func main() {
	os.Exit(dicomconv.Run(dicomconv.DCM2JSON, os.Args[1:], os.Stdout, os.Stderr))
}
