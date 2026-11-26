package render

import (
	"context"
	"errors"
	"image"
	"sync"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// Fused volume rendering (#1180) composites a secondary volume (PET or SPECT
// activity) into the ray cast of a primary anatomical volume (CT or MR). The
// ray, its samples, clipping and the primary transfer function are those of
// RenderVRPreparedContext; at every sample the secondary volume is sampled at
// the same patient point (mapped through the registration when the volumes do
// not share a Frame of Reference) and classified by its own transfer function
// and window. The two opacities add, and their premultiplied colours mix, before
// the usual front-to-back accumulation. The approach follows the documented
// behaviour of VTK's multi-volume ray caster; no VTK code is copied.
//
// Scope: rays traverse the primary volume's box, so secondary activity outside
// it is not drawn; the secondary is classified without shading or gradient
// opacity (activity reads as emission); only DVR is fused.

// ErrFusedVRUnsupportedMode reports a fused render requested for a projection
// mode other than DVR (MIP, MinIP, Average). The returned image is the primary
// volume alone.
var ErrFusedVRUnsupportedMode = errors.New("dicom/render: fused volume rendering requires DVR mode")

// VRFusionLayer is the secondary volume of a fused volume rendering.
type VRFusionLayer struct {
	// Volume is the secondary (functional) volume.
	Volume *Volume
	// PrimaryToSecondary maps a primary patient point into the secondary
	// volume's patient coordinates. Nil is the identity, for volumes sharing
	// a Frame of Reference. A mapper error matching ErrMapperOutsideDomain
	// leaves that secondary sample empty; any other error fails the render
	// atomically with a *FusionMappingError.
	PrimaryToSecondary PatientPointMapper
	// TF classifies secondary values. A normalized function is addressed by
	// the Window coordinate; an HU-domain function by the secondary value
	// range, as the primary DVR does. The zero value uses
	// NewVRFusionPaletteTransferFunction(nil, 0): a gray ramp.
	TF VRTransferFunction
	// Window is the secondary window; an invalid one uses the volume's
	// recommended window.
	Window WindowLevel
	// Opacity in [0,1] scales every secondary sample opacity before the
	// step correction; it is the blend of the secondary over the primary.
	Opacity float64
}

// VRFusionReport counts the secondary samples of a fused render.
type VRFusionReport struct {
	// SecondarySamples is the number of DVR samples at which the secondary
	// was looked up.
	SecondarySamples int64
	// SecondaryInBounds is the number of those samples inside the secondary
	// volume. Zero is reported with ErrFusionNoOverlap.
	SecondaryInBounds int64
}

// NewVRFusionPaletteTransferFunction returns the normalized transfer function
// of a functional overlay: the palette's colours over the window (gray when
// palette is nil) and an opacity that rises linearly from zero at the window
// floor to one at its ceiling, so low uptake stays transparent. A
// non-positive opacityUnitDistanceMM uses DefaultVROpacityUnitDistanceMM.
func NewVRFusionPaletteTransferFunction(palette *display.PaletteColorLUT, opacityUnitDistanceMM float64) VRTransferFunction {
	const entries = 256
	colors := make([]TFColorStop, entries)
	for index := range colors {
		position := float64(index) / float64(entries-1)
		r, g, b := position, position, position
		if palette != nil {
			red, green, blue := palette.At(index)
			r, g, b = float64(red)/255, float64(green)/255, float64(blue)/255
		}
		colors[index] = TFColorStop{HU: position, R: r, G: g, B: b}
	}
	alpha := []TFAlphaStop{{HU: 0, A: 0}, {HU: 1, A: 1}}
	return NewNormalizedVRTransferFunction(colors, alpha, opacityUnitDistanceMM)
}

// RenderFusedVRPreparedContext renders prepared with the secondary layer
// fused into every DVR sample. With a zero layer opacity the image equals
// RenderVRPreparedContext's. A non-DVR preset returns the primary image with
// ErrFusedVRUnsupportedMode; no secondary sample inside the secondary volume
// returns the (primary-only) image with ErrFusionNoOverlap; a mapper failure
// returns a nil image and a *FusionMappingError.
func RenderFusedVRPreparedContext(ctx context.Context, prepared *VRPreparedVolume, secondary VRFusionLayer, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, quality VRQuality) (image.Image, VRFusionReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if preset.Mode != VRModeDVR {
		img, err := RenderVRPreparedContext(ctx, prepared, cam, preset, window, shading, clip, quality)
		if err != nil {
			return img, VRFusionReport{}, err
		}
		return img, VRFusionReport{}, ErrFusedVRUnsupportedMode
	}
	if secondary.Volume == nil {
		return nil, VRFusionReport{}, invalidMPRField("FusionVolume")
	}
	if !finite(secondary.Opacity) || secondary.Opacity < 0 || secondary.Opacity > 1 {
		return nil, VRFusionReport{}, invalidMPRField("FusionOpacity")
	}
	if secondary.Opacity == 0 {
		img, err := RenderVRPreparedContext(ctx, prepared, cam, preset, window, shading, clip, quality)
		return img, VRFusionReport{}, err
	}
	reader, err := secondary.Volume.AcquireReader()
	if err != nil {
		return nil, VRFusionReport{}, err
	}
	defer reader.Close()
	renderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	frame := newVRFusionFrame(secondary, reader, cancel)
	img, err := renderVRPreparedContext(renderCtx, prepared, cam, preset, window, shading, clip, quality, frame)
	report := frame.report()
	if frame.mappingErr != nil {
		return nil, report, frame.mappingErr
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return img, report, ctxErr
		}
		return img, report, err
	}
	if report.SecondaryInBounds == 0 {
		return img, report, ErrFusionNoOverlap
	}
	return img, report, nil
}

// RenderFusedVRPreparedPreviewContext is the interaction frame of
// RenderFusedVRPreparedContext: shading and gradient opacity are disabled, as
// in RenderVRPreparedPreviewContext.
func RenderFusedVRPreparedPreviewContext(ctx context.Context, prepared *VRPreparedVolume, secondary VRFusionLayer, cam VRCamera, preset VRPreset, window WindowLevel, clip *VRClip, quality VRQuality) (image.Image, VRFusionReport, error) {
	preset.GradientOpacityScale, preset.GradientOpacity = 0, nil
	return RenderFusedVRPreparedContext(ctx, prepared, secondary, cam, preset, window, false, clip, quality)
}

// vrFusionFrame is the per-render secondary classification shared by the
// row workers.
type vrFusionFrame struct {
	reader           *VolumeReader
	mapper           PatientPointMapper
	tf               VRTransferFunction
	lut              VRLUT
	window           WindowLevel
	dataMin, dataMax float64
	opacity          float64
	unitDistance     float64
	cancel           context.CancelFunc

	mappingOnce sync.Once
	mappingErr  error

	workersMu sync.Mutex
	workers   []*vrFusionWorker
}

func newVRFusionFrame(layer VRFusionLayer, reader *VolumeReader, cancel context.CancelFunc) *vrFusionFrame {
	tf := layer.TF
	if len(tf.ColorStops()) == 0 && len(tf.AlphaStops()) == 0 {
		tf = NewVRFusionPaletteTransferFunction(nil, 0)
	}
	window := normalizeWindow(layer.Window, layer.Volume.RecommendedWindow())
	dataMin, dataMax := 0.0, 1.0
	var lut VRLUT
	if tf.Domain() == VRTransferDomainHU {
		if minimum, maximum, ok := layer.Volume.HURange(); ok {
			dataMin, dataMax = minimum, maximum
		}
		lut = tf.BakeLUT(dataMin, dataMax, tf.PreferredLUTSize())
	} else {
		lut = tf.BakeWindowLUT(window, tf.PreferredLUTSize())
	}
	return &vrFusionFrame{
		reader:       reader,
		mapper:       layer.PrimaryToSecondary,
		tf:           tf,
		lut:          lut,
		window:       window,
		dataMin:      dataMin,
		dataMax:      dataMax,
		opacity:      layer.Opacity,
		unitDistance: tf.OpacityUnitDistanceMM(),
		cancel:       cancel,
	}
}

// newWorker returns the sampling state of one row worker; primary is the
// volume whose texture coordinates the rays carry. A nil frame has no worker.
func (f *vrFusionFrame) newWorker(primary *Volume) *vrFusionWorker {
	if f == nil {
		return nil
	}
	worker := &vrFusionWorker{frame: f, primary: primary}
	f.workersMu.Lock()
	f.workers = append(f.workers, worker)
	f.workersMu.Unlock()
	return worker
}

func (f *vrFusionFrame) fail(err error) {
	f.mappingOnce.Do(func() {
		if err == nil {
			err = invalidMPRField("PrimaryToSecondary")
		}
		f.mappingErr = &FusionMappingError{Err: err}
		f.cancel()
	})
}

// report sums the worker counters; call it after the workers finished.
func (f *vrFusionFrame) report() VRFusionReport {
	var report VRFusionReport
	f.workersMu.Lock()
	defer f.workersMu.Unlock()
	for _, worker := range f.workers {
		report.SecondarySamples += worker.samples
		report.SecondaryInBounds += worker.inBounds
	}
	return report
}

// vrFusionWorker is owned by one row worker, so its counters need no
// synchronization.
type vrFusionWorker struct {
	frame             *vrFusionFrame
	primary           *Volume
	samples, inBounds int64
	failed            bool
}

// sample returns the step-corrected, non-premultiplied linear colour and
// opacity of the secondary volume at the primary texture coordinate tex.
func (w *vrFusionWorker) sample(tex Vec3, stepLen float64) RGBA {
	if w == nil || w.failed {
		return RGBA{}
	}
	frame := w.frame
	w.samples++
	patient := w.primary.VoxelToPatient(w.primary.TextureToVoxel(tex))
	if frame.mapper != nil {
		mapped, err := frame.mapper(patient)
		if err != nil && errors.Is(err, ErrMapperOutsideDomain) {
			return RGBA{}
		}
		if err != nil || !finiteMPRVec3(mapped) {
			w.failed = true
			frame.fail(err)
			return RGBA{}
		}
		patient = mapped
	}
	value, ok := frame.reader.SamplePatient(patient)
	if !ok {
		return RGBA{}
	}
	w.inBounds++
	rgba := dvrTransferSample(frame.tf, frame.lut, value, frame.window, frame.dataMin, frame.dataMax)
	rgba.A = CorrectVRAlpha(rgba.A, frame.opacity, stepLen, frame.unitDistance)
	return rgba
}

// mixVRFusionSample combines the primary sample (non-premultiplied linear
// colour, step-corrected opacity) with the secondary one: the opacities add,
// clamped to one, and the colour is the opacity-weighted mix.
func mixVRFusionSample(r, g, b, alpha float64, secondary RGBA) (float64, float64, float64, float64) {
	total := alpha + secondary.A
	if total <= 0 {
		return r, g, b, 0
	}
	inv := 1 / total
	r = (alpha*r + secondary.A*secondary.R) * inv
	g = (alpha*g + secondary.A*secondary.G) * inv
	b = (alpha*b + secondary.A*secondary.B) * inv
	if total > 1 {
		total = 1
	}
	return r, g, b, total
}
