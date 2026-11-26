package printmgmt

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// Image is the pixel content of one image box: preformatted, with any
// annotation or overlay already burned in (PS3.4 H.3.2.1). Grayscale images
// have one sample (MONOCHROME1 or MONOCHROME2, 8 bits, or 12 bits in 16);
// color images are RGB, 8 bits, color-by-plane. PixelData is little-endian.
type Image struct {
	Color       bool
	Rows        int
	Columns     int
	Bits        int // Bits Stored: 8, or 12 for grayscale in 16-bit containers
	Photometric string
	// PixelAspectRatio is vertical:horizontal; zero means square pixels.
	PixelAspectRatio [2]int
	PixelData        []byte
}

func (i *Image) clone() *Image {
	if i == nil {
		return nil
	}
	copy := *i
	copy.PixelData = append([]byte(nil), i.PixelData...)
	return &copy
}

// ImageBox is one position of a film box, numbered from 1 in row-major order.
type ImageBox struct {
	SOPClassUID        string
	SOPInstanceUID     string
	Position           int
	Polarity           string
	Image              *Image
	Presentation       ImageBoxPresentation
	Plan               PhysicalPlan
	PresentationLUTUID string
}

// FilmBoxPresentation are the Basic Film Box presentation attributes the SCP
// keeps. Empty values mean the printer default.
type FilmBoxPresentation struct {
	AnnotationDisplayFormatID string
	PresentationLUTUID        string
	FilmOrientation           string
	FilmSizeID                string
	MagnificationType         string
	SmoothingType             string
	BorderDensity             string
	EmptyImageDensity         string
	Trim                      string
	// MinDensity and MaxDensity are in hundredths of optical density;
	// Illumination and ReflectedAmbientLight in cd/m². See PrintDensity.
	MinDensity                                                              int
	MaxDensity                                                              int
	Illumination                                                            int
	ReflectedAmbientLight                                                   int
	MinDensitySet, MaxDensitySet, IlluminationSet, ReflectedAmbientLightSet bool
	Configuration                                                           string
}

// PrintDensity returns the film's density range and viewing conditions for
// GSDF printing and Presentation LUT Shape LIN OD, with the display package
// defaults (0.20..3.00 OD, 2000 and 10 cd/m²) for unset values.
func (p FilmBoxPresentation) PrintDensity() display.PrintDensity {
	out := display.DefaultPrintDensity
	if p.MinDensity > 0 {
		out.MinDensity = float64(p.MinDensity) / 100
	}
	if p.MaxDensity > 0 {
		out.MaxDensity = float64(p.MaxDensity) / 100
	}
	if p.Illumination > 0 {
		out.Illumination = float64(p.Illumination)
	}
	if p.ReflectedAmbientLight > 0 {
		out.ReflectedAmbientLight = float64(p.ReflectedAmbientLight)
	}
	if out.MinDensity >= out.MaxDensity {
		out.MinDensity, out.MaxDensity = display.DefaultPrintDensity.MinDensity, display.DefaultPrintDensity.MaxDensity
	}
	return out
}

// FilmBox is one film: STANDARD\Columns,Rows image boxes.
type FilmBox struct {
	SOPInstanceUID  string
	Color           bool
	Columns         int
	Rows            int
	Presentation    FilmBoxPresentation
	ImageBoxes      []ImageBox
	AnnotationBoxes []AnnotationBox
	Printed         bool
}

func (f FilmBox) clone() FilmBox {
	boxes := make([]ImageBox, len(f.ImageBoxes))
	for i, box := range f.ImageBoxes {
		box.Image = box.Image.clone()
		boxes[i] = box
	}
	f.ImageBoxes = boxes
	f.AnnotationBoxes = append([]AnnotationBox(nil), f.AnnotationBoxes...)
	return f
}

// FilmSessionPresentation are the Basic Film Session attributes.
type FilmSessionPresentation struct {
	SpecificCharacterSet string
	NumberOfCopies       int
	PrintPriority        string
	MediumType           string
	FilmDestination      string
	Label                string
	OwnerID              string
}

// Job is what the SCP hands to the Backend on N-ACTION: a deep copy of the
// working hierarchy, so later N-SETs cannot change it (PS3.4 H.4.1.2.4.3).
type Job struct {
	ID                string
	FilmSessionUID    string
	CallingAETitle    string
	Session           FilmSessionPresentation
	FilmBoxes         []FilmBox
	WholeSession      bool
	Copies            int
	RequestedAt       time.Time
	EmptyImageBoxes   int
	ImageBoxesPrinted int
	PresentationLUTs  []PresentationLUTInstance
}

// PrinterStatus is returned by Printer N-GET.
type PrinterStatus struct {
	Status                string
	Info                  string
	Name                  string
	Manufacturer          string
	ManufacturerModelName string
}

// ErrQueueFull is returned by a Backend that cannot accept another job; the
// SCP answers C601H (session) or C602H (film box).
var ErrQueueFull = errors.New("printmgmt: print queue is full")

// Backend executes print jobs. Print must either accept the job durably
// enough for the application's purposes and return nil, or refuse it; the
// SCP never calls Print twice for one N-ACTION. Print receives its own copy.
type Backend interface {
	Print(ctx context.Context, job Job) error
	PrinterStatus(ctx context.Context) (PrinterStatus, error)
}

// MemoryBackend captures jobs in memory for tests and examples; it prints
// nothing.
type MemoryBackend struct {
	mu     sync.Mutex
	max    int
	jobs   []Job
	status PrinterStatus
}

// NewMemoryBackend keeps at most maxJobs jobs (0 means 64) and reports a
// NORMAL printer.
func NewMemoryBackend(maxJobs int) *MemoryBackend {
	if maxJobs <= 0 {
		maxJobs = 64
	}
	return &MemoryBackend{max: maxJobs, status: PrinterStatus{Status: PrinterNormal, Info: "NORMAL", Name: "MEMORY", Manufacturer: "dicom-go", ManufacturerModelName: "printmgmt.MemoryBackend"}}
}

func (b *MemoryBackend) Print(ctx context.Context, job Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.jobs) >= b.max {
		return ErrQueueFull
	}
	b.jobs = append(b.jobs, cloneJob(job))
	return nil
}

func (b *MemoryBackend) PrinterStatus(ctx context.Context) (PrinterStatus, error) {
	if err := ctx.Err(); err != nil {
		return PrinterStatus{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status, nil
}

// SetPrinterStatus changes what Printer N-GET reports.
func (b *MemoryBackend) SetPrinterStatus(status PrinterStatus) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.status = status
}

// Jobs returns copies of the captured jobs.
func (b *MemoryBackend) Jobs() []Job {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Job, len(b.jobs))
	for i, job := range b.jobs {
		out[i] = cloneJob(job)
	}
	return out
}

func cloneJob(job Job) Job {
	boxes := make([]FilmBox, len(job.FilmBoxes))
	for i, box := range job.FilmBoxes {
		boxes[i] = box.clone()
	}
	job.FilmBoxes = boxes
	luts := make([]PresentationLUTInstance, len(job.PresentationLUTs))
	for i, p := range job.PresentationLUTs {
		luts[i] = p.clone()
	}
	job.PresentationLUTs = luts
	return job
}
