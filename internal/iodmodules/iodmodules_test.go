package iodmodules

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestLegacyConvertedIODsResolve(t *testing.T) {
	for _, iod := range []string{"Legacy Converted Enhanced CT Image", "Legacy Converted Enhanced MR Image", "Legacy Converted Enhanced PET Image", "CT Image", "MR Image", "Positron Emission Tomography Image", "Enhanced PET Image"} {
		modules, ok := Modules(iod)
		if !ok || len(modules) < 10 {
			t.Fatalf("%s: %d modules", iod, len(modules))
		}
		for _, use := range modules {
			if _, ok := Attributes(use.Module); !ok {
				t.Errorf("%s: module %q has no attributes", iod, use.Module)
			}
		}
	}
	tags, _ := TopLevelTags("Legacy Converted Enhanced CT Image")
	for _, tag := range []core.Tag{core.NewTag(0x0010, 0x0010), core.NewTag(0x0020, 0x000e), core.NewTag(0x5200, 0x9229), core.NewTag(0x0028, 0x0010), core.NewTag(0x0008, 0x0008), core.NewTag(0x0040, 0x0555)} {
		if _, ok := tags[tag]; !ok {
			t.Errorf("%s not top-level in Legacy Converted Enhanced CT", tag)
		}
	}
	if _, ok := tags[core.NewTag(0x0020, 0x0032)]; ok {
		t.Error("Image Position (Patient) listed at the top level of an Enhanced IOD")
	}
}
