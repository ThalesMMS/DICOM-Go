// Command ianscp exposes the corresponding reusable library capability.
package main

import (
	"github.com/ThalesMMS/dicom-go/internal/servicecli"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
)

func main() { wrappercli.Main("ianscp", servicecli.Execute) }
