package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// Grey level co-occurrence (GLCM) and run length (GLRLM) texture features of
// segments and VOIs (#1187), research use only.
//
// The definitions are those of the Image Biomarker Standardisation
// Initiative (IBSI 1 reference manual, Zwanenburg et al., Radiology 2020,
// sections 3.6 and 3.7), re-implemented from the published formulas; ITK's
// co-occurrence and run length filters and pyradiomics informed behaviour
// only. Intensities are discretised with a fixed bin width,
// X_d = ⌊(X − X_min)/w⌋ + 1, X_min being the lower bound of the
// re-segmentation range or, without one, the lowest value in the region.
// Matrices are built on the voxel grid over the 13 unique directions of the
// 26-neighbourhood (distance 1 by default) and features are, as configured,
// computed per direction and averaged (IBSI "3D:avg", the default) or
// computed once from the merged matrices ("3D:mrg"); averaged, a feature
// undefined in some directions (Correlation where a direction sees a single
// grey level) is averaged over the others. Logarithms are base 2.
// The voxel grid is used as it is: resample anisotropic images to isotropic
// voxels first when that matters, as IBSI recommends.

// TextureAggregation selects how the 13 directional matrices combine.
type TextureAggregation int

const (
	// TextureAveraged computes the features of each direction and averages
	// them over the directions with data (IBSI 3D:avg).
	TextureAveraged TextureAggregation = iota
	// TextureMerged sums the directional matrices and computes the features
	// once (IBSI 3D:mrg).
	TextureMerged
)

// DefaultMaxTextureGreyLevels bounds the discretised grey levels (the GLCM
// is levels² per direction).
const DefaultMaxTextureGreyLevels = 1024

// ErrInvalidTexture reports unusable texture options or input.
var ErrInvalidTexture = errors.New("roi: invalid texture input")

// TextureOptions configure texture feature extraction.
type TextureOptions struct {
	// BinWidth is the fixed discretisation bin width in value units (for
	// example 25 HU); it must be positive.
	BinWidth float64
	// LowerBound, when set, is X_min of the discretisation, the lower bound
	// of IBSI's re-segmentation range: values below it are left out (counted
	// in TextureFeatures.Excluded). Nil uses the lowest value in the region.
	LowerBound *float64
	// Distance is the GLCM neighbour distance in voxels along each
	// direction (default 1).
	Distance int
	// Aggregation combines the directions (default TextureAveraged).
	Aggregation TextureAggregation
	// MaxGreyLevels bounds the discretised levels (default
	// DefaultMaxTextureGreyLevels); more is an error asking for a wider bin.
	MaxGreyLevels int
	// Workers bounds the goroutines over directions (0: the module's shared
	// concurrency budget). The result does not depend on it.
	Workers int
}

// GLCMFeatures are IBSI's grey level co-occurrence features (section 3.6).
// Correlation and InformationCorrelation1 are NaN for a single grey level,
// where they are undefined.
type GLCMFeatures struct {
	JointMaximum                      float64
	JointAverage                      float64
	JointVariance                     float64
	JointEntropy                      float64
	DifferenceAverage                 float64
	DifferenceVariance                float64
	DifferenceEntropy                 float64
	SumAverage                        float64
	SumVariance                       float64
	SumEntropy                        float64
	AngularSecondMoment               float64
	Contrast                          float64
	Dissimilarity                     float64
	InverseDifference                 float64
	NormalisedInverseDifference       float64
	InverseDifferenceMoment           float64
	NormalisedInverseDifferenceMoment float64
	InverseVariance                   float64
	Correlation                       float64
	Autocorrelation                   float64
	ClusterTendency                   float64
	ClusterShade                      float64
	ClusterProminence                 float64
	InformationCorrelation1           float64
	InformationCorrelation2           float64
}

// GLRLMFeatures are IBSI's grey level run length features (section 3.7).
type GLRLMFeatures struct {
	ShortRunsEmphasis                float64
	LongRunsEmphasis                 float64
	LowGreyLevelRunEmphasis          float64
	HighGreyLevelRunEmphasis         float64
	ShortRunLowGreyLevelEmphasis     float64
	ShortRunHighGreyLevelEmphasis    float64
	LongRunLowGreyLevelEmphasis      float64
	LongRunHighGreyLevelEmphasis     float64
	GreyLevelNonUniformity           float64
	NormalisedGreyLevelNonUniformity float64
	RunLengthNonUniformity           float64
	NormalisedRunLengthNonUniformity float64
	RunPercentage                    float64
	GreyLevelVariance                float64
	RunLengthVariance                float64
	RunEntropy                       float64
}

// TextureFeatures are the texture features of one region.
type TextureFeatures struct {
	// Voxels is the number of voxels discretised (finite values at or above
	// the lower bound); Excluded counts those below the lower bound and
	// NonFinite those without a finite value.
	Voxels, Excluded, NonFinite int
	// GreyLevels is the number of discretised levels N_g (the highest
	// level); BinWidth and LowerBound are the discretisation used.
	GreyLevels int
	BinWidth   float64
	LowerBound float64
	// GLCMDirections and GLRLMDirections are the directions with at least
	// one voxel pair or run (all 13 for a solid 3D region, 4 in one slice).
	GLCMDirections, GLRLMDirections int
	GLCM                            GLCMFeatures
	GLRLM                           GLRLMFeatures
}

// textureDirections are the 13 unique directions of the 26-neighbourhood.
var textureDirections = [13][3]int{
	{1, 0, 0}, {0, 1, 0}, {0, 0, 1},
	{1, 1, 0}, {1, -1, 0}, {1, 0, 1}, {1, 0, -1}, {0, 1, 1}, {0, 1, -1},
	{1, 1, 1}, {1, 1, -1}, {1, -1, 1}, {1, -1, -1},
}

// textureGrid is the discretised region cropped to its bounding box: level
// 0 is outside, 1..levels inside.
type textureGrid struct {
	dims   [3]int
	level  []int32
	levels int
}

func (g *textureGrid) at(x, y, z int) int32 {
	if x < 0 || y < 0 || z < 0 || x >= g.dims[0] || y >= g.dims[1] || z >= g.dims[2] {
		return 0
	}
	return g.level[(z*g.dims[1]+y)*g.dims[0]+x]
}

type textureVoxel struct {
	x, y, z int
	value   float64
}

// textureSource visits the region's voxels with their values (ok false
// when a voxel has no value).
type textureSource func(visit func(x, y, z int, value float64, ok bool)) error

func (o TextureOptions) normalized() (TextureOptions, error) {
	if o.Distance == 0 {
		o.Distance = 1
	}
	if o.MaxGreyLevels == 0 {
		o.MaxGreyLevels = DefaultMaxTextureGreyLevels
	}
	switch {
	case !(o.BinWidth > 0) || math.IsInf(o.BinWidth, 0):
		return o, fmt.Errorf("%w: bin width %v", ErrInvalidTexture, o.BinWidth)
	case o.LowerBound != nil && !finite(*o.LowerBound):
		return o, fmt.Errorf("%w: lower bound %v", ErrInvalidTexture, *o.LowerBound)
	case o.Distance < 1:
		return o, fmt.Errorf("%w: distance %d", ErrInvalidTexture, o.Distance)
	case o.Aggregation != TextureAveraged && o.Aggregation != TextureMerged:
		return o, fmt.Errorf("%w: aggregation %d", ErrInvalidTexture, o.Aggregation)
	case o.MaxGreyLevels < 1:
		return o, fmt.Errorf("%w: %d grey levels", ErrInvalidTexture, o.MaxGreyLevels)
	case o.Workers < 0:
		return o, fmt.Errorf("%w: %d workers", ErrInvalidTexture, o.Workers)
	}
	return o, nil
}

// computeTexture discretises the region source visits and computes its
// features.
func computeTexture(ctx context.Context, source textureSource, opts TextureOptions) (TextureFeatures, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := opts.normalized()
	if err != nil {
		return TextureFeatures{}, err
	}
	var out TextureFeatures
	var voxels []textureVoxel
	lo, hi := [3]int{math.MaxInt, math.MaxInt, math.MaxInt}, [3]int{-1, -1, -1}
	minimum := math.Inf(1)
	err = source(func(x, y, z int, value float64, ok bool) {
		if !ok || !finite(value) {
			out.NonFinite++
			return
		}
		if opts.LowerBound != nil && value < *opts.LowerBound {
			out.Excluded++
			return
		}
		voxels = append(voxels, textureVoxel{x, y, z, value})
		minimum = math.Min(minimum, value)
		for axis, c := range [3]int{x, y, z} {
			lo[axis], hi[axis] = min(lo[axis], c), max(hi[axis], c)
		}
	})
	if err != nil {
		return TextureFeatures{}, err
	}
	if err := ctx.Err(); err != nil {
		return TextureFeatures{}, err
	}
	out.Voxels, out.BinWidth = len(voxels), opts.BinWidth
	if len(voxels) == 0 {
		return out, fmt.Errorf("%w: no voxel with a value", ErrInvalidTexture)
	}
	out.LowerBound = minimum
	if opts.LowerBound != nil {
		out.LowerBound = *opts.LowerBound
	}
	g := &textureGrid{dims: [3]int{hi[0] - lo[0] + 1, hi[1] - lo[1] + 1, hi[2] - lo[2] + 1}}
	g.level = make([]int32, g.dims[0]*g.dims[1]*g.dims[2])
	for _, v := range voxels {
		level := math.Floor((v.value-out.LowerBound)/opts.BinWidth) + 1
		if level > float64(opts.MaxGreyLevels) {
			return out, fmt.Errorf("%w: more than %d grey levels with bin width %v; use a wider bin", ErrInvalidTexture, opts.MaxGreyLevels, opts.BinWidth)
		}
		g.level[((v.z-lo[2])*g.dims[1]+v.y-lo[1])*g.dims[0]+v.x-lo[0]] = int32(level)
		g.levels = max(g.levels, int(level))
	}
	out.GreyLevels = g.levels
	glcm, glrlm, err := g.directionalMatrices(ctx, opts)
	if err != nil {
		return out, err
	}
	out.GLCM, out.GLCMDirections = aggregateGLCM(glcm, g.levels, opts.Aggregation)
	out.GLRLM, out.GLRLMDirections = aggregateGLRLM(glrlm, g.levels, len(voxels), opts.Aggregation)
	return out, nil
}

// glcmMatrix is a symmetric co-occurrence count matrix, levels × levels,
// (i−1)·levels + (j−1) for levels i and j.
type glcmMatrix struct {
	counts []float64
	pairs  float64
}

// glrlmMatrix counts runs, levels × maxRun, (i−1)·maxRun + (j−1) for level
// i and length j.
type glrlmMatrix struct {
	counts []float64
	maxRun int
	runs   float64
}

// directionalMatrices builds the GLCM and GLRLM of every direction, the
// directions spread over the shared concurrency budget.
func (g *textureGrid) directionalMatrices(ctx context.Context, opts TextureOptions) ([13]glcmMatrix, [13]glrlmMatrix, error) {
	var glcm [13]glcmMatrix
	var glrlm [13]glrlmMatrix
	ng := g.levels
	maxRun := max(g.dims[0], g.dims[1], g.dims[2])
	err := parallel.Do(ctx, len(textureDirections), opts.Workers, func(d int) {
		dir := textureDirections[d]
		dx, dy, dz := dir[0]*opts.Distance, dir[1]*opts.Distance, dir[2]*opts.Distance
		co := glcmMatrix{counts: make([]float64, ng*ng)}
		runs := glrlmMatrix{counts: make([]float64, ng*maxRun), maxRun: maxRun}
		for z := 0; z < g.dims[2]; z++ {
			if ctx.Err() != nil {
				return
			}
			for y := 0; y < g.dims[1]; y++ {
				row := (z*g.dims[1] + y) * g.dims[0]
				for x := 0; x < g.dims[0]; x++ {
					a := g.level[row+x]
					if a == 0 {
						continue
					}
					if b := g.at(x+dx, y+dy, z+dz); b != 0 {
						co.counts[int(a-1)*ng+int(b-1)]++
						co.counts[int(b-1)*ng+int(a-1)]++
						co.pairs += 2
					}
					// A run starts where the previous voxel along the
					// direction is outside or of another level.
					if g.at(x-dir[0], y-dir[1], z-dir[2]) == a {
						continue
					}
					length := 1
					for g.at(x+length*dir[0], y+length*dir[1], z+length*dir[2]) == a {
						length++
					}
					runs.counts[int(a-1)*maxRun+length-1]++
					runs.runs++
				}
			}
		}
		glcm[d], glrlm[d] = co, runs
	})
	return glcm, glrlm, err
}

func log2Term(p float64) float64 {
	if p <= 0 {
		return 0
	}
	return p * math.Log2(p)
}

// glcmFeatures computes the features of a count matrix of ng levels with a
// positive total.
func glcmFeatures(counts []float64, total float64, ng int) GLCMFeatures {
	var f GLCMFeatures
	n := float64(ng)
	px := make([]float64, ng)
	diff := make([]float64, ng)    // p_{x−y}(k), k = 0..ng−1
	sum := make([]float64, 2*ng+1) // p_{x+y}(k), k = 2..2ng
	for i := 1; i <= ng; i++ {
		for j := 1; j <= ng; j++ {
			c := counts[(i-1)*ng+j-1]
			if c == 0 {
				continue
			}
			p := c / total
			px[i-1] += p
			k := i - j
			if k < 0 {
				k = -k
			}
			diff[k] += p
			sum[i+j] += p
			fi, fj, fk := float64(i), float64(j), float64(k)
			f.JointMaximum = math.Max(f.JointMaximum, p)
			f.JointAverage += fi * p
			f.JointEntropy -= log2Term(p)
			f.AngularSecondMoment += p * p
			f.Contrast += fk * fk * p
			f.Dissimilarity += fk * p
			f.InverseDifference += p / (1 + fk)
			f.NormalisedInverseDifference += p / (1 + fk/n)
			f.InverseDifferenceMoment += p / (1 + fk*fk)
			f.NormalisedInverseDifferenceMoment += p / (1 + fk*fk/(n*n))
			f.Autocorrelation += fi * fj * p
		}
	}
	mu := f.JointAverage
	var hx, hxy1, hxy2 float64
	for i := 1; i <= ng; i++ {
		pi := px[i-1]
		f.JointVariance += (float64(i) - mu) * (float64(i) - mu) * pi
		hx -= log2Term(pi)
	}
	for i := 1; i <= ng; i++ {
		for j := 1; j <= ng; j++ {
			pipj := px[i-1] * px[j-1]
			if pipj == 0 {
				continue
			}
			p := counts[(i-1)*ng+j-1] / total
			hxy1 -= p * math.Log2(pipj)
			hxy2 -= log2Term(pipj)
			s := float64(i+j) - 2*mu
			f.ClusterTendency += s * s * p
			f.ClusterShade += s * s * s * p
			f.ClusterProminence += s * s * s * s * p
		}
	}
	for k, p := range diff {
		f.DifferenceAverage += float64(k) * p
		f.DifferenceEntropy -= log2Term(p)
		if k > 0 {
			f.InverseVariance += p / float64(k*k)
		}
	}
	for k, p := range diff {
		d := float64(k) - f.DifferenceAverage
		f.DifferenceVariance += d * d * p
	}
	for k := 2; k <= 2*ng; k++ {
		f.SumAverage += float64(k) * sum[k]
		f.SumEntropy -= log2Term(sum[k])
	}
	for k := 2; k <= 2*ng; k++ {
		d := float64(k) - f.SumAverage
		f.SumVariance += d * d * sum[k]
	}
	// Symmetric matrix: μ_i = μ_j = μ and σ_i = σ_j.
	if f.JointVariance > 0 {
		f.Correlation = (f.Autocorrelation - mu*mu) / f.JointVariance
	} else {
		f.Correlation = math.NaN()
	}
	if hx > 0 {
		f.InformationCorrelation1 = (f.JointEntropy - hxy1) / hx
	} else {
		f.InformationCorrelation1 = math.NaN()
	}
	f.InformationCorrelation2 = math.Sqrt(1 - math.Exp(-2*math.Max(0, hxy2-f.JointEntropy)))
	return f
}

// glrlmFeatures computes the features of a run count matrix of ng levels
// and maxRun lengths over voxels voxels.
func glrlmFeatures(counts []float64, runs float64, ng, maxRun int, voxels float64) GLRLMFeatures {
	var f GLRLMFeatures
	byLevel := make([]float64, ng)
	byLength := make([]float64, maxRun)
	var muLevel, muLength float64
	for i := 1; i <= ng; i++ {
		for j := 1; j <= maxRun; j++ {
			r := counts[(i-1)*maxRun+j-1]
			if r == 0 {
				continue
			}
			fi, fj := float64(i), float64(j)
			byLevel[i-1] += r
			byLength[j-1] += r
			f.ShortRunLowGreyLevelEmphasis += r / (fi * fi * fj * fj)
			f.ShortRunHighGreyLevelEmphasis += r * fi * fi / (fj * fj)
			f.LongRunLowGreyLevelEmphasis += r * fj * fj / (fi * fi)
			f.LongRunHighGreyLevelEmphasis += r * fi * fi * fj * fj
			p := r / runs
			muLevel += fi * p
			muLength += fj * p
			f.RunEntropy -= log2Term(p)
		}
	}
	for i := 1; i <= ng; i++ {
		r, fi := byLevel[i-1], float64(i)
		f.LowGreyLevelRunEmphasis += r / (fi * fi)
		f.HighGreyLevelRunEmphasis += r * fi * fi
		f.GreyLevelNonUniformity += r * r
	}
	for j := 1; j <= maxRun; j++ {
		r, fj := byLength[j-1], float64(j)
		f.ShortRunsEmphasis += r / (fj * fj)
		f.LongRunsEmphasis += r * fj * fj
		f.RunLengthNonUniformity += r * r
	}
	for i := 1; i <= ng; i++ {
		for j := 1; j <= maxRun; j++ {
			if r := counts[(i-1)*maxRun+j-1]; r != 0 {
				p := r / runs
				f.GreyLevelVariance += (float64(i) - muLevel) * (float64(i) - muLevel) * p
				f.RunLengthVariance += (float64(j) - muLength) * (float64(j) - muLength) * p
			}
		}
	}
	for _, v := range []*float64{&f.ShortRunsEmphasis, &f.LongRunsEmphasis, &f.LowGreyLevelRunEmphasis, &f.HighGreyLevelRunEmphasis,
		&f.ShortRunLowGreyLevelEmphasis, &f.ShortRunHighGreyLevelEmphasis, &f.LongRunLowGreyLevelEmphasis, &f.LongRunHighGreyLevelEmphasis,
		&f.GreyLevelNonUniformity, &f.RunLengthNonUniformity} {
		*v /= runs
	}
	f.NormalisedGreyLevelNonUniformity = f.GreyLevelNonUniformity / runs
	f.NormalisedRunLengthNonUniformity = f.RunLengthNonUniformity / runs
	f.RunPercentage = runs / voxels
	return f
}

// aggregateGLCM combines the directional matrices; directions without a
// pair are left out.
func aggregateGLCM(matrices [13]glcmMatrix, ng int, aggregation TextureAggregation) (GLCMFeatures, int) {
	directions := 0
	if aggregation == TextureMerged {
		merged := make([]float64, ng*ng)
		var total float64
		for _, m := range matrices {
			if m.pairs > 0 {
				directions++
				total += m.pairs
				for i, c := range m.counts {
					merged[i] += c
				}
			}
		}
		if directions == 0 {
			return nanGLCM(), 0
		}
		return glcmFeatures(merged, total, ng), directions
	}
	var features []GLCMFeatures
	for _, m := range matrices {
		if m.pairs > 0 {
			features = append(features, glcmFeatures(m.counts, m.pairs, ng))
		}
	}
	if len(features) == 0 {
		return nanGLCM(), 0
	}
	return averageFeatures(features), len(features)
}

// aggregateGLRLM combines the directional run matrices. Merged, the run
// percentage divides by the voxels of every direction (IBSI 3D:mrg).
func aggregateGLRLM(matrices [13]glrlmMatrix, ng, voxels int, aggregation TextureAggregation) (GLRLMFeatures, int) {
	directions := 0
	if aggregation == TextureMerged {
		maxRun := matrices[0].maxRun
		merged := make([]float64, ng*maxRun)
		var runs float64
		for _, m := range matrices {
			if m.runs > 0 {
				directions++
				runs += m.runs
				for i, c := range m.counts {
					merged[i] += c
				}
			}
		}
		return glrlmFeatures(merged, runs, ng, maxRun, float64(voxels*directions)), directions
	}
	var features []GLRLMFeatures
	for _, m := range matrices {
		if m.runs > 0 {
			features = append(features, glrlmFeatures(m.counts, m.runs, ng, m.maxRun, float64(voxels)))
		}
	}
	return averageFeatures(features), len(features)
}

func nanGLCM() GLCMFeatures {
	var f GLCMFeatures
	for _, v := range glcmFields(&f) {
		*v = math.NaN()
	}
	return f
}

// averageFeatures averages feature structs field by field, in order, over
// the directions where the feature is defined (NaN when it is nowhere).
func averageFeatures[T GLCMFeatures | GLRLMFeatures](features []T) T {
	var out T
	outFields := featureFields(&out)
	defined := make([]int, len(outFields))
	for i := range features {
		for k, v := range featureFields(&features[i]) {
			if !math.IsNaN(*v) {
				*outFields[k] += *v
				defined[k]++
			}
		}
	}
	for k, v := range outFields {
		if defined[k] == 0 {
			*v = math.NaN()
		} else {
			*v /= float64(defined[k])
		}
	}
	return out
}

func featureFields[T GLCMFeatures | GLRLMFeatures](f *T) []*float64 {
	switch f := any(f).(type) {
	case *GLCMFeatures:
		return glcmFields(f)
	case *GLRLMFeatures:
		return glrlmFields(f)
	}
	return nil
}

func glcmFields(f *GLCMFeatures) []*float64 {
	return []*float64{&f.JointMaximum, &f.JointAverage, &f.JointVariance, &f.JointEntropy,
		&f.DifferenceAverage, &f.DifferenceVariance, &f.DifferenceEntropy,
		&f.SumAverage, &f.SumVariance, &f.SumEntropy, &f.AngularSecondMoment, &f.Contrast, &f.Dissimilarity,
		&f.InverseDifference, &f.NormalisedInverseDifference, &f.InverseDifferenceMoment, &f.NormalisedInverseDifferenceMoment,
		&f.InverseVariance, &f.Correlation, &f.Autocorrelation, &f.ClusterTendency, &f.ClusterShade, &f.ClusterProminence,
		&f.InformationCorrelation1, &f.InformationCorrelation2}
}

func glrlmFields(f *GLRLMFeatures) []*float64 {
	return []*float64{&f.ShortRunsEmphasis, &f.LongRunsEmphasis, &f.LowGreyLevelRunEmphasis, &f.HighGreyLevelRunEmphasis,
		&f.ShortRunLowGreyLevelEmphasis, &f.ShortRunHighGreyLevelEmphasis, &f.LongRunLowGreyLevelEmphasis, &f.LongRunHighGreyLevelEmphasis,
		&f.GreyLevelNonUniformity, &f.NormalisedGreyLevelNonUniformity, &f.RunLengthNonUniformity, &f.NormalisedRunLengthNonUniformity,
		&f.RunPercentage, &f.GreyLevelVariance, &f.RunLengthVariance, &f.RunEntropy}
}

// TextureFeature is one named feature value, for tables and CSV export.
type TextureFeature struct {
	// Family is "GLCM" or "GLRLM"; Name is the IBSI feature name.
	Family, Name string
	Value        float64
}

var (
	glcmNames = []string{"Joint maximum", "Joint average", "Joint variance", "Joint entropy",
		"Difference average", "Difference variance", "Difference entropy",
		"Sum average", "Sum variance", "Sum entropy", "Angular second moment", "Contrast", "Dissimilarity",
		"Inverse difference", "Normalised inverse difference", "Inverse difference moment", "Normalised inverse difference moment",
		"Inverse variance", "Correlation", "Autocorrelation", "Cluster tendency", "Cluster shade", "Cluster prominence",
		"Information correlation 1", "Information correlation 2"}
	glrlmNames = []string{"Short runs emphasis", "Long runs emphasis", "Low grey level run emphasis", "High grey level run emphasis",
		"Short run low grey level emphasis", "Short run high grey level emphasis", "Long run low grey level emphasis", "Long run high grey level emphasis",
		"Grey level non-uniformity", "Normalised grey level non-uniformity", "Run length non-uniformity", "Normalised run length non-uniformity",
		"Run percentage", "Grey level variance", "Run length variance", "Run entropy"}
)

// List returns every feature with its IBSI name, GLCM first, in the order
// of the struct fields.
func (t TextureFeatures) List() []TextureFeature {
	out := make([]TextureFeature, 0, len(glcmNames)+len(glrlmNames))
	for i, v := range glcmFields(&t.GLCM) {
		out = append(out, TextureFeature{Family: "GLCM", Name: glcmNames[i], Value: *v})
	}
	for i, v := range glrlmFields(&t.GLRLM) {
		out = append(out, TextureFeature{Family: "GLRLM", Name: glrlmNames[i], Value: *v})
	}
	return out
}

// TextureFeatures computes the texture features of the mask's voxels;
// valueAt reads a voxel's value (ok false or a non-finite value leaves the
// voxel out).
func (m *VolumeMask) TextureFeatures(ctx context.Context, valueAt VoxelValueFunc, opts TextureOptions) (TextureFeatures, error) {
	if err := m.validate(); err != nil {
		return TextureFeatures{}, err
	}
	if valueAt == nil {
		return TextureFeatures{}, fmt.Errorf("%w: no value accessor", ErrInvalidTexture)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plane := m.Columns * m.Rows
	return computeTexture(ctx, func(visit func(x, y, z int, value float64, ok bool)) error {
		for z := 0; z < m.Slices; z++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for i, on := range m.Voxels[z*plane : (z+1)*plane] {
				if on {
					x, y := i%m.Columns, i/m.Columns
					v, ok := valueAt(x, y, z)
					visit(x, y, z, v, ok)
				}
			}
		}
		return nil
	}, opts)
}

// TextureFeatures computes the texture features of the VOI's voxels whose
// centre is inside the primitive (texture needs a binary region, so
// partial-volume weights are not used).
func (m *VOIMask) TextureFeatures(ctx context.Context, valueAt VoxelValueFunc, opts TextureOptions) (TextureFeatures, error) {
	if m == nil || valueAt == nil {
		return TextureFeatures{}, fmt.Errorf("%w: texture needs a mask and a value accessor", ErrInvalidTexture)
	}
	return computeTexture(ctx, func(visit func(x, y, z int, value float64, ok bool)) error {
		for _, voxel := range m.Voxels {
			if voxel.CentreInside {
				v, ok := valueAt(voxel.Column, voxel.Row, voxel.Slice)
				visit(voxel.Column, voxel.Row, voxel.Slice, v, ok)
			}
		}
		return nil
	}, opts)
}
