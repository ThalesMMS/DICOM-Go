// Command dsr2html exposes the corresponding reusable library capability.
package main

import (
	"github.com/ThalesMMS/dicom-go/internal/srcli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func main() { wrappercli.Main("dsr2html", srcli.Execute) }
