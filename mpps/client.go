package mpps

import (
	"context"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PresentationContext proposes the MPPS SOP Class with Explicit and Implicit
// VR Little Endian.
func PresentationContext() ul.PresentationContext {
	return ul.PresentationContext{AbstractSyntaxUID: SOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}}
}

// Result is the outcome of one exchange. StatusFields holds the Error
// Comment, Error ID and Attribute Identifier List of a failure, if any.
type Result struct {
	Status         uint16
	SOPInstanceUID string
	StatusFields   *dimse.NormalizedStatusFields
}

// Client sends MPPS N-CREATE and N-SET on a borrowed association. A failure
// status is returned as a *dimse.NormalizedStatusError together with the
// Result. A transport error leaves the outcome unknown: the client never
// repeats an operation, because an unobserved N-CREATE or N-SET may have
// been applied. Use ServiceOptions.AcceptIdenticalCreateReplay on the SCP, or
// an application reconciliation, to retry safely.
type Client struct {
	normalized *dimse.NormalizedClient
}

// NewClient borrows assoc; it is neither released nor closed.
func NewClient(assoc *ul.Association) *Client {
	return &Client{normalized: dimse.NewNormalizedClient(assoc)}
}

func (c *Client) options(ctx context.Context) dimse.NormalizedOperationOptions {
	return dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}}
}

// Create sends N-CREATE with Performed Procedure Step Status IN PROGRESS. An
// empty sopInstanceUID lets the SCP assign one, which the Result carries.
func (c *Client) Create(ctx context.Context, sopInstanceUID string, attributes *object.Object) (Result, error) {
	if err := ValidateCreate(attributes); err != nil {
		return Result{}, err
	}
	exchange, err := c.normalized.CreateWithOptions(c.options(ctx), dimse.NormalizedCreateRequest{
		AffectedSOPClassUID: SOPClassUID, AffectedSOPInstanceUID: sopInstanceUID, CommandDataSetType: dimse.DataSetPresent,
	}, attributes)
	result := Result{SOPInstanceUID: sopInstanceUID}
	if exchange.Response != nil {
		result.Status = exchange.Response.Status
		result.StatusFields = exchange.Response.StatusFields
		if exchange.Response.AffectedSOPInstanceUID != "" {
			result.SOPInstanceUID = exchange.Response.AffectedSOPInstanceUID
		}
	}
	return result, err
}

// Set sends N-SET with a Modification List from BuildSet, Complete or
// Discontinue.
func (c *Client) Set(ctx context.Context, sopInstanceUID string, modifications *object.Object) (Result, error) {
	if err := validateModificationShape(modifications); err != nil {
		return Result{}, err
	}
	exchange, err := c.normalized.SetWithOptions(c.options(ctx), dimse.NormalizedSetRequest{
		RequestedSOPClassUID: SOPClassUID, RequestedSOPInstanceUID: sopInstanceUID,
	}, modifications)
	result := Result{SOPInstanceUID: sopInstanceUID}
	if exchange.Response != nil {
		result.Status = exchange.Response.Status
		result.StatusFields = exchange.Response.StatusFields
	}
	return result, err
}
