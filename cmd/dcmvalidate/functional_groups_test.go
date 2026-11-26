package main

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"io"
	"strings"
	"testing"
)

func TestFunctionalGroupsCLIExplicitOption(t *testing.T) {
	opts, err := parseArgs([]string{"-functional-groups", "-iod", "Enhanced CT Image", "file.dcm"}, io.Discard)
	if err != nil || !opts.functionalGroups {
		t.Fatalf("%+v %v", opts, err)
	}
	opts, err = parseArgs([]string{"file.dcm"}, io.Discard)
	if err != nil || opts.functionalGroups {
		t.Fatalf("default %+v %v", opts, err)
	}
}

func TestFunctionalGroupsCLIStructureAndPrivacy(t *testing.T) {
	file := ctImage(t)
	file.Dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "1"))
	path := writeFile(t, "functional-groups.dcm", file)
	if code, out, errOut := runTool("-iod-only", path); code != 0 {
		t.Fatalf("legacy changed %d %s %s", code, out, errOut)
	}
	code, out, errOut := runTool("-functional-groups", "-iod-only", "-iod", "Enhanced CT Image", path)
	if code != 1 || !strings.Contains(out, "shared-cardinality") || !strings.Contains(out, "invalid-functional-group") {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
}
