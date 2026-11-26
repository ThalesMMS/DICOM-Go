package roi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Opt-in independent implementation. Equality is asserted only for unchanged
// contours/cavities. Scale/branch comparisons report differences, since the
// recursive ITK median and this physical geodesic schedule are not identical.
func TestContourInterpolationITKOracle(t *testing.T) {
	python := os.Getenv("CONTOUR_ITK_PYTHON")
	if python == "" {
		t.Skip("set CONTOUR_ITK_PYTHON to an environment with itk5.4.7 and morphology2.1.2")
	}
	for _, name := range []string{"identity-cavity", "scale", "branch"} {
		t.Run(name, func(t *testing.T) {
			r := contourFixture()
			r.Geometry.RowSpacing = 1
			r.Manual[0] = contourRectangle(3, 3, 14, 12)
			r.Manual[4] = r.Manual[0].Clone()
			switch name {
			case "identity-cavity":
				for _, m := range r.Manual {
					for y := 6; y < 11; y++ {
						for x := 7; x < 12; x++ {
							m.Set(x, y, false)
						}
					}
				}
			case "scale":
				r.Manual[0] = contourRectangle(7, 6, 5, 5)
			case "branch":
				r.Manual[4] = contourRectangle(3, 3, 5, 12)
				for y := 3; y < 15; y++ {
					r.Manual[4].SetRun(y, 12, 17)
				}
			}
			got, err := InterpolateContours(context.Background(), r)
			if err != nil || got.ValidateComplete() != nil {
				t.Fatal(err, got.Unresolved)
			}
			input := make([]int, 24*20*5)
			expected := make([]int, len(input))
			for z, m := range r.Manual {
				for y := 0; y < 20; y++ {
					for x := 0; x < 24; x++ {
						if m.Get(x, y) {
							input[z*480+y*24+x] = 7
							expected[z*480+y*24+x] = 7
						}
					}
				}
			}
			for z, m := range got.Generated {
				for y := 0; y < 20; y++ {
					for x := 0; x < 24; x++ {
						if m.Get(x, y) {
							expected[z*480+y*24+x] = 7
						}
					}
				}
			}
			payload, _ := json.Marshal(input)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, python, "-c", contourITKOracleScript)
			command.Stdin = bytes.NewReader(payload)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("ITK oracle: %v\n%s", err, output)
			}
			var reference struct {
				Values          []int
				Version, Module string
			}
			if err := json.Unmarshal(output, &reference); err != nil {
				t.Fatalf("ITK output: %v\n%s", err, output)
			}
			if reference.Version != "5.4.7" || reference.Module != "2.1.2" || len(reference.Values) != len(expected) {
				t.Fatalf("unexpected oracle contract %+v", reference)
			}
			differences := 0
			for i, v := range expected {
				if reference.Values[i] != v {
					differences++
				}
			}
			if name == "identity-cavity" && differences != 0 {
				t.Fatalf("equivalent unchanged cavity differs at %d voxels", differences)
			}
			t.Logf("ITK%s/module%s %s: %d differing voxels out of %d (general equivalence not claimed)", reference.Version, reference.Module, name, differences, len(expected))
		})
	}
}

const contourITKOracleScript = `import sys,json,importlib.metadata
import numpy as np,itk
values=np.asarray(json.load(sys.stdin),dtype=np.uint16).reshape((5,20,24))
image=itk.image_from_array(values)
image.SetSpacing((1.0,1.0,1.0))
f=itk.MorphologicalContourInterpolator[type(image)].New(Input=image)
f.SetAxis(2);f.SetLabel(7);f.SetUseCustomSlicePositions(True)
f.SetLabeledSliceIndices(2,7,[0,4]);f.SetUseExtrapolation(False)
f.SetUseDistanceTransform(False);f.SetUseBallStructuringElement(False)
f.Update()
print(json.dumps({'Values':itk.array_from_image(f.GetOutput()).reshape(-1).astype(int).tolist(),'Version':importlib.metadata.version('itk-core'),'Module':importlib.metadata.version('itk-morphologicalcontourinterpolation')}))
`
