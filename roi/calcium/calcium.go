package calcium

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/codes/sct"
)

// Defaults of the published Agatston method.
const (
	DefaultThresholdHU          = 130.0
	DefaultMinLesionAreaMM2     = 1.0
	DefaultReferenceThicknessMM = 3.0
)

var (
	// ErrInvalidGeometry reports a stack that cannot be scored: no slices,
	// missing pixel spacing, slice positions that are not strictly monotonic,
	// or a single slice without a thickness.
	ErrInvalidGeometry = errors.New("calcium: invalid geometry")
	// ErrIrregularSpacing reports slice positions that are not evenly spaced
	// (a missing slice or mixed increments); the increment normalization of
	// the Agatston score needs one increment.
	ErrIrregularSpacing = errors.New("calcium: slices are not evenly spaced")
	// ErrInvalidOptions reports a negative or non-finite option.
	ErrInvalidOptions = errors.New("calcium: invalid options")
	// ErrUnknownLesion reports a lesion ID that is not in the analysis.
	ErrUnknownLesion = errors.New("calcium: unknown lesion")
	// ErrUnknownVessel reports a vessel that is not one of Vessels() or
	// Unassigned.
	ErrUnknownVessel = errors.New("calcium: unknown vessel")
)

// Vessel is the coronary artery a lesion is assigned to.
type Vessel string

const (
	// Unassigned marks a candidate lesion that is not counted (bone, valve
	// or aortic calcium, noise).
	Unassigned Vessel = ""
	LM         Vessel = "LM"
	LAD        Vessel = "LAD"
	LCX        Vessel = "LCX"
	RCA        Vessel = "RCA"
	// Other is any other coronary artery (ramus intermedius, a graft),
	// counted in the total.
	Other Vessel = "Other"
	// Total labels the sum of the assigned vessels in a Score.
	Total Vessel = "Total"
)

var vessels = []Vessel{LM, LAD, LCX, RCA, Other}

// Vessels returns the vessels lesions can be assigned to, in report order.
func Vessels() []Vessel { return append([]Vessel(nil), vessels...) }

// Valid reports one of Vessels().
func (v Vessel) Valid() bool {
	for _, known := range vessels {
		if v == known {
			return true
		}
	}
	return false
}

// Name is the vessel's name for display.
func (v Vessel) Name() string {
	switch v {
	case LM:
		return "Left main"
	case LAD:
		return "Left anterior descending"
	case LCX:
		return "Left circumflex"
	case RCA:
		return "Right coronary"
	case Other:
		return "Other coronary"
	case Total:
		return "Total"
	case Unassigned:
		return "Unassigned"
	default:
		return string(v)
	}
}

// Site is the anatomic site code (SNOMED CT) of the vessel; Other and Total
// are "Coronary artery".
func (v Vessel) Site() codes.Code {
	switch v {
	case LM:
		return sct.LeftMainCoronaryArtery
	case LAD:
		return sct.LeftAnteriorDescendingCoronaryArtery
	case LCX:
		return sct.CircumflexCoronaryArtery
	case RCA:
		return sct.RightCoronaryArtery
	default:
		return sct.CoronaryArtery
	}
}

// Geometry describes the scored stack.
type Geometry struct {
	Columns, Rows int
	// PixelSpacing is the DICOM Pixel Spacing: the distance between rows,
	// then between columns, in mm.
	PixelSpacing [2]float64
	// SlicePositions holds each slice's position (mm) along the stack
	// normal, strictly increasing or decreasing; slice z of Detect is
	// SlicePositions[z].
	SlicePositions []float64
	// SliceThicknessMM is the reconstructed slice thickness; it is the
	// increment of a single slice and is otherwise reported only.
	SliceThicknessMM float64
}

// Options tune the method; zero values select the published defaults.
type Options struct {
	// ThresholdHU is the attenuation threshold (130 HU).
	ThresholdHU float64
	// MinLesionAreaMM2 is the smallest lesion area in a slice (1 mm²).
	MinLesionAreaMM2 float64
	// ReferenceThicknessMM is the slice thickness of the original method
	// (3 mm) the Agatston score is normalized to.
	ReferenceThicknessMM float64
	// FourConnected joins pixels by their edges only; the default joins
	// diagonal neighbours too (8-connected).
	FourConnected bool
	// MassCalibration is the calibration factor c in mg/cm³ per HU of the
	// mass score; zero reports no mass.
	MassCalibration float64
}

func (o Options) resolved() (Options, error) {
	for _, value := range []float64{o.ThresholdHU, o.MinLesionAreaMM2, o.ReferenceThicknessMM, o.MassCalibration} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return Options{}, fmt.Errorf("%w: non-finite value", ErrInvalidOptions)
		}
	}
	if o.MinLesionAreaMM2 < 0 || o.ReferenceThicknessMM < 0 || o.MassCalibration < 0 {
		return Options{}, fmt.Errorf("%w: negative value", ErrInvalidOptions)
	}
	if o.ThresholdHU == 0 {
		o.ThresholdHU = DefaultThresholdHU
	}
	if o.MinLesionAreaMM2 == 0 {
		o.MinLesionAreaMM2 = DefaultMinLesionAreaMM2
	}
	if o.ReferenceThicknessMM == 0 {
		o.ReferenceThicknessMM = DefaultReferenceThicknessMM
	}
	return o, nil
}

// DensityWeight is the Agatston weighting factor of a lesion's peak
// attenuation: 1 below 200 HU, 2 below 300 HU, 3 below 400 HU, otherwise 4.
func DensityWeight(peakHU float64) int {
	switch {
	case peakHU >= 400:
		return 4
	case peakHU >= 300:
		return 3
	case peakHU >= 200:
		return 2
	default:
		return 1
	}
}

// Lesion is a connected region of one slice at or above the threshold.
type Lesion struct {
	// ID is the lesion's index in Analysis.Lesions.
	ID int
	// Slice is the slice index z of Detect.
	Slice int
	// Group is the ID of the lowest lesion of the 3D lesion this lesion
	// belongs to: lesions of adjacent slices sharing a pixel position are
	// one group.
	Group int
	// Pixels are the lesion's pixel indices (row × Columns + column),
	// ascending.
	Pixels []int
	// CentroidX and CentroidY are the mean column and row of the pixels.
	CentroidX, CentroidY float64
	AreaMM2              float64
	PeakHU               float64
	MeanHU               float64
	// Weight is DensityWeight(PeakHU).
	Weight int
	// Agatston is AreaMM2 × Weight × the analysis's NormalizationFactor.
	Agatston float64
	// VolumeMM3 is AreaMM2 × the slice increment.
	VolumeMM3 float64
	Vessel    Vessel
}

// MassMG is the lesion's calcium mass for calibration factor c (mg/cm³ per
// HU): c × volume (cm³) × mean attenuation.
func (l Lesion) MassMG(c float64) float64 {
	return c * l.VolumeMM3 / 1000 * l.MeanHU
}

// SliceReader returns the attenuation (HU) of slice z, row by row
// (Columns × Rows values).
type SliceReader func(z int) ([]float32, error)

// SliceValues reads slices from memory.
func SliceValues(slices [][]float32) SliceReader {
	return func(z int) ([]float32, error) {
		if z < 0 || z >= len(slices) {
			return nil, fmt.Errorf("calcium: slice %d out of range", z)
		}
		return slices[z], nil
	}
}

// Analysis holds the candidate lesions of a stack and their vessel
// assignment. It is not safe for concurrent use.
type Analysis struct {
	Geometry Geometry
	// Options are the options used, defaults resolved.
	Options Options
	// IncrementMM is the distance between adjacent slices.
	IncrementMM float64
	// NormalizationFactor is IncrementMM / Options.ReferenceThicknessMM, the
	// factor applied to every lesion's Agatston score.
	NormalizationFactor float64
	Lesions             []Lesion
	bySlice             [][]int
}

// Increment returns the distance between adjacent slices of g: the mean gap
// (the span over the number of gaps), which every gap must match within 5%
// (at least 0.01 mm), or the slice thickness of a single slice.
func Increment(g Geometry) (float64, error) {
	positions := g.SlicePositions
	if len(positions) == 0 {
		return 0, fmt.Errorf("%w: no slices", ErrInvalidGeometry)
	}
	for _, p := range positions {
		if math.IsNaN(p) || math.IsInf(p, 0) {
			return 0, fmt.Errorf("%w: non-finite slice position", ErrInvalidGeometry)
		}
	}
	if len(positions) == 1 {
		if !(g.SliceThicknessMM > 0) || math.IsInf(g.SliceThicknessMM, 0) {
			return 0, fmt.Errorf("%w: a single slice needs its slice thickness", ErrInvalidGeometry)
		}
		return g.SliceThicknessMM, nil
	}
	gaps := make([]float64, len(positions)-1)
	ascending := positions[1] > positions[0]
	for i := range gaps {
		gap := positions[i+1] - positions[i]
		if !ascending {
			gap = -gap
		}
		if !(gap > 0) {
			return 0, fmt.Errorf("%w: slice positions are not strictly monotonic at slice %d", ErrInvalidGeometry, i+1)
		}
		gaps[i] = gap
	}
	increment := math.Abs(positions[len(positions)-1]-positions[0]) / float64(len(gaps))
	tolerance := math.Max(0.01, 0.05*increment)
	for i, gap := range gaps {
		if math.Abs(gap-increment) > tolerance {
			return 0, fmt.Errorf("%w: gap of %.3g mm after slice %d, increment %.3g mm", ErrIrregularSpacing, gap, i, increment)
		}
	}
	return increment, nil
}

// Detect finds the candidate lesions of every slice of the stack. All
// lesions start unassigned.
func Detect(ctx context.Context, g Geometry, read SliceReader, opts Options) (*Analysis, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	if g.Columns <= 0 || g.Rows <= 0 {
		return nil, fmt.Errorf("%w: %d×%d pixels", ErrInvalidGeometry, g.Columns, g.Rows)
	}
	if !(g.PixelSpacing[0] > 0) || !(g.PixelSpacing[1] > 0) || math.IsInf(g.PixelSpacing[0], 0) || math.IsInf(g.PixelSpacing[1], 0) {
		return nil, fmt.Errorf("%w: pixel spacing %v", ErrInvalidGeometry, g.PixelSpacing)
	}
	if read == nil {
		return nil, fmt.Errorf("%w: no slice reader", ErrInvalidGeometry)
	}
	increment, err := Increment(g)
	if err != nil {
		return nil, err
	}
	analysis := &Analysis{
		Geometry:            g,
		Options:             opts,
		IncrementMM:         increment,
		NormalizationFactor: increment / opts.ReferenceThicknessMM,
		bySlice:             make([][]int, len(g.SlicePositions)),
	}
	analysis.Geometry.SlicePositions = append([]float64(nil), g.SlicePositions...)
	pixelArea := g.PixelSpacing[0] * g.PixelSpacing[1]
	plane := g.Columns * g.Rows
	visited := make([]bool, plane)
	var stack, pixels []int
	for z := range g.SlicePositions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values, err := read(z)
		if err != nil {
			return nil, fmt.Errorf("calcium: read slice %d: %w", z, err)
		}
		if len(values) != plane {
			return nil, fmt.Errorf("%w: slice %d has %d values, want %d", ErrInvalidGeometry, z, len(values), plane)
		}
		clear(visited)
		for start, value := range values {
			if visited[start] || !(float64(value) >= opts.ThresholdHU) {
				continue
			}
			pixels = pixels[:0]
			stack = append(stack[:0], start)
			visited[start] = true
			for len(stack) > 0 {
				index := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				pixels = append(pixels, index)
				x, y := index%g.Columns, index/g.Columns
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						if (dx == 0 && dy == 0) || (opts.FourConnected && dx != 0 && dy != 0) {
							continue
						}
						nx, ny := x+dx, y+dy
						if nx < 0 || ny < 0 || nx >= g.Columns || ny >= g.Rows {
							continue
						}
						neighbour := ny*g.Columns + nx
						if visited[neighbour] || !(float64(values[neighbour]) >= opts.ThresholdHU) {
							continue
						}
						visited[neighbour] = true
						stack = append(stack, neighbour)
					}
				}
			}
			area := float64(len(pixels)) * pixelArea
			if area < opts.MinLesionAreaMM2*(1-1e-9) {
				continue
			}
			analysis.addLesion(z, pixels, values, area)
		}
	}
	analysis.groupAcrossSlices()
	return analysis, nil
}

func (a *Analysis) addLesion(z int, pixels []int, values []float32, area float64) {
	sorted := append([]int(nil), pixels...)
	sort.Ints(sorted)
	lesion := Lesion{ID: len(a.Lesions), Slice: z, Pixels: sorted, AreaMM2: area, PeakHU: math.Inf(-1)}
	var sum, sumX, sumY float64
	for _, index := range sorted {
		value := float64(values[index])
		sum += value
		lesion.PeakHU = math.Max(lesion.PeakHU, value)
		sumX += float64(index % a.Geometry.Columns)
		sumY += float64(index / a.Geometry.Columns)
	}
	n := float64(len(sorted))
	lesion.MeanHU = sum / n
	lesion.CentroidX, lesion.CentroidY = sumX/n, sumY/n
	lesion.Weight = DensityWeight(lesion.PeakHU)
	lesion.Agatston = area * float64(lesion.Weight) * a.NormalizationFactor
	lesion.VolumeMM3 = area * a.IncrementMM
	lesion.Group = lesion.ID
	a.Lesions = append(a.Lesions, lesion)
	a.bySlice[z] = append(a.bySlice[z], lesion.ID)
}

// groupAcrossSlices joins lesions of adjacent slices that share a pixel
// position into one group.
func (a *Analysis) groupAcrossSlices() {
	parent := make([]int, len(a.Lesions))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for z := 1; z < len(a.bySlice); z++ {
		for _, current := range a.bySlice[z] {
			for _, previous := range a.bySlice[z-1] {
				if overlap(a.Lesions[current].Pixels, a.Lesions[previous].Pixels) {
					ra, rb := find(current), find(previous)
					if ra < rb {
						ra, rb = rb, ra
					}
					parent[ra] = rb
				}
			}
		}
	}
	for i := range a.Lesions {
		a.Lesions[i].Group = find(i)
	}
}

func overlap(a, b []int) bool {
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// SliceLesions returns the IDs of the lesions of slice z.
func (a *Analysis) SliceLesions(z int) []int {
	if a == nil || z < 0 || z >= len(a.bySlice) {
		return nil
	}
	return append([]int(nil), a.bySlice[z]...)
}

// LesionAt returns the lesion containing pixel (x, y) of slice z.
func (a *Analysis) LesionAt(z, x, y int) (int, bool) {
	if a == nil || z < 0 || z >= len(a.bySlice) || x < 0 || y < 0 || x >= a.Geometry.Columns || y >= a.Geometry.Rows {
		return 0, false
	}
	index := y*a.Geometry.Columns + x
	for _, id := range a.bySlice[z] {
		pixels := a.Lesions[id].Pixels
		if i := sort.SearchInts(pixels, index); i < len(pixels) && pixels[i] == index {
			return id, true
		}
	}
	return 0, false
}

func (a *Analysis) checkAssignment(id int, vessel Vessel) error {
	if a == nil || id < 0 || id >= len(a.Lesions) {
		return fmt.Errorf("%w: %d", ErrUnknownLesion, id)
	}
	if vessel != Unassigned && !vessel.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownVessel, vessel)
	}
	return nil
}

// Assign assigns one lesion to a vessel; Unassigned removes it from the
// scores.
func (a *Analysis) Assign(id int, vessel Vessel) error {
	if err := a.checkAssignment(id, vessel); err != nil {
		return err
	}
	a.Lesions[id].Vessel = vessel
	return nil
}

// AssignGroup assigns every lesion of the 3D group of lesion id to a vessel
// and returns how many lesions it assigned.
func (a *Analysis) AssignGroup(id int, vessel Vessel) (int, error) {
	if err := a.checkAssignment(id, vessel); err != nil {
		return 0, err
	}
	group, count := a.Lesions[id].Group, 0
	for i := range a.Lesions {
		if a.Lesions[i].Group == group {
			a.Lesions[i].Vessel = vessel
			count++
		}
	}
	return count, nil
}

// SetMassCalibration sets the calibration factor c (mg/cm³ per HU) of the
// mass score; zero reports no mass.
func (a *Analysis) SetMassCalibration(c float64) error {
	if a == nil {
		return ErrInvalidOptions
	}
	if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 {
		return fmt.Errorf("%w: mass calibration %v", ErrInvalidOptions, c)
	}
	a.Options.MassCalibration = c
	return nil
}

// VesselScore sums the lesions of one vessel.
type VesselScore struct {
	Vessel    Vessel
	Lesions   int
	Agatston  float64
	VolumeMM3 float64
	// MassMG is zero when the score has no mass calibration.
	MassMG float64
}

func (s *VesselScore) add(lesion Lesion, calibration float64) {
	s.Lesions++
	s.Agatston += lesion.Agatston
	s.VolumeMM3 += lesion.VolumeMM3
	s.MassMG += lesion.MassMG(calibration)
}

// Score is the scoring of an analysis at one moment.
type Score struct {
	// Vessels holds one entry per Vessels(), in that order.
	Vessels []VesselScore
	// Total sums the assigned vessels.
	Total VesselScore
	// Unassigned sums the candidates left out of the scores.
	Unassigned VesselScore
	// MassCalibrated reports a mass calibration factor; masses are zero
	// otherwise.
	MassCalibrated      bool
	MassCalibration     float64
	ThresholdHU         float64
	MinLesionAreaMM2    float64
	IncrementMM         float64
	SliceThicknessMM    float64
	NormalizationFactor float64
	Slices              int
}

// Vessel returns the score of one vessel, Total or Unassigned.
func (s Score) Vessel(vessel Vessel) VesselScore {
	switch vessel {
	case Total:
		return s.Total
	case Unassigned:
		return s.Unassigned
	}
	for _, score := range s.Vessels {
		if score.Vessel == vessel {
			return score
		}
	}
	return VesselScore{Vessel: vessel}
}

// Score sums the assigned lesions per vessel.
func (a *Analysis) Score() Score {
	score := Score{Total: VesselScore{Vessel: Total}, Unassigned: VesselScore{Vessel: Unassigned}}
	if a == nil {
		for _, vessel := range vessels {
			score.Vessels = append(score.Vessels, VesselScore{Vessel: vessel})
		}
		return score
	}
	calibration := a.Options.MassCalibration
	score.MassCalibrated = calibration > 0
	score.MassCalibration = calibration
	score.ThresholdHU = a.Options.ThresholdHU
	score.MinLesionAreaMM2 = a.Options.MinLesionAreaMM2
	score.IncrementMM = a.IncrementMM
	score.SliceThicknessMM = a.Geometry.SliceThicknessMM
	score.NormalizationFactor = a.NormalizationFactor
	score.Slices = len(a.Geometry.SlicePositions)
	index := map[Vessel]int{}
	for i, vessel := range vessels {
		index[vessel] = i
		score.Vessels = append(score.Vessels, VesselScore{Vessel: vessel})
	}
	for _, lesion := range a.Lesions {
		i, assigned := index[lesion.Vessel]
		if !assigned {
			score.Unassigned.add(lesion, calibration)
			continue
		}
		score.Vessels[i].add(lesion, calibration)
		score.Total.add(lesion, calibration)
	}
	return score
}
