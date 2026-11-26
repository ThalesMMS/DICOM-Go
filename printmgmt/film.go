package printmgmt

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
)

// Steps of a PrintFilm session, named in StepStatus and PrintError.
const (
	StepCreateFilmSession = "Film Session N-CREATE"
	StepCreateFilmBox     = "Film Box N-CREATE"
	StepSetImageBox       = "Image Box N-SET"
	StepPrintFilmBox      = "Film Box N-ACTION"
	StepDeleteFilmSession = "Film Session N-DELETE"
)

// cleanupTimeout bounds the best-effort Film Session N-DELETE that PrintFilm
// sends after a failure, even when the caller's context is already done.
const cleanupTimeout = 5 * time.Second

// Film is one sheet for Client.PrintFilm: a STANDARD\Columns,Rows Film Box
// in its own Film Session.
type Film struct {
	Session      FilmSessionPresentation
	Columns      int
	Rows         int
	Presentation FilmBoxPresentation
	// Polarity is NORMAL, REVERSE or empty (printer default) for every box.
	Polarity string
	// Images fill the image boxes in row-major order. A nil entry, or a
	// position beyond len(Images), is not sent and prints with the Empty
	// Image Density. All images must match the client: grayscale for the
	// Basic Grayscale and RGB for the Basic Color Meta SOP Class.
	Images []*Image
	// ImagePresentations optionally correspond to Images in row-major order.
	ImagePresentations []ImageBoxPresentation
	// RequireImagePresentation stops before N-ACTION when a requested image
	// presentation was ignored by the peer (0001H, 0107H or 0116H).
	RequireImagePresentation bool
	// Native requests separately negotiated Presentation LUT and/or Annotation
	// objects. Unsupported optional-attribute warnings stop before N-ACTION.
	Native NativeFilmOptions
}

// StepStatus is a warning status returned by one step of a print session.
type StepStatus struct {
	Step     string
	Position int // image box position, for StepSetImageBox
	Status   uint16
}

// String names the step, the status and its PS3.4 Annex H / PS3.7 meaning.
func (s StepStatus) String() string {
	step := s.Step
	if s.Position > 0 {
		step = fmt.Sprintf("%s (position %d)", step, s.Position)
	}
	return fmt.Sprintf("%s: status %04XH (%s)", step, s.Status, StatusText(s.Status))
}

// FilmResult describes a PrintFilm session.
type FilmResult struct {
	FilmSessionUID string
	FilmBoxUID     string
	// ImageBoxes is the number of image boxes the printer created and
	// ImagesSet the number filled with an image.
	ImageBoxes int
	ImagesSet  int
	// Printed is true once the Film Box N-ACTION succeeded (possibly with a
	// warning); the film is then queued on the printer.
	Printed  bool
	Warnings []StepStatus
	// CleanupErr is the outcome of the final Film Session N-DELETE when it
	// failed; it does not undo a successful print.
	CleanupErr error
}

// PrintError reports the step of a print session that failed. Its message
// carries the step, the DIMSE status and the status meaning only: neither
// pixel data nor the SCP's free-text Error Comment, which applications must
// not assume to be free of identifying text. The underlying error, with any
// comment, stays available through Unwrap.
type PrintError struct {
	Step     string
	Position int // image box position, for StepSetImageBox
	// Status is the DIMSE status when the SCP answered; HasStatus is false
	// for transport, timeout, cancellation and local validation failures.
	Status    uint16
	HasStatus bool
	// OutcomeUnknown is set when the Film Box N-ACTION was sent but its
	// response was not received: the film may be printing, and PrintFilm
	// does not send it again.
	OutcomeUnknown bool
	Err            error
}

func (e *PrintError) Error() string {
	step := e.Step
	if e.Position > 0 {
		step = fmt.Sprintf("%s (position %d)", step, e.Position)
	}
	if e.HasStatus {
		return fmt.Sprintf("printmgmt: %s failed with status %04XH (%s)", step, e.Status, StatusText(e.Status))
	}
	if e.OutcomeUnknown {
		return fmt.Sprintf("printmgmt: %s response not received; the film may have been printed: %v", step, e.Err)
	}
	return fmt.Sprintf("printmgmt: %s failed: %v", step, e.Err)
}

func (e *PrintError) Unwrap() error { return e.Err }

// ErrInvalidFilm is returned, wrapped in a PrintError, for a Film refused
// before anything is sent.
var ErrInvalidFilm = errors.New("printmgmt: invalid film")

// StatusText is the meaning of a Print Management DIMSE status from the
// PS3.4 Annex H status tables and PS3.7 Annex C.
func StatusText(status uint16) string {
	switch status {
	case StatusSuccess:
		return "success"
	case 0x0001:
		return "requested optional attributes are not supported"
	case StatusAttributeListError:
		return "attribute list error: unsupported optional attributes were ignored"
	case 0x0116:
		return "attribute value out of range"
	case StatusMemoryAllocationNotSupport:
		return "memory allocation not supported"
	case StatusEmptySessionPage:
		return "film session printed with no image boxes filled"
	case StatusEmptyFilmBoxPage:
		return "film box printed with no image boxes filled"
	case 0xB604:
		return "image size larger than the image box; image demagnified"
	case 0xB605:
		return "requested Min or Max Density outside the printer's range"
	case 0xB609:
		return "image size larger than the image box; image cropped"
	case 0xB60A:
		return "image size or combined print image size larger than the image box; image decimated"
	case StatusNoFilmBoxInSession:
		return "film session has no film box"
	case StatusSessionQueueFull:
		return "unable to create a print job: print queue is full"
	case StatusFilmBoxQueueFull:
		return "unable to create a print job: print queue is full"
	case 0xC603:
		return "image size larger than the image box"
	case 0xC605:
		return "insufficient memory in the printer to store the image"
	case 0xC613:
		return "combined print image size larger than the image box"
	case 0xC616:
		return "more than one Film Box in the film session"
	case StatusNoSuchAttribute:
		return "no such attribute"
	case StatusInvalidAttributeValue:
		return "invalid attribute value"
	case StatusProcessingFailure:
		return "processing failure"
	case StatusNoSuchSOPInstance:
		return "no such SOP instance"
	case StatusMissingAttribute:
		return "missing attribute"
	case StatusMissingAttributeValue:
		return "missing attribute value"
	case 0x0111:
		return "duplicate SOP instance"
	case 0x0117:
		return "invalid object instance"
	case 0x0118:
		return "no such SOP class"
	case 0x0119:
		return "class-instance conflict"
	case 0x0122:
		return "SOP class not supported"
	case 0x0123:
		return "no such action"
	case 0x0124:
		return "not authorized"
	case 0x0210:
		return "duplicate invocation"
	case 0x0211:
		return "unrecognized operation"
	case 0x0212:
		return "mistyped argument"
	case StatusResourceLimitation:
		return "resource limitation"
	}
	if dimse.ClassifyNormalizedStatus(status) == dimse.NormalizedStatusWarning {
		return "warning"
	}
	return "failure"
}

// PrintFilm prints one film: Film Session N-CREATE, Film Box N-CREATE, one
// Image Box N-SET per image, Film Box N-ACTION, then Film Session N-DELETE.
// Warning statuses are collected in FilmResult.Warnings and the session
// continues; a failure stops it with a *PrintError and, once the Film
// Session exists, a best-effort N-DELETE. The N-ACTION is sent at most once.
// The association is borrowed: PrintFilm neither releases nor closes it.
func (c *Client) PrintFilm(ctx context.Context, film Film) (FilmResult, error) {
	var result FilmResult
	if err := c.validateFilm(film); err != nil {
		return result, err
	}
	if film.Native.Specified() {
		return c.printNativeFilm(ctx, film)
	}
	sessionUID, status, err := c.CreateFilmSession(ctx, "", film.Session)
	if stepErr := result.check(StepCreateFilmSession, 0, status, err); stepErr != nil {
		return result, stepErr
	}
	if sessionUID == "" {
		return result, &PrintError{Step: StepCreateFilmSession, Err: errors.New("the printer assigned no Film Session UID")}
	}
	result.FilmSessionUID = sessionUID
	fail := func(stepErr error) (FilmResult, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = c.DeleteFilmSession(cleanupCtx, sessionUID)
		return result, stepErr
	}

	filmBoxUID, boxes, status, err := c.CreateFilmBox(ctx, sessionUID, film.Columns, film.Rows, film.Presentation)
	if stepErr := result.check(StepCreateFilmBox, 0, status, err); stepErr != nil {
		return fail(stepErr)
	}
	if filmBoxUID == "" {
		return fail(&PrintError{Step: StepCreateFilmBox, Err: errors.New("the printer assigned no Film Box UID")})
	}
	result.FilmBoxUID = filmBoxUID
	result.ImageBoxes = len(boxes)
	for i, img := range film.Images {
		if img == nil {
			continue
		}
		presentation := ImageBoxPresentation{}
		if i < len(film.ImagePresentations) {
			presentation = film.ImagePresentations[i]
		}
		status, err := c.SetImageBoxWithPresentation(ctx, boxes[i], img, film.Polarity, presentation)
		if film.RequireImagePresentation && presentation.Specified() && (status.Code == 0x0001 || status.Code == StatusAttributeListError || status.Code == 0x0116) {
			return fail(&PrintError{Step: StepSetImageBox, Position: boxes[i].Position, Status: status.Code, HasStatus: true, Err: ErrPhysicalUnsupported})
		}
		if stepErr := result.check(StepSetImageBox, boxes[i].Position, status, err); stepErr != nil {
			return fail(stepErr)
		}
		result.ImagesSet++
	}

	status, err = c.PrintFilmBox(ctx, filmBoxUID)
	if stepErr := result.check(StepPrintFilmBox, 0, status, err); stepErr != nil {
		var printErr *PrintError
		if errors.As(stepErr, &printErr) && !printErr.HasStatus {
			printErr.OutcomeUnknown = true
		}
		return fail(stepErr)
	}
	result.Printed = true

	status, err = c.DeleteFilmSession(ctx, sessionUID)
	if stepErr := result.check(StepDeleteFilmSession, 0, status, err); stepErr != nil {
		result.CleanupErr = stepErr
	}
	return result, nil
}

func (c *Client) validateFilm(film Film) error {
	invalidFilm := func(reason string) error {
		return &PrintError{Step: StepCreateFilmBox, Err: fmt.Errorf("%w: %s", ErrInvalidFilm, reason)}
	}
	if !boxCountWithin(film.Columns, film.Rows, MaxImageBoxesCeiling) {
		return invalidFilm("columns and rows must be positive and within MaxImageBoxesCeiling image boxes")
	}
	if len(film.Images) > film.Columns*film.Rows {
		return invalidFilm(fmt.Sprintf("%d images do not fit %d image boxes", len(film.Images), film.Columns*film.Rows))
	}
	if film.Polarity != "" && film.Polarity != "NORMAL" && film.Polarity != "REVERSE" {
		return invalidFilm("polarity must be NORMAL or REVERSE")
	}
	if len(film.ImagePresentations) > len(film.Images) {
		return invalidFilm("more image presentations than images")
	}
	for i, p := range film.ImagePresentations {
		if err := p.Validate(); err != nil {
			return invalidFilm("invalid image presentation")
		}
		if film.Images[i] == nil && p.Specified() {
			return invalidFilm("presentation for an empty image")
		}
	}
	color := c.meta == ColorMetaSOPClassUID
	for i, img := range film.Images {
		if img == nil {
			continue
		}
		if img.Color != color {
			if color {
				return invalidFilm(fmt.Sprintf("image %d is grayscale on a color print session", i+1))
			}
			return invalidFilm(fmt.Sprintf("image %d is color on a grayscale print session", i+1))
		}
		if _, err := BuildImageBox(i+1, img, film.Polarity); err != nil {
			return invalidFilm(fmt.Sprintf("image %d does not meet the preformatted image constraints", i+1))
		}
	}
	return nil
}

// check classifies one step's outcome: success continues, a warning is
// recorded and continues, anything else is a *PrintError.
func (r *FilmResult) check(step string, position int, status Status, err error) error {
	if err == nil {
		return nil
	}
	var statusErr *dimse.NormalizedStatusError
	if errors.As(err, &statusErr) {
		if statusErr.Class == dimse.NormalizedStatusWarning {
			r.Warnings = append(r.Warnings, StepStatus{Step: step, Position: position, Status: statusErr.Status})
			return nil
		}
		return &PrintError{Step: step, Position: position, Status: statusErr.Status, HasStatus: true, Err: err}
	}
	if status.Code != StatusSuccess {
		return &PrintError{Step: step, Position: position, Status: status.Code, HasStatus: true, Err: err}
	}
	return &PrintError{Step: step, Position: position, Err: err}
}
