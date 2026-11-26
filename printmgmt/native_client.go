package printmgmt

import (
	"context"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	StepCreatePresentationLUT = "Presentation LUT N-CREATE"
	StepDeletePresentationLUT = "Presentation LUT N-DELETE"
	StepSetAnnotationBox      = "Annotation Box N-SET"
)

// NativePresentationContexts proposes optional classes independently of the
// Basic Print Meta SOP Class. Acceptance alone does not prove printer optics.
func NativePresentationContexts(color, lut, annotations bool) []ul.PresentationContext {
	out := PresentationContexts(color)
	for _, c := range []struct {
		enabled bool
		uid     string
	}{{lut, PresentationLUTSOPClassUID}, {annotations, AnnotationBoxSOPClassUID}} {
		if c.enabled {
			out = append(out, ul.PresentationContext{AbstractSyntaxUID: c.uid, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}})
		}
	}
	return out
}

func (c *Client) nativeOptions(ctx context.Context, class string) (dimse.NormalizedOperationOptions, error) {
	if !c.nativeContexts[class] {
		return dimse.NormalizedOperationOptions{}, ErrNativeUnsupported
	}
	p := c.options(ctx)
	p.PresentationContextAbstractSyntaxUID = class
	return p, nil
}

func (c *Client) CreatePresentationLUT(ctx context.Context, uid string, p PresentationLUT) (string, Status, error) {
	ds, err := BuildPresentationLUT(p)
	if err != nil {
		return "", Status{}, err
	}
	options, err := c.nativeOptions(ctx, PresentationLUTSOPClassUID)
	if err != nil {
		return "", Status{}, err
	}
	r, err := c.normalized.CreateWithOptions(options, dimse.NormalizedCreateRequest{AffectedSOPClassUID: PresentationLUTSOPClassUID, AffectedSOPInstanceUID: uid, CommandDataSetType: dimse.DataSetPresent}, ds)
	status := Status{}
	if r.Response != nil {
		uid = r.Response.AffectedSOPInstanceUID
		status = Status{r.Response.Status, r.Response.StatusFields}
	}
	return uid, status, err
}

func (c *Client) DeletePresentationLUT(ctx context.Context, uid string) (Status, error) {
	options, err := c.nativeOptions(ctx, PresentationLUTSOPClassUID)
	if err != nil {
		return Status{}, err
	}
	r, err := c.normalized.DeleteWithOptions(options, dimse.NormalizedDeleteRequest{RequestedSOPClassUID: PresentationLUTSOPClassUID, RequestedSOPInstanceUID: uid})
	if r.Response != nil {
		return Status{r.Response.Status, r.Response.StatusFields}, err
	}
	return Status{}, err
}

// CreateFilmBoxWithNative returns the separately owned Annotation Box objects.
// Reference positions are the client's ordinal UID-selection convention.
// N-SET assigns the requested slot; an SCP UID does not fix a physical slot.
func (c *Client) CreateFilmBoxWithNative(ctx context.Context, session string, columns, rows int, p FilmBoxPresentation) (string, FilmBoxReferences, Status, error) {
	if !boxCountWithin(columns, rows, MaxImageBoxesCeiling) {
		return "", FilmBoxReferences{}, Status{}, ErrInvalidFilm
	}
	if (p.PresentationLUTUID != "" && !c.nativeContexts[PresentationLUTSOPClassUID]) || (p.AnnotationDisplayFormatID != "" && !c.nativeContexts[AnnotationBoxSOPClassUID]) {
		return "", FilmBoxReferences{}, Status{}, ErrNativeUnsupported
	}
	r, err := c.normalized.CreateWithOptions(c.options(ctx), dimse.NormalizedCreateRequest{AffectedSOPClassUID: FilmBoxSOPClassUID, CommandDataSetType: dimse.DataSetPresent}, BuildFilmBox(session, columns, rows, p))
	status := Status{}
	uid := ""
	if r.Response != nil {
		uid = r.Response.AffectedSOPInstanceUID
		status = Status{r.Response.Status, r.Response.StatusFields}
	}
	if err != nil && !isWarning(err) {
		return uid, FilmBoxReferences{}, status, err
	}
	refs, parseErr := filmBoxReferences(r.DataSet, columns*rows, c.meta == ColorMetaSOPClassUID, c.nativeContexts[AnnotationBoxSOPClassUID],
		session, uid, p.PresentationLUTUID)
	if parseErr != nil {
		return uid, FilmBoxReferences{}, status, parseErr
	}
	return uid, refs, status, err
}

func filmBoxReferences(ds *object.Object, count int, color, annotations bool, assignedUIDs ...string) (FilmBoxReferences, error) {
	var out FilmBoxReferences
	if ds == nil {
		return out, errors.New("printmgmt: Film Box response omitted object references")
	}
	seen := map[string]bool{}
	for _, uid := range assignedUIDs {
		seen[core.NormalizeUID(uid)] = true
	}
	read := func(tag core.Tag, max int, class string) ([]string, error) {
		e, present := ds.Get(tag)
		seq, ok := e.Value.(core.SequenceValue)
		if !present || e.Header.VR != core.VRSQ || !ok || len(seq.Items) > max {
			return nil, invalid(tag)
		}
		items, _ := ds.GetSequence(tag)
		uids := make([]string, len(items))
		for i, item := range items {
			actual, err := nativeUID(item, TagReferencedSOPClassUID)
			if err != nil || actual != class {
				return nil, invalid(tag)
			}
			uid, err := nativeUID(item, TagReferencedSOPInstanceUID)
			if err != nil || seen[uid] {
				return nil, invalid(tag)
			}
			seen[uid] = true
			uids[i] = uid
		}
		return uids, nil
	}
	class := GrayscaleImageBoxUID
	if color {
		class = ColorImageBoxUID
	}
	images, err := read(TagReferencedImageBoxSequence, count, class)
	if err != nil || len(images) != count {
		return out, invalid(TagReferencedImageBoxSequence)
	}
	for i, uid := range images {
		out.ImageBoxes = append(out.ImageBoxes, ImageBoxReference{SOPClassUID: class, SOPInstanceUID: uid, Position: i + 1})
	}
	if annotations {
		uids, err := read(TagReferencedAnnotationBoxSequence, MaxNativeAnnotationBoxes, AnnotationBoxSOPClassUID)
		if err != nil {
			return out, err
		}
		for i, uid := range uids {
			out.AnnotationBoxes = append(out.AnnotationBoxes, AnnotationBoxReference{SOPInstanceUID: uid, Position: i + 1})
		}
	}
	return out, nil
}

func (c *Client) SetAnnotationBox(ctx context.Context, box AnnotationBoxReference, text, charset string) (Status, error) {
	ds, err := BuildAnnotationBox(box.Position, text, charset)
	if err != nil {
		return Status{}, err
	}
	options, err := c.nativeOptions(ctx, AnnotationBoxSOPClassUID)
	if err != nil {
		return Status{}, err
	}
	r, err := c.normalized.SetWithOptions(options, dimse.NormalizedSetRequest{RequestedSOPClassUID: AnnotationBoxSOPClassUID, RequestedSOPInstanceUID: box.SOPInstanceUID}, ds)
	if r.Response != nil {
		return Status{r.Response.Status, r.Response.StatusFields}, err
	}
	return Status{}, err
}

func (c *Client) SetImageBoxWithLUT(ctx context.Context, box ImageBoxReference, img *Image, polarity string, p ImageBoxPresentation, lutUID string) (Status, error) {
	if lutUID != "" && !c.nativeContexts[PresentationLUTSOPClassUID] {
		return Status{}, ErrNativeUnsupported
	}
	ds, err := BuildImageBoxWithPresentation(box.Position, img, polarity, p)
	if err != nil {
		return Status{}, err
	}
	if lutUID != "" {
		if !core.IsValidUID(lutUID) {
			return Status{}, invalid(TagReferencedPresentationLUT)
		}
		ds.Put(presentationLUTReference(lutUID))
	}
	return c.set(ctx, box.SOPClassUID, box.SOPInstanceUID, ds)
}

func nativeIgnored(status Status) bool {
	return status.Code == 0x0001 || status.Code == StatusAttributeListError || status.Code == 0x0116
}

func (c *Client) validateNativeFilm(film Film) error {
	if !c.nativeContexts[c.meta] {
		return ErrNativeUnsupported
	}
	n := film.Native
	if n.PresentationLUT != nil {
		if !c.nativeContexts[PresentationLUTSOPClassUID] || c.meta == ColorMetaSOPClassUID {
			return ErrNativeUnsupported
		}
		if err := n.PresentationLUT.Validate(); err != nil {
			return err
		}
		if _, err := film.Presentation.NativePrintDensity(); err != nil {
			return err
		}
		for _, img := range film.Images {
			if err := n.PresentationLUT.validateImage(img); err != nil {
				return err
			}
		}
	}
	if n.AnnotationDisplayFormatID != "" || len(n.Annotations) > 0 {
		if !c.nativeContexts[AnnotationBoxSOPClassUID] {
			return ErrNativeUnsupported
		}
		if n.AnnotationDisplayFormatID == "" || !validNativeCS(n.AnnotationDisplayFormatID) || len(n.Annotations) > MaxNativeAnnotationBoxes {
			return invalid(TagAnnotationDisplayFormatID)
		}
		if !supportedAnnotationCharset(film.Session.SpecificCharacterSet) {
			return invalid(TagSpecificCharacterSet)
		}
		seen := map[int]bool{}
		for _, a := range n.Annotations {
			if seen[a.Position] {
				return invalid(TagAnnotationPosition)
			}
			seen[a.Position] = true
			if _, err := BuildAnnotationBox(a.Position, a.Text, film.Session.SpecificCharacterSet); err != nil {
				return err
			}
		}
	}
	if film.Presentation.PresentationLUTUID != "" || film.Presentation.AnnotationDisplayFormatID != "" {
		return fmt.Errorf("%w: native object references are owned by PrintFilm", ErrInvalidFilm)
	}
	return nil
}

func (c *Client) printNativeFilm(ctx context.Context, film Film) (result FilmResult, retErr error) {
	if err := c.validateNativeFilm(film); err != nil {
		return result, &PrintError{Step: StepCreateFilmBox, Err: err}
	}
	var lutUID, sessionUID string
	cleanup := func(ctx context.Context) {
		if sessionUID != "" {
			status, err := c.DeleteFilmSession(ctx, sessionUID)
			if stepErr := result.check(StepDeleteFilmSession, 0, status, err); stepErr != nil {
				result.CleanupErr = errors.Join(result.CleanupErr, stepErr)
			}
		}
		if lutUID != "" {
			status, err := c.DeletePresentationLUT(ctx, lutUID)
			if stepErr := result.check(StepDeletePresentationLUT, 0, status, err); stepErr != nil {
				result.CleanupErr = errors.Join(result.CleanupErr, stepErr)
			}
		}
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		cleanup(cleanupCtx)
	}()
	check := func(step string, pos int, status Status, err error) error {
		if nativeIgnored(status) {
			return &PrintError{Step: step, Position: pos, Status: status.Code, HasStatus: true, Err: ErrNativeUnsupported}
		}
		return result.check(step, pos, status, err)
	}
	if film.Native.PresentationLUT != nil {
		uid, status, err := c.CreatePresentationLUT(ctx, "", *film.Native.PresentationLUT)
		lutUID = uid
		if stepErr := check(StepCreatePresentationLUT, 0, status, err); stepErr != nil {
			return result, stepErr
		}
		if !core.IsValidUID(uid) {
			return result, &PrintError{Step: StepCreatePresentationLUT, Err: invalid(TagReferencedSOPInstanceUID)}
		}
		film.Presentation.PresentationLUTUID = uid
	}
	uid, status, err := c.CreateFilmSession(ctx, "", film.Session)
	sessionUID = uid
	result.FilmSessionUID = uid
	if stepErr := check(StepCreateFilmSession, 0, status, err); stepErr != nil {
		return result, stepErr
	}
	if !core.IsValidUID(uid) {
		return result, &PrintError{Step: StepCreateFilmSession, Err: invalid(TagReferencedSOPInstanceUID)}
	}
	film.Presentation.AnnotationDisplayFormatID = film.Native.AnnotationDisplayFormatID
	boxUID, refs, status, err := c.CreateFilmBoxWithNative(ctx, uid, film.Columns, film.Rows, film.Presentation)
	if stepErr := check(StepCreateFilmBox, 0, status, err); stepErr != nil {
		return result, stepErr
	}
	if !core.IsValidUID(boxUID) {
		return result, &PrintError{Step: StepCreateFilmBox, Err: invalid(TagReferencedSOPInstanceUID)}
	}
	result.FilmBoxUID = boxUID
	result.ImageBoxes = len(refs.ImageBoxes)
	for _, a := range film.Native.Annotations {
		if a.Position > len(refs.AnnotationBoxes) {
			return result, &PrintError{Step: StepSetAnnotationBox, Position: a.Position, Err: invalid(TagAnnotationPosition)}
		}
		status, err := c.SetAnnotationBox(ctx, refs.AnnotationBoxes[a.Position-1], a.Text, film.Session.SpecificCharacterSet)
		if stepErr := check(StepSetAnnotationBox, a.Position, status, err); stepErr != nil {
			return result, stepErr
		}
	}
	for i, img := range film.Images {
		if img == nil {
			continue
		}
		p := ImageBoxPresentation{}
		if i < len(film.ImagePresentations) {
			p = film.ImagePresentations[i]
		}
		status, err := c.SetImageBoxWithPresentation(ctx, refs.ImageBoxes[i], img, film.Polarity, p)
		if stepErr := check(StepSetImageBox, i+1, status, err); stepErr != nil {
			return result, stepErr
		}
		result.ImagesSet++
	}
	status, err = c.PrintFilmBox(ctx, boxUID)
	if stepErr := result.check(StepPrintFilmBox, 0, status, err); stepErr != nil {
		var p *PrintError
		if errors.As(stepErr, &p) && !p.HasStatus {
			p.OutcomeUnknown = true
		}
		return result, stepErr
	}
	result.Printed = true
	return result, nil
}
