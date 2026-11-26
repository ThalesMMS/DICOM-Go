// Command dcmpsmk exposes the corresponding reusable library capability.
package main

import (
	"github.com/ThalesMMS/dicom-go/internal/presentationcli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func main() { wrappercli.Main("dcmpsmk", presentationcli.Execute) }
