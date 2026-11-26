package roi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// skimageScript prints, per label, the voxel count, centroid (mm),
// inertia-tensor eigenvalues, Feret diameter, mean and percentiles that
// scikit-image's regionprops and NumPy compute on the same label volume.
const skimageScript = `
import json, sys
import numpy as np
from skimage.measure import regionprops
p = json.load(open(sys.argv[1]))
shape = (p["slices"], p["rows"], p["columns"])
lab = np.fromfile(p["labels"], dtype=np.uint8).reshape(shape)
img = np.fromfile(p["values"], dtype="<f8").reshape(shape)
spacing = (p["sz"], p["sy"], p["sx"])
out = {}
for r in regionprops(lab, intensity_image=img, spacing=spacing):
    idx = np.argwhere(lab == r.label).astype(float) * np.array(spacing)
    vals = img[lab == r.label]
    out[str(r.label)] = dict(
        count=int(r.num_pixels),
        centroid_zyx=[float(v) for v in idx.mean(axis=0)],
        inertia=[float(v) for v in r.inertia_tensor_eigvals],
        feret=float(r.feret_diameter_max),
        mean=float(r.intensity_mean),
        pct=[float(v) for v in np.percentile(vals, [5, 25, 50, 75, 95])],
    )
print(json.dumps(out))
`

// TestShapeStatisticsMatchSkimage compares with scikit-image's regionprops
// when DICOMGO_SKIMAGE_PYTHON names a Python with scikit-image (for example
// "py -3.13"). Counts, centroids, second moments and percentiles must agree
// to round-off; scikit-image's Feret diameter is measured on a marching-cubes
// surface, so it only has to agree within a voxel diagonal.
func TestShapeStatisticsMatchSkimage(t *testing.T) {
	python := strings.Fields(os.Getenv("DICOMGO_SKIMAGE_PYTHON"))
	if len(python) == 0 {
		t.Skip("set DICOMGO_SKIMAGE_PYTHON to a Python with scikit-image to compare")
	}
	sp := [3]float64{0.7, 0.9, 1.6}
	nx, ny, nz := 48, 40, 24
	rot := rotation(0.6, 0.4, -0.2)
	lv := &LabelVolume{Grid: shapeAxisGrid(nx, ny, nz, sp), Labels: make([]uint16, nx*ny*nz)}
	values := make([]float64, len(lv.Labels))
	labels8 := make([]byte, len(lv.Labels))
	centre := render.Vec3{X: 14, Y: 17, Z: 19}
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				i := (z*ny+y)*nx + x
				p := render.Vec3{X: float64(x) * sp[0], Y: float64(y) * sp[1], Z: float64(z) * sp[2]}
				d := p.Sub(centre)
				u, v, w := d.Dot(rot[0])/11, d.Dot(rot[1])/7, d.Dot(rot[2])/4
				switch {
				case u*u+v*v+w*w <= 1:
					lv.Labels[i] = 1
				case x >= 32 && x < 44 && y >= 5 && y < 30 && z >= 3 && z < 20:
					lv.Labels[i] = 2
				}
				labels8[i] = byte(lv.Labels[i])
				values[i] = math.Sin(float64(i)*0.37)*100 + float64(z)
			}
		}
	}
	dir := t.TempDir()
	write := func(name string, data any) string {
		var buf bytes.Buffer
		if err := binary.Write(&buf, binary.LittleEndian, data); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	params, _ := json.Marshal(map[string]any{
		"columns": nx, "rows": ny, "slices": nz, "sx": sp[0], "sy": sp[1], "sz": sp[2],
		"labels": write("labels.raw", labels8), "values": write("values.raw", values),
	})
	paramsPath := filepath.Join(dir, "params.json")
	scriptPath := filepath.Join(dir, "regionprops.py")
	if err := os.WriteFile(paramsPath, params, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(skimageScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python[0], append(python[1:], scriptPath, paramsPath)...)
	outBytes, err := cmd.Output()
	if err != nil {
		t.Fatalf("scikit-image: %v", err)
	}
	var ref map[string]struct {
		Count       int       `json:"count"`
		CentroidZYX []float64 `json:"centroid_zyx"`
		Inertia     []float64 `json:"inertia"`
		Feret       float64   `json:"feret"`
		Mean        float64   `json:"mean"`
		Pct         []float64 `json:"pct"`
	}
	if err := json.Unmarshal(outBytes, &ref); err != nil {
		t.Fatalf("scikit-image output %q: %v", outBytes, err)
	}

	shapes, err := LabelShapeStatistics(context.Background(), lv, ShapeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	intensities, err := LabelIntensityStatistics(context.Background(), lv, values)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range shapes {
		r, ok := ref[strconv.Itoa(int(s.Label))]
		if !ok {
			t.Fatalf("scikit-image has no label %d", s.Label)
		}
		if s.VoxelCount != r.Count {
			t.Errorf("label %d: count %d, scikit-image %d", s.Label, s.VoxelCount, r.Count)
		}
		expectVecNear(t, "centroid", []float64{r.CentroidZYX[2], r.CentroidZYX[1], r.CentroidZYX[0]}, vec(s.Centroid), 1e-9)
		// Inertia tensor I = tr(C)·1 - C: covariance eigenvalues are
		// tr(I)/2 - eig(I), ascending.
		half := (r.Inertia[0] + r.Inertia[1] + r.Inertia[2]) / 2
		expectVecNear(t, "principal moments", []float64{half - r.Inertia[0], half - r.Inertia[1], half - r.Inertia[2]}, s.PrincipalMoments, 1e-8)
		if diag := math.Sqrt(sp[0]*sp[0] + sp[1]*sp[1] + sp[2]*sp[2]); math.Abs(s.FeretDiameter-r.Feret) > diag {
			t.Errorf("label %d: Feret %g, scikit-image %g", s.Label, s.FeretDiameter, r.Feret)
		}
		t.Logf("label %d: Feret %.3f (centres) vs %.3f (scikit-image marching cubes)", s.Label, s.FeretDiameter, r.Feret)
		in := intensities[i]
		expectNear(t, "mean", r.Mean, in.Mean, 1e-9)
		expectVecNear(t, "percentiles", r.Pct, in.Percentiles(5, 25, 50, 75, 95), 1e-9)
	}
}
