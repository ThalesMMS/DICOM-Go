// Command printscu exposes the corresponding reusable library capability.
package main

import (
	"github.com/ThalesMMS/dicom-go/internal/printcli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func main() { wrappercli.Main("printscu", printcli.Execute) }
