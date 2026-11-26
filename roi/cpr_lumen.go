package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
)

var (
	ErrCPRLumenInput     = errors.New("roi: invalid CPR lumen analysis")
	ErrCPRLumenLimit     = errors.New("roi: CPR lumen analysis resource limit")
	ErrCPRLumenReference = errors.New("roi: selected lumen reference is not measurable")
)

const (
	MaxCPRLumenSections      = 512
	MaxCPRLumenGridSide      = 513
	MaxCPRLumenSamples       = 16_000_000
	MaxCPRLumenContourPoints = 1_000_000
)

// CPRLumenOptions define source-modality threshold segmentation on transported
// transverse planes. The reference is an explicit arc position, never an
// automatically presumed healthy maximum. Percentages compare like measures.
type CPRLumenOptions struct {
	ThresholdMin, ThresholdMax              float64
	SectionStepMM, PixelSpacingMM, RadiusMM float64
	ReferenceArcMM, RotationDegrees         float64
}

type CPRLumenSection struct {
	ArcLengthMM                                    float64
	Center, Normal, Binormal                       render.Vec3
	Valid                                          bool
	Reason                                         string
	AreaMM2, MinDiameterMM, MaxDiameterMM          float64
	AreaReductionPercent, DiameterReductionPercent float64
	// Contours are physical coordinates in the local Normal/Binormal plane.
	// Outer paths have positive area; holes have negative area.
	Contours [][]Point2D
}

type CPRLumenProfile struct {
	Options        CPRLumenOptions
	PathLengthMM   float64
	ReferenceIndex int
	Sections       []CPRLumenSection
}

func normalizeCPRLumenOptions(path *render.CPRPath, opts CPRLumenOptions) (CPRLumenOptions, int, []float64, error) {
	if path == nil || !finiteLumen(path.Length()) || path.Length() <= 0 {
		return opts, 0, nil, ErrCPRLumenInput
	}
	if opts.SectionStepMM == 0 {
		opts.SectionStepMM = 2
	}
	if opts.PixelSpacingMM == 0 {
		opts.PixelSpacingMM = .25
	}
	if opts.RadiusMM == 0 {
		opts.RadiusMM = 12
	}
	for _, value := range []float64{opts.ThresholdMin, opts.ThresholdMax, opts.SectionStepMM, opts.PixelSpacingMM, opts.RadiusMM, opts.ReferenceArcMM, opts.RotationDegrees} {
		if !finiteLumen(value) {
			return opts, 0, nil, ErrCPRLumenInput
		}
	}
	if opts.ThresholdMin >= opts.ThresholdMax || opts.SectionStepMM <= 0 || opts.PixelSpacingMM <= 0 || opts.RadiusMM <= 0 || opts.ReferenceArcMM < 0 || opts.ReferenceArcMM > path.Length() {
		return opts, 0, nil, ErrCPRLumenInput
	}
	half := math.Ceil(opts.RadiusMM / opts.PixelSpacingMM)
	count := math.Ceil(path.Length()/opts.SectionStepMM) + 1
	if half > float64((MaxCPRLumenGridSide-1)/2) || count > MaxCPRLumenSections {
		return opts, 0, nil, ErrCPRLumenLimit
	}
	side := 2*int(half) + 1
	positions := make([]float64, 0, int(count)+1)
	for i := 0; i < int(count); i++ {
		positions = append(positions, math.Min(path.Length(), float64(i)*opts.SectionStepMM))
	}
	positions = append(positions, opts.ReferenceArcMM)
	sort.Float64s(positions)
	unique := positions[:0]
	for _, arc := range positions {
		if len(unique) == 0 || math.Abs(arc-unique[len(unique)-1]) > 1e-9 {
			unique = append(unique, arc)
		}
	}
	if len(unique) > MaxCPRLumenSections || int64(side)*int64(side)*int64(len(unique)) > MaxCPRLumenSamples {
		return opts, 0, nil, ErrCPRLumenLimit
	}
	return opts, side, unique, nil
}

func finiteLumen(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

// AnalyzeCPRLumen measures geometry and scalar data, not rendered images.
// The source is leased once; each section uses bounded threshold and flood
// buffers. Missing seeds, truncated components and source FOV gaps remain
// explicitly invalid, rather than being reported as zero lumen/100% stenosis.
func AnalyzeCPRLumen(ctx context.Context, volume *render.Volume, path *render.CPRPath, opts CPRLumenOptions) (*CPRLumenProfile, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts, side, positions, err := normalizeCPRLumenOptions(path, opts)
	if err != nil || volume == nil {
		if err != nil {
			return nil, err
		}
		return nil, ErrCPRLumenInput
	}
	lease, err := volume.AcquireSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Close()
	reader, err := volume.AcquireReader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	profile := &CPRLumenProfile{Options: opts, PathLengthMM: path.Length(), ReferenceIndex: -1}
	contourPoints := 0
	for _, arc := range positions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		section, err := measureCPRLumenSection(ctx, reader.SamplePatient, path.FrameAt(arc), opts, side)
		if err != nil {
			return nil, err
		}
		for _, contour := range section.Contours {
			contourPoints += len(contour)
			if contourPoints > MaxCPRLumenContourPoints {
				return nil, ErrCPRLumenLimit
			}
		}
		if math.Abs(arc-opts.ReferenceArcMM) <= 1e-9 {
			profile.ReferenceIndex = len(profile.Sections)
		}
		profile.Sections = append(profile.Sections, section)
	}
	if profile.ReferenceIndex < 0 || !profile.Sections[profile.ReferenceIndex].Valid {
		return nil, fmt.Errorf("%w at %.3f mm", ErrCPRLumenReference, opts.ReferenceArcMM)
	}
	ref := profile.Sections[profile.ReferenceIndex]
	for i := range profile.Sections {
		s := &profile.Sections[i]
		if s.Valid {
			s.AreaReductionPercent = 100 * (1 - s.AreaMM2/ref.AreaMM2)
			s.DiameterReductionPercent = 100 * (1 - s.MinDiameterMM/ref.MinDiameterMM)
			if !finiteLumen(s.AreaReductionPercent) || !finiteLumen(s.DiameterReductionPercent) {
				return nil, ErrCPRLumenInput
			}
		}
	}
	return profile, nil
}

func measureCPRLumenSection(ctx context.Context, sample func(render.Vec3) (float64, bool), frame render.CPRPathSample, opts CPRLumenOptions, side int) (CPRLumenSection, error) {
	angle := opts.RotationDegrees * math.Pi / 180
	u := frame.Normal.Scale(math.Cos(angle)).Add(frame.Binormal.Scale(math.Sin(angle)))
	v := frame.Binormal.Scale(math.Cos(angle)).Sub(frame.Normal.Scale(math.Sin(angle)))
	section := CPRLumenSection{ArcLengthMM: frame.ArcLength, Center: frame.Position, Normal: u, Binormal: v}
	inside := make([]bool, side*side)
	available := make([]bool, len(inside))
	score := make([]float64, len(inside))
	half := side / 2
	for y := 0; y < side; y++ {
		if err := ctx.Err(); err != nil {
			return section, err
		}
		for x := 0; x < side; x++ {
			p := frame.Position.Add(u.Scale(float64(x-half) * opts.PixelSpacingMM)).Add(v.Scale(float64(y-half) * opts.PixelSpacingMM))
			value, ok := sample(p)
			index := y*side + x
			available[index] = ok && finiteLumen(value)
			inside[index] = available[index] && value >= opts.ThresholdMin && value <= opts.ThresholdMax
			score[index] = math.Min(value-opts.ThresholdMin, opts.ThresholdMax-value)
		}
	}
	seed := half*side + half
	if !inside[seed] {
		section.Reason = "centerline seed outside threshold or source"
		return section, nil
	}
	component := make([]bool, len(inside))
	queue := make([]int, 1, len(inside))
	queue[0] = seed
	component[seed] = true
	for head := 0; head < len(queue); head++ {
		if head&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return section, err
			}
		}
		index := queue[head]
		x, y := index%side, index/side
		if x == 0 || y == 0 || x == side-1 || y == side-1 {
			section.Reason = "lumen reaches analysis boundary"
			return section, nil
		}
		for _, neighbor := range []int{index - 1, index + 1, index - side, index + side} {
			if !available[neighbor] {
				section.Reason = "lumen reaches source FOV boundary"
				return section, nil
			}
			if inside[neighbor] && !component[neighbor] {
				component[neighbor] = true
				queue = append(queue, neighbor)
			}
		}
	}
	contours, err := IsoContours(side, side, func(x, y int) float64 {
		index := y*side + x
		if component[index] {
			return score[index]
		}
		// Preserve scalar interpolation at the selected component's boundary;
		// another disconnected above-threshold object is excluded explicitly.
		if !available[index] {
			return -1
		}
		return -math.Max(math.Abs(score[index]), 1e-9)
	}, 0)
	if err != nil {
		return section, err
	}
	var outer []Point2D
	largestArea := 0.0
	for _, contour := range contours {
		physical := make([]Point2D, len(contour.Points))
		for i, p := range contour.Points {
			physical[i] = Point2D{X: (p.X - float64(half)) * opts.PixelSpacingMM, Y: (p.Y - float64(half)) * opts.PixelSpacingMM}
		}
		area := ClosedPolygonArea(physical)
		section.AreaMM2 += area
		section.Contours = append(section.Contours, physical)
		if area > largestArea {
			largestArea, outer = area, physical
		}
	}
	if !(section.AreaMM2 > 0) || len(outer) < 3 {
		section.Reason = "no closed lumen contour"
		return section, nil
	}
	section.MinDiameterMM, section.MaxDiameterMM, err = PolygonFeretDiametersContext(ctx, outer)
	if err != nil {
		return section, err
	}
	section.Valid = true
	return section, nil
}
