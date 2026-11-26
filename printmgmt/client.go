package printmgmt

import (
	"context"
	"errors"
	"image"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PresentationContexts proposes the grayscale or color Meta SOP Class with
// Explicit and Implicit VR Little Endian. Commands for the Film Session, Film
// Box, Image Box and Printer SOP Classes travel on that context.
func PresentationContexts(color bool) []ul.PresentationContext {
	meta := GrayscaleMetaSOPClassUID
	if color {
		meta = ColorMetaSOPClassUID
	}
	return []ul.PresentationContext{{AbstractSyntaxUID: meta, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}}}
}

// ImageBoxReference is one image box created by a Film Box N-CREATE.
type ImageBoxReference struct {
	SOPClassUID    string
	SOPInstanceUID string
	Position       int
}

// Client drives a print session on a borrowed association negotiated with
// PresentationContexts. It sends each operation once: an N-ACTION whose
// outcome is unknown (transport error) is never repeated, because the film
// may already be printing.
type Client struct {
	normalized     *dimse.NormalizedClient
	meta           string
	events         *dimse.NormalizedEventReportOptions
	nativeContexts map[string]bool
}

// PrinterEventReportOptions opts into Printer status notifications received
// during a confirmed operation. Only Printer SOP Class events 1, 2 and 3 are
// accepted, on the negotiated Meta or Printer context. A nil Handler explicitly
// refuses them. The handler must observe its context and must not read the
// association or initiate another operation. This is not a print completion
// monitor: no reader runs between operations and Print Job remains unsupported.
type PrinterEventReportOptions struct {
	Handler         dimse.NormalizedEventReportHandler
	MaxEvents       int
	MaxDataSetBytes int64
}

type ClientOptions struct {
	// PrinterEvents is nil by default, preserving unexpected-command cleanup.
	PrinterEvents *PrinterEventReportOptions
}

// NewClient borrows assoc; it is neither released nor closed.
func NewClient(assoc *ul.Association, color bool) *Client {
	return NewClientWithOptions(assoc, color, ClientOptions{})
}

// NewClientWithOptions borrows assoc and explicitly configures notifications.
func NewClientWithOptions(assoc *ul.Association, color bool, options ClientOptions) *Client {
	meta := GrayscaleMetaSOPClassUID
	if color {
		meta = ColorMetaSOPClassUID
	}
	c := &Client{normalized: dimse.NewNormalizedClient(assoc), meta: meta, nativeContexts: make(map[string]bool)}
	if assoc != nil {
		for _, pc := range assoc.AcceptedContexts {
			c.nativeContexts[pc.AbstractSyntaxUID] = true
		}
	}
	if options.PrinterEvents != nil {
		configured := *options.PrinterEvents
		c.events = &dimse.NormalizedEventReportOptions{
			Handler: configured.Handler, MaxEvents: configured.MaxEvents, MaxDataSetBytes: configured.MaxDataSetBytes,
			Subscriptions: []dimse.NormalizedEventReportSubscription{{SOPClassUID: PrinterSOPClassUID, EventTypeIDs: []uint16{1, 2, 3}}},
			PresentationContextPolicy: func(pc ul.AcceptedContext, class string) error {
				if class == PrinterSOPClassUID && (pc.AbstractSyntaxUID == meta || pc.AbstractSyntaxUID == PrinterSOPClassUID) {
					return nil
				}
				return &dimse.NormalizedPresentationContextError{PresentationContextID: pc.ID, AbstractSyntaxUID: pc.AbstractSyntaxUID, CommandSOPClassUID: class}
			},
		}
		if configured.Handler != nil {
			c.events.Handler = func(ctx context.Context, req dimse.NormalizedEventReportRequest, dataSet *object.Object) (dimse.NormalizedEventReportSCPResult, error) {
				if req.AffectedSOPInstanceUID != PrinterSOPInstanceUID {
					return dimse.NormalizedEventReportSCPResult{Response: dimse.NormalizedEventReportResponse{Status: dimse.StatusNoSuchSOPInstance}}, nil
				}
				return configured.Handler(ctx, req, dataSet)
			}
		}
	}
	return c
}

func (c *Client) options(ctx context.Context) dimse.NormalizedOperationOptions {
	return dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}, PresentationContextAbstractSyntaxUID: c.meta, EventReports: c.events}
}

// Status is the status of the last response, including warnings.
type Status struct {
	Code   uint16
	Fields *dimse.NormalizedStatusFields
}

// CreateFilmSession creates the association's Film Session; an empty uid
// lets the SCP assign it.
func (c *Client) CreateFilmSession(ctx context.Context, uid string, p FilmSessionPresentation) (string, Status, error) {
	result, err := c.normalized.CreateWithOptions(c.options(ctx), dimse.NormalizedCreateRequest{AffectedSOPClassUID: FilmSessionSOPClassUID, AffectedSOPInstanceUID: uid, CommandDataSetType: dimse.DataSetPresent}, BuildFilmSession(p))
	status := Status{}
	if result.Response != nil {
		status = Status{result.Response.Status, result.Response.StatusFields}
		if result.Response.AffectedSOPInstanceUID != "" {
			uid = result.Response.AffectedSOPInstanceUID
		}
	}
	return uid, status, err
}

// CreateFilmBox creates a STANDARD\columns,rows Film Box and returns its UID
// and the validated image boxes the SCP created, by response position.
func (c *Client) CreateFilmBox(ctx context.Context, filmSessionUID string, columns, rows int, p FilmBoxPresentation) (string, []ImageBoxReference, Status, error) {
	if !boxCountWithin(columns, rows, MaxImageBoxesCeiling) {
		return "", nil, Status{}, errors.New("printmgmt: Film Box columns and rows must be positive and within MaxImageBoxesCeiling image boxes")
	}
	result, err := c.normalized.CreateWithOptions(c.options(ctx), dimse.NormalizedCreateRequest{AffectedSOPClassUID: FilmBoxSOPClassUID, CommandDataSetType: dimse.DataSetPresent}, BuildFilmBox(filmSessionUID, columns, rows, p))
	status := Status{}
	uid := ""
	if result.Response != nil {
		status = Status{result.Response.Status, result.Response.StatusFields}
		uid = result.Response.AffectedSOPInstanceUID
	}
	// A warning (for example 0107H, optional attributes ignored) still
	// creates the Film Box and its image boxes; only failures stop here.
	if err != nil && !isWarning(err) {
		return uid, nil, status, err
	}
	refs, parseErr := filmBoxReferences(result.DataSet, columns*rows, c.meta == ColorMetaSOPClassUID, false,
		filmSessionUID, uid, p.PresentationLUTUID)
	if parseErr != nil {
		return uid, nil, status, parseErr
	}
	return uid, refs.ImageBoxes, status, err
}

// isWarning reports a DIMSE warning status, which the normalized client
// returns as an error although the operation was performed.
func isWarning(err error) bool {
	var statusErr *dimse.NormalizedStatusError
	return errors.As(err, &statusErr) && statusErr.Class == dimse.NormalizedStatusWarning
}

// SetImageBox places image (nil erases the position) in an image box.
func (c *Client) SetImageBox(ctx context.Context, box ImageBoxReference, img *Image, polarity string) (Status, error) {
	modifications, err := BuildImageBox(box.Position, img, polarity)
	if err != nil {
		return Status{}, err
	}
	return c.set(ctx, box.SOPClassUID, box.SOPInstanceUID, modifications)
}

// SetImageBoxWithPresentation requests explicit per-image physical presentation.
func (c *Client) SetImageBoxWithPresentation(ctx context.Context, box ImageBoxReference, img *Image, polarity string, p ImageBoxPresentation) (Status, error) {
	ds, err := BuildImageBoxWithPresentation(box.Position, img, polarity, p)
	if err != nil {
		return Status{}, err
	}
	return c.set(ctx, box.SOPClassUID, box.SOPInstanceUID, ds)
}

// SetFilmBox updates the last created Film Box.
func (c *Client) SetFilmBox(ctx context.Context, uid string, modifications *object.Object) (Status, error) {
	return c.set(ctx, FilmBoxSOPClassUID, uid, modifications)
}

func (c *Client) set(ctx context.Context, class, uid string, modifications *object.Object) (Status, error) {
	result, err := c.normalized.SetWithOptions(c.options(ctx), dimse.NormalizedSetRequest{RequestedSOPClassUID: class, RequestedSOPInstanceUID: uid}, modifications)
	if result.Response != nil {
		return Status{result.Response.Status, result.Response.StatusFields}, err
	}
	return Status{}, err
}

// PrintFilmBox prints the last created Film Box.
func (c *Client) PrintFilmBox(ctx context.Context, uid string) (Status, error) {
	return c.print(ctx, FilmBoxSOPClassUID, uid)
}

// PrintFilmSession prints every Film Box of the session, collated.
func (c *Client) PrintFilmSession(ctx context.Context, uid string) (Status, error) {
	return c.print(ctx, FilmSessionSOPClassUID, uid)
}

func (c *Client) print(ctx context.Context, class, uid string) (Status, error) {
	result, err := c.normalized.ActionWithOptions(c.options(ctx), dimse.NormalizedActionRequest{RequestedSOPClassUID: class, RequestedSOPInstanceUID: uid, ActionTypeID: PrintActionTypeID, CommandDataSetType: dimse.NoDataSet}, nil)
	if result.Response != nil {
		return Status{result.Response.Status, result.Response.StatusFields}, err
	}
	return Status{}, err
}

// DeleteFilmSession deletes the whole Film Session hierarchy.
func (c *Client) DeleteFilmSession(ctx context.Context, uid string) (Status, error) {
	return c.delete(ctx, FilmSessionSOPClassUID, uid)
}

// DeleteFilmBox deletes the last created Film Box.
func (c *Client) DeleteFilmBox(ctx context.Context, uid string) (Status, error) {
	return c.delete(ctx, FilmBoxSOPClassUID, uid)
}

func (c *Client) delete(ctx context.Context, class, uid string) (Status, error) {
	result, err := c.normalized.DeleteWithOptions(c.options(ctx), dimse.NormalizedDeleteRequest{RequestedSOPClassUID: class, RequestedSOPInstanceUID: uid})
	if result.Response != nil {
		return Status{result.Response.Status, result.Response.StatusFields}, err
	}
	return Status{}, err
}

// PrinterStatus reads the Printer SOP Instance with N-GET.
func (c *Client) PrinterStatus(ctx context.Context) (PrinterStatus, error) {
	result, err := c.normalized.GetWithOptions(c.options(ctx), dimse.NormalizedGetRequest{RequestedSOPClassUID: PrinterSOPClassUID, RequestedSOPInstanceUID: PrinterSOPInstanceUID})
	if err != nil || result.DataSet == nil {
		return PrinterStatus{}, err
	}
	status := PrinterStatus{}
	status.Status, _ = result.DataSet.GetString(TagPrinterStatus)
	status.Info, _ = result.DataSet.GetString(TagPrinterStatusInfo)
	status.Name, _ = result.DataSet.GetString(TagPrinterName)
	status.Manufacturer, _ = result.DataSet.GetString(TagManufacturer)
	status.ManufacturerModelName, _ = result.DataSet.GetString(TagManufacturerModelName)
	return status, nil
}

// ImageFromGray converts an 8-bit grayscale raster, for example one rendered
// with pixeldata/display under the caller's own modality/VOI choices, into a
// MONOCHROME2 print image. No windowing or scaling is applied here.
func ImageFromGray(gray *image.Gray) *Image {
	b := gray.Bounds()
	out := &Image{Rows: b.Dy(), Columns: b.Dx(), Bits: 8, Photometric: "MONOCHROME2", PixelData: make([]byte, 0, b.Dx()*b.Dy())}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		out.PixelData = append(out.PixelData, gray.Pix[(y-b.Min.Y)*gray.Stride:(y-b.Min.Y)*gray.Stride+b.Dx()]...)
	}
	return out
}

// ImageFromRGBA converts an RGBA raster into an RGB color-by-plane print
// image. Alpha is dropped, not composited: pass an opaque image.
func ImageFromRGBA(rgba *image.RGBA) *Image {
	b := rgba.Bounds()
	n := b.Dx() * b.Dy()
	out := &Image{Color: true, Rows: b.Dy(), Columns: b.Dx(), Bits: 8, Photometric: "RGB", PixelData: make([]byte, 3*n)}
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		row := rgba.Pix[(y-b.Min.Y)*rgba.Stride:]
		for x := 0; x < b.Dx(); x++ {
			out.PixelData[i] = row[4*x]
			out.PixelData[n+i] = row[4*x+1]
			out.PixelData[2*n+i] = row[4*x+2]
			i++
		}
	}
	return out
}
