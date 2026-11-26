package calcium

import (
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// Synthetic headers with only the attributes CheckSource reads.

func header(modality string) *object.Object {
	obj := object.New(std.Dictionary)
	_ = obj.SetStrings(core.NewTag(0x0008, 0x0060), core.VRCS, modality)
	return obj
}

func withAgentSequence(obj *object.Object, meaning string) *object.Object {
	agent := object.New(std.Dictionary)
	_ = agent.SetStrings(core.NewTag(0x0008, 0x0100), core.VRSH, "C-B0322")
	_ = agent.SetStrings(core.NewTag(0x0008, 0x0102), core.VRSH, "SRT")
	_ = agent.SetStrings(core.NewTag(0x0008, 0x0104), core.VRLO, meaning)
	obj.SetSequence(core.NewTag(0x0018, 0x0012), agent)
	return obj
}

func withUsage(obj *object.Object, administered string) *object.Object {
	usage := object.New(std.Dictionary)
	_ = usage.SetStrings(core.NewTag(0x0018, 0x9342), core.VRCS, administered)
	group := object.New(std.Dictionary)
	group.SetSequence(core.NewTag(0x0018, 0x9341), usage)
	obj.SetSequence(core.NewTag(0x5200, 0x9230), group)
	return obj
}

func TestCheckSourceRefusesNonCTAndContrast(t *testing.T) {
	agent := header("CT")
	_ = agent.SetStrings(core.NewTag(0x0018, 0x0010), core.VRLO, "IOPAMIDOL")
	for _, tc := range []struct {
		name  string
		obj   *object.Object
		err   error
		agent string
	}{
		{name: "non-contrast CT", obj: header("CT")},
		{name: "MR", obj: header("MR"), err: ErrNotCT},
		{name: "no image", obj: nil, err: ErrNotCT},
		{name: "contrast/bolus agent", obj: agent, err: ErrContrastEnhanced, agent: "IOPAMIDOL"},
		{name: "enhanced CT agent administered", obj: withUsage(withAgentSequence(header("CT"), "Iodinated contrast"), "YES"), err: ErrContrastEnhanced, agent: "Iodinated contrast"},
		{name: "enhanced CT agent not administered", obj: withUsage(withAgentSequence(header("CT"), "Iodinated contrast"), "NO")},
		{name: "enhanced contrast agent without usage", obj: withAgentSequence(header("CT"), "Iodinated contrast"), err: ErrContrastEnhanced, agent: "Iodinated contrast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckSource(tc.obj, 0); !errors.Is(err, tc.err) || (tc.err == nil && err != nil) {
				t.Fatalf("CheckSource err = %v, want %v", err, tc.err)
			}
			if tc.agent != "" {
				if got, ok := ContrastAgent(tc.obj, 0); !ok || got != tc.agent {
					t.Fatalf("ContrastAgent = %q, %v; want %q", got, ok, tc.agent)
				}
			}
		})
	}
}
