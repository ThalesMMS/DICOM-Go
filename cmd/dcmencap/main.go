// Command dcmencap exposes the corresponding reusable library capability.
package main

import (
	"github.com/ThalesMMS/dicom-go/internal/doccli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func main() { wrappercli.Main("dcmencap", doccli.Execute) }
