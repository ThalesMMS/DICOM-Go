package roi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"
)

// textureFixture is a mask and values on a small grid.
type textureFixture struct {
	nx, ny, nz int
	inside     []bool
	values     []float64
}

func (f textureFixture) mask() *VolumeMask {
	m := NewVolumeMask(f.nx, f.ny, f.nz, [3]float64{1, 1, 1})
	copy(m.Voxels, f.inside)
	return m
}

func (f textureFixture) valueAt(x, y, z int) (float64, bool) {
	return f.values[(z*f.ny+y)*f.nx+x], true
}

func (f textureFixture) features(t testing.TB, opts TextureOptions) TextureFeatures {
	t.Helper()
	out, err := f.mask().TextureFeatures(context.Background(), f.valueAt, opts)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// haralickImage is the 4 × 4 example of Haralick, Shanmugam and Dinstein
// (IEEE SMC 1973), grey levels 0..3; Galloway's run length example (CGIP
// 1975) is gallowayImage.
var (
	haralickImage = []float64{0, 0, 1, 1, 0, 0, 1, 1, 0, 2, 2, 2, 2, 2, 3, 3}
	gallowayImage = []float64{0, 1, 2, 3, 0, 2, 3, 3, 2, 1, 1, 1, 3, 0, 3, 0}
)

func planeFixture(values []float64) textureFixture {
	inside := make([]bool, len(values))
	for i := range inside {
		inside[i] = true
	}
	return textureFixture{nx: 4, ny: 4, nz: 1, inside: inside, values: values}
}

func gridOf(t *testing.T, f textureFixture) *textureGrid {
	t.Helper()
	g := &textureGrid{dims: [3]int{f.nx, f.ny, f.nz}, level: make([]int32, len(f.values))}
	for i, v := range f.values {
		if f.inside[i] {
			g.level[i] = int32(v) + 1
			g.levels = max(g.levels, int(v)+1)
		}
	}
	return g
}

// TestTextureMatricesMatchTheClassicExamples: the horizontal GLCM of
// Haralick's image and the horizontal run lengths of Galloway's are those
// of the papers.
func TestTextureMatricesMatchTheClassicExamples(t *testing.T) {
	opts, _ := TextureOptions{BinWidth: 1}.normalized()
	glcm, _, err := gridOf(t, planeFixture(haralickImage)).directionalMatrices(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{4, 2, 1, 0, 2, 4, 0, 0, 1, 0, 6, 1, 0, 0, 1, 2}
	for i, c := range glcm[0].counts {
		if c != want[i] {
			t.Fatalf("Haralick 0° GLCM %v, want %v", glcm[0].counts, want)
		}
	}
	if glcm[0].pairs != 24 || glcm[2].pairs != 0 {
		t.Fatalf("pairs %v along x, %v along z", glcm[0].pairs, glcm[2].pairs)
	}
	_, glrlm, err := gridOf(t, planeFixture(gallowayImage)).directionalMatrices(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// Rows: grey level 0..3; columns: run length 1..4.
	wantRuns := []float64{4, 0, 0, 0, 1, 0, 1, 0, 3, 0, 0, 0, 3, 1, 0, 0}
	for i, c := range glrlm[0].counts {
		if c != wantRuns[i] {
			t.Fatalf("Galloway 0° GLRLM %v, want %v", glrlm[0].counts, wantRuns)
		}
	}
	// Along z every voxel of one slice is a run of one.
	if glrlm[2].runs != 16 {
		t.Fatalf("%v runs along z, want 16", glrlm[2].runs)
	}

	// Features of those matrices worked by hand.
	g := glcmFeatures(want, 24, 4)
	expectNear(t, "contrast", 14.0/24, g.Contrast, 1e-15)
	expectNear(t, "angular second moment", 84.0/576, g.AngularSecondMoment, 1e-15)
	expectNear(t, "dissimilarity", 10.0/24, g.Dissimilarity, 1e-15)
	expectNear(t, "joint maximum", 6.0/24, g.JointMaximum, 1e-15)
	r := glrlmFeatures(wantRuns, 13, 4, 4, 16)
	expectNear(t, "short runs emphasis", (11+1.0/4+1.0/9)/13, r.ShortRunsEmphasis, 1e-15)
	expectNear(t, "long runs emphasis", 24.0/13, r.LongRunsEmphasis, 1e-15)
	expectNear(t, "grey level non-uniformity", 45.0/13, r.GreyLevelNonUniformity, 1e-15)
	expectNear(t, "run length non-uniformity", 123.0/13, r.RunLengthNonUniformity, 1e-15)
	expectNear(t, "low grey level run emphasis", (4+2.0/4+3.0/9+4.0/16)/13, r.LowGreyLevelRunEmphasis, 1e-15)
	expectNear(t, "run percentage", 13.0/16, r.RunPercentage, 1e-15)
}

// referenceGLCM is a direct transcription of IBSI section 3.6 for one
// direction: ordered voxel pairs counted both ways in a map, marginals and
// means computed for i and j separately.
func referenceGLCM(f textureFixture, levels []int, d [3]int) (GLCMFeatures, bool) {
	ng := 0
	for _, l := range levels {
		ng = max(ng, l)
	}
	counts := map[[2]int]float64{}
	total := 0.0
	index := func(x, y, z int) int { return (z*f.ny+y)*f.nx + x }
	for z := 0; z < f.nz; z++ {
		for y := 0; y < f.ny; y++ {
			for x := 0; x < f.nx; x++ {
				qx, qy, qz := x+d[0], y+d[1], z+d[2]
				if qx < 0 || qy < 0 || qz < 0 || qx >= f.nx || qy >= f.ny || qz >= f.nz {
					continue
				}
				a, b := levels[index(x, y, z)], levels[index(qx, qy, qz)]
				if a == 0 || b == 0 {
					continue
				}
				counts[[2]int{a, b}]++
				counts[[2]int{b, a}]++
				total += 2
			}
		}
	}
	if total == 0 {
		return GLCMFeatures{}, false
	}
	p := func(i, j int) float64 { return counts[[2]int{i, j}] / total }
	pi, pj := make([]float64, ng+1), make([]float64, ng+1)
	for i := 1; i <= ng; i++ {
		for j := 1; j <= ng; j++ {
			pi[i] += p(i, j)
			pj[j] += p(i, j)
		}
	}
	var muI, muJ float64
	for i := 1; i <= ng; i++ {
		muI += float64(i) * pi[i]
		muJ += float64(i) * pj[i]
	}
	var sdI, sdJ float64
	for i := 1; i <= ng; i++ {
		sdI += (float64(i) - muI) * (float64(i) - muI) * pi[i]
		sdJ += (float64(i) - muJ) * (float64(i) - muJ) * pj[i]
	}
	sdI, sdJ = math.Sqrt(sdI), math.Sqrt(sdJ)
	pMinus := make([]float64, ng)
	pPlus := make([]float64, 2*ng+1)
	var g GLCMFeatures
	var hxy, hxy1, hxy2, cross float64
	for i := 1; i <= ng; i++ {
		for j := 1; j <= ng; j++ {
			v := p(i, j)
			fi, fj := float64(i), float64(j)
			pMinus[int(math.Abs(fi-fj))] += v
			pPlus[i+j] += v
			g.JointMaximum = math.Max(g.JointMaximum, v)
			g.JointAverage += fi * v
			if v > 0 {
				hxy -= v * math.Log2(v)
			}
			g.AngularSecondMoment += v * v
			g.Contrast += (fi - fj) * (fi - fj) * v
			g.Dissimilarity += math.Abs(fi-fj) * v
			g.InverseDifference += v / (1 + math.Abs(fi-fj))
			g.NormalisedInverseDifference += v / (1 + math.Abs(fi-fj)/float64(ng))
			g.InverseDifferenceMoment += v / (1 + (fi-fj)*(fi-fj))
			g.NormalisedInverseDifferenceMoment += v / (1 + (fi-fj)*(fi-fj)/float64(ng*ng))
			if i != j {
				g.InverseVariance += v / ((fi - fj) * (fi - fj))
			}
			g.Autocorrelation += fi * fj * v
			cross += fi * fj * v
			s := fi + fj - muI - muJ
			g.ClusterTendency += s * s * v
			g.ClusterShade += s * s * s * v
			g.ClusterProminence += s * s * s * s * v
			if q := pi[i] * pj[j]; q > 0 {
				hxy1 -= v * math.Log2(q)
				hxy2 -= q * math.Log2(q)
			}
		}
	}
	for i := 1; i <= ng; i++ {
		g.JointVariance += (float64(i) - g.JointAverage) * (float64(i) - g.JointAverage) * pi[i]
	}
	g.JointEntropy = hxy
	for k, v := range pMinus {
		g.DifferenceAverage += float64(k) * v
		if v > 0 {
			g.DifferenceEntropy -= v * math.Log2(v)
		}
	}
	for k, v := range pMinus {
		g.DifferenceVariance += (float64(k) - g.DifferenceAverage) * (float64(k) - g.DifferenceAverage) * v
	}
	for k := 2; k <= 2*ng; k++ {
		g.SumAverage += float64(k) * pPlus[k]
		if pPlus[k] > 0 {
			g.SumEntropy -= pPlus[k] * math.Log2(pPlus[k])
		}
	}
	for k := 2; k <= 2*ng; k++ {
		g.SumVariance += (float64(k) - g.SumAverage) * (float64(k) - g.SumAverage) * pPlus[k]
	}
	g.Correlation = (cross - muI*muJ) / (sdI * sdJ)
	var hx, hy float64
	for i := 1; i <= ng; i++ {
		if pi[i] > 0 {
			hx -= pi[i] * math.Log2(pi[i])
		}
		if pj[i] > 0 {
			hy -= pj[i] * math.Log2(pj[i])
		}
	}
	g.InformationCorrelation1 = (hxy - hxy1) / math.Max(hx, hy)
	g.InformationCorrelation2 = math.Sqrt(1 - math.Exp(-2*(hxy2-hxy)))
	return g, true
}

// referenceGLRLM is a transcription of IBSI section 3.7 for one direction
// that finds runs as chains of equal neighbours joined by union-find.
func referenceGLRLM(f textureFixture, levels []int, d [3]int, voxels int) GLRLMFeatures {
	parent := make([]int, len(levels))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	index := func(x, y, z int) int { return (z*f.ny+y)*f.nx + x }
	for z := 0; z < f.nz; z++ {
		for y := 0; y < f.ny; y++ {
			for x := 0; x < f.nx; x++ {
				qx, qy, qz := x+d[0], y+d[1], z+d[2]
				if qx < 0 || qy < 0 || qz < 0 || qx >= f.nx || qy >= f.ny || qz >= f.nz {
					continue
				}
				a, b := index(x, y, z), index(qx, qy, qz)
				if levels[a] != 0 && levels[a] == levels[b] {
					parent[find(a)] = find(b)
				}
			}
		}
	}
	size := map[int]int{}
	level := map[int]int{}
	for i, l := range levels {
		if l != 0 {
			size[find(i)]++
			level[find(i)] = l
		}
	}
	runs := map[[2]int]float64{}
	ns := 0.0
	for root, n := range size {
		runs[[2]int{level[root], n}]++
		ns++
	}
	var r GLRLMFeatures
	byLevel, byLength := map[int]float64{}, map[int]float64{}
	var muI, muJ float64
	for k, c := range runs {
		i, j := float64(k[0]), float64(k[1])
		byLevel[k[0]] += c
		byLength[k[1]] += c
		r.ShortRunLowGreyLevelEmphasis += c / (i * i * j * j) / ns
		r.ShortRunHighGreyLevelEmphasis += c * i * i / (j * j) / ns
		r.LongRunLowGreyLevelEmphasis += c * j * j / (i * i) / ns
		r.LongRunHighGreyLevelEmphasis += c * i * i * j * j / ns
		muI += i * c / ns
		muJ += j * c / ns
		r.RunEntropy -= c / ns * math.Log2(c/ns)
	}
	for k, c := range runs {
		i, j := float64(k[0]), float64(k[1])
		r.GreyLevelVariance += (i - muI) * (i - muI) * c / ns
		r.RunLengthVariance += (j - muJ) * (j - muJ) * c / ns
	}
	for i, c := range byLevel {
		r.LowGreyLevelRunEmphasis += c / float64(i*i) / ns
		r.HighGreyLevelRunEmphasis += c * float64(i*i) / ns
		r.GreyLevelNonUniformity += c * c / ns
		r.NormalisedGreyLevelNonUniformity += c * c / (ns * ns)
	}
	for j, c := range byLength {
		r.ShortRunsEmphasis += c / float64(j*j) / ns
		r.LongRunsEmphasis += c * float64(j*j) / ns
		r.RunLengthNonUniformity += c * c / ns
		r.NormalisedRunLengthNonUniformity += c * c / (ns * ns)
	}
	r.RunPercentage = ns / float64(voxels)
	return r
}

// randomTextureFixture is an irregular region with clustered values.
func randomTextureFixture(seed int64, nx, ny, nz int) textureFixture {
	random := rand.New(rand.NewSource(seed))
	f := textureFixture{nx: nx, ny: ny, nz: nz, inside: make([]bool, nx*ny*nz), values: make([]float64, nx*ny*nz)}
	for i := range f.values {
		x, y, z := i%nx, (i/nx)%ny, i/(nx*ny)
		dx, dy, dz := float64(x)-float64(nx-1)/2, float64(y)-float64(ny-1)/2, float64(z)-float64(nz-1)/2
		f.inside[i] = dx*dx/float64(nx*nx)+dy*dy/float64(ny*ny)+dz*dz/float64(nz*nz) < 0.3 && random.Float64() < 0.9
		f.values[i] = 100 + 30*math.Sin(float64(x)/2) + 20*float64(z) + 25*random.NormFloat64()
	}
	f.values[3] = math.NaN()
	return f
}

func discretise(f textureFixture, width, lower float64) []int {
	levels := make([]int, len(f.values))
	for i, v := range f.values {
		if f.inside[i] && !math.IsNaN(v) && v >= lower {
			levels[i] = int(math.Floor((v-lower)/width)) + 1
		}
	}
	return levels
}

func expectFieldsNear(t *testing.T, name string, want, got []*float64, tolerance float64) {
	t.Helper()
	for k := range want {
		w, g := *want[k], *got[k]
		if math.IsNaN(w) && math.IsNaN(g) {
			continue
		}
		if !(math.Abs(w-g) <= tolerance*math.Max(1, math.Abs(w))) {
			t.Fatalf("%s field %d: %v, reference %v", name, k, g, w)
		}
	}
}

// TestTextureFeaturesMatchTheIBSIReference: on irregular 3D fixtures the
// averaged and merged features equal a direct transcription of the IBSI
// formulas.
func TestTextureFeaturesMatchTheIBSIReference(t *testing.T) {
	for _, c := range []struct {
		seed       int64
		nx, ny, nz int
		width      float64
	}{{1, 7, 6, 5, 20}, {2, 9, 8, 4, 35}, {3, 6, 6, 1, 15}} {
		f := randomTextureFixture(c.seed, c.nx, c.ny, c.nz)
		got := f.features(t, TextureOptions{BinWidth: c.width})
		lower := math.Inf(1)
		voxels := 0
		for i, v := range f.values {
			if f.inside[i] && !math.IsNaN(v) {
				lower = math.Min(lower, v)
				voxels++
			}
		}
		levels := discretise(f, c.width, lower)
		if got.Voxels != voxels || got.LowerBound != lower || got.NonFinite > 1 {
			t.Fatalf("seed %d: %d voxels from %v, want %d from %v", c.seed, got.Voxels, got.LowerBound, voxels, lower)
		}
		var glcm []GLCMFeatures
		var glrlm []GLRLMFeatures
		for _, d := range textureDirections {
			if g, ok := referenceGLCM(f, levels, d); ok {
				glcm = append(glcm, g)
			}
			glrlm = append(glrlm, referenceGLRLM(f, levels, d, voxels))
		}
		wantGLCM, wantGLRLM := averageFeatures(glcm), averageFeatures(glrlm)
		if got.GLCMDirections != len(glcm) || got.GLRLMDirections != 13 {
			t.Fatalf("seed %d: %d GLCM and %d GLRLM directions, want %d and 13", c.seed, got.GLCMDirections, got.GLRLMDirections, len(glcm))
		}
		expectFieldsNear(t, "GLCM", glcmFields(&wantGLCM), glcmFields(&got.GLCM), 1e-10)
		expectFieldsNear(t, "GLRLM", glrlmFields(&wantGLRLM), glrlmFields(&got.GLRLM), 1e-10)
		if c.nz == 1 && got.GLCMDirections != 4 {
			t.Fatalf("one slice: %d GLCM directions, want 4", got.GLCMDirections)
		}
	}
}

// TestTextureMergedAggregation: merged features are those of the summed
// directional matrices, and the merged run percentage, which divides by the
// voxels of all 13 directions, is the mean of the directional ones.
func TestTextureMergedAggregation(t *testing.T) {
	f := randomTextureFixture(5, 8, 8, 6)
	averaged := f.features(t, TextureOptions{BinWidth: 25})
	merged := f.features(t, TextureOptions{BinWidth: 25, Aggregation: TextureMerged})
	if merged.GLCMDirections != averaged.GLCMDirections || merged.GLRLMDirections != 13 {
		t.Fatalf("directions %d merged, %d averaged", merged.GLCMDirections, averaged.GLCMDirections)
	}
	expectNear(t, "merged run percentage", averaged.GLRLM.RunPercentage, merged.GLRLM.RunPercentage, 1e-12)
	if merged.GLCM.Contrast == averaged.GLCM.Contrast {
		t.Fatal("merged and averaged contrast coincide on an anisotropic texture")
	}

	opts, _ := TextureOptions{BinWidth: 1}.normalized()
	glcm, _, err := gridOf(t, planeFixture(haralickImage)).directionalMatrices(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	sum := make([]float64, 16)
	var total float64
	for _, m := range glcm {
		for i, c := range m.counts {
			sum[i] += c
		}
		total += m.pairs
	}
	want := glcmFeatures(sum, total, 4)
	got, n := aggregateGLCM(glcm, 4, TextureMerged)
	if n != 4 || !sameFeatures(glcmFields(&got), glcmFields(&want)) {
		t.Fatalf("merged Haralick GLCM over %d directions: %+v, want %+v", n, got, want)
	}
}

// TestTextureFeaturesAreRotationInvariant: transposing and mirroring the
// grid permutes the 13 directions, so the averaged features do not change.
func TestTextureFeaturesAreRotationInvariant(t *testing.T) {
	f := randomTextureFixture(11, 7, 9, 5)
	want := f.features(t, TextureOptions{BinWidth: 20})
	turned := textureFixture{nx: f.ny, ny: f.nx, nz: f.nz, inside: make([]bool, len(f.inside)), values: make([]float64, len(f.values))}
	for i := range f.values {
		x, y, z := i%f.nx, (i/f.nx)%f.ny, i/(f.nx*f.ny)
		j := (z*turned.ny+(f.nx-1-x))*turned.nx + y // transpose x↔y, mirror the new y
		turned.inside[j], turned.values[j] = f.inside[i], f.values[i]
	}
	got := turned.features(t, TextureOptions{BinWidth: 20})
	expectFieldsNear(t, "GLCM", glcmFields(&want.GLCM), glcmFields(&got.GLCM), 1e-12)
	expectFieldsNear(t, "GLRLM", glrlmFields(&want.GLRLM), glrlmFields(&got.GLRLM), 1e-12)
}

func TestTextureOfAConstantRegion(t *testing.T) {
	f := planeFixture(make([]float64, 16))
	got := f.features(t, TextureOptions{BinWidth: 10})
	if got.GreyLevels != 1 || got.GLCM.AngularSecondMoment != 1 || got.GLCM.Contrast != 0 || got.GLCM.JointEntropy != 0 ||
		!math.IsNaN(got.GLCM.Correlation) || !math.IsNaN(got.GLCM.InformationCorrelation1) || got.GLCM.InformationCorrelation2 != 0 {
		t.Fatalf("constant GLCM %+v", got.GLCM)
	}
	// In plane, each row is one run of 4 (x), each column one of 4 (y), the
	// diagonals runs of 1..4, and z runs of 1.
	if got.GLRLM.GreyLevelVariance != 0 || got.GLRLM.LowGreyLevelRunEmphasis != 1 {
		t.Fatalf("constant GLRLM %+v", got.GLRLM)
	}
	if got.GLRLM.RunPercentage <= 0 || got.GLRLM.RunPercentage > 1 {
		t.Fatalf("run percentage %v", got.GLRLM.RunPercentage)
	}
}

func TestTextureDiscretisationAndLowerBound(t *testing.T) {
	f := planeFixture([]float64{-10, 0, 5, 9.999, 10, 19, 20, 35, 0, 0, 0, 0, 0, 0, 0, 0})
	lower := 0.0
	got := f.features(t, TextureOptions{BinWidth: 10, LowerBound: &lower})
	// -10 is below the bound; 0..9.999 → 1, 10..19 → 2, 20 → 3, 35 → 4.
	if got.Excluded != 1 || got.Voxels != 15 || got.GreyLevels != 4 || got.LowerBound != 0 || got.BinWidth != 10 {
		t.Fatalf("discretisation %+v", got)
	}
	free := f.features(t, TextureOptions{BinWidth: 10})
	if free.Excluded != 0 || free.LowerBound != -10 || free.GreyLevels != 5 {
		t.Fatalf("without a bound: %+v", free)
	}
}

func TestTextureEntryPointsAndWorkersAgree(t *testing.T) {
	f := randomTextureFixture(21, 8, 7, 6)
	want := f.features(t, TextureOptions{BinWidth: 25, Workers: 1})
	for _, workers := range []int{0, 3, 13} {
		got := f.features(t, TextureOptions{BinWidth: 25, Workers: workers})
		if !sameFeatures(glcmFields(&got.GLCM), glcmFields(&want.GLCM)) || got.GLRLM != want.GLRLM {
			t.Fatalf("workers %d: features differ", workers)
		}
	}
	voi := &VOIMask{}
	for i, on := range f.inside {
		if on {
			voi.Voxels = append(voi.Voxels, VOIVoxel{Column: i % f.nx, Row: (i / f.nx) % f.ny, Slice: i / (f.nx * f.ny), Weight: 1, CentreInside: true})
		}
	}
	voi.Voxels = append(voi.Voxels, VOIVoxel{Column: 0, Row: 0, Slice: 0, Weight: 0.2})
	got, err := voi.TextureFeatures(context.Background(), f.valueAt, TextureOptions{BinWidth: 25})
	if err != nil {
		t.Fatal(err)
	}
	if !sameFeatures(glcmFields(&got.GLCM), glcmFields(&want.GLCM)) || got.GLRLM != want.GLRLM || got.Voxels != want.Voxels {
		t.Fatal("VOI texture differs from the mask texture of the same voxels")
	}
	if list := want.List(); len(list) != 41 || list[0].Name != "Joint maximum" || list[25].Family != "GLRLM" || list[40].Name != "Run entropy" {
		t.Fatalf("feature list %+v", list)
	}
}

// sameFeatures compares fields bit for bit, NaN equal to NaN.
func sameFeatures(a, b []*float64) bool {
	for k := range a {
		if math.Float64bits(*a[k]) != math.Float64bits(*b[k]) {
			return false
		}
	}
	return true
}

func TestTextureRejectsAndCancels(t *testing.T) {
	f := randomTextureFixture(3, 6, 6, 3)
	m := f.mask()
	nan := math.NaN()
	for name, opts := range map[string]TextureOptions{
		"bin width":    {},
		"lower bound":  {BinWidth: 1, LowerBound: &nan},
		"distance":     {BinWidth: 1, Distance: -1},
		"aggregation":  {BinWidth: 1, Aggregation: 7},
		"workers":      {BinWidth: 1, Workers: -2},
		"grey levels":  {BinWidth: 0.001, MaxGreyLevels: 64},
		"empty region": {BinWidth: 1, LowerBound: func() *float64 { v := 1e9; return &v }()},
	} {
		if _, err := m.TextureFeatures(context.Background(), f.valueAt, opts); !errors.Is(err, ErrInvalidTexture) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := m.TextureFeatures(context.Background(), nil, TextureOptions{BinWidth: 1}); !errors.Is(err, ErrInvalidTexture) {
		t.Errorf("nil accessor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.TextureFeatures(ctx, f.valueAt, TextureOptions{BinWidth: 10}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

// BenchmarkTextureSphere computes GLCM and GLRLM features of a 40-voxel
// radius noisy sphere (about 268k voxels) at 25 HU bins.
func BenchmarkTextureSphere(b *testing.B) {
	const n = 96
	random := rand.New(rand.NewSource(1187))
	f := textureFixture{nx: n, ny: n, nz: n, inside: make([]bool, n*n*n), values: make([]float64, n*n*n)}
	for i := range f.values {
		x, y, z := i%n-n/2, (i/n)%n-n/2, i/(n*n)-n/2
		f.inside[i] = x*x+y*y+z*z < 40*40
		f.values[i] = 40 + 15*math.Sin(float64(x)/3) + 30*random.NormFloat64()
	}
	m := f.mask()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.TextureFeatures(context.Background(), f.valueAt, TextureOptions{BinWidth: 25}); err != nil {
			b.Fatal(err)
		}
	}
}
