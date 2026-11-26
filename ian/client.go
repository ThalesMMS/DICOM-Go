package ian

import (
	"context"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PresentationContext proposes the Instance Availability Notification SOP
// Class with Explicit and Implicit VR Little Endian.
func PresentationContext() ul.PresentationContext {
	return ul.PresentationContext{AbstractSyntaxUID: SOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}}
}

// Result is the outcome of one N-CREATE. StatusFields holds the Error
// Comment, Error ID and Attribute Identifier List of a failure, if any.
type Result struct {
	Status         uint16
	SOPInstanceUID string
	StatusFields   *dimse.NormalizedStatusFields
}

// Client sends Instance Availability Notification N-CREATE requests on a
// borrowed association. A failure status is returned as a
// *dimse.NormalizedStatusError (Class failure or warning) together with the
// Result. A transport error leaves the outcome unknown and the client never
// repeats the request; the SCP's ServiceOptions.AcceptIdenticalReplay lets a
// caller resend the same notification UID safely.
type Client struct {
	normalized *dimse.NormalizedClient
}

// NewClient borrows assoc; it is neither released nor closed.
func NewClient(assoc *ul.Association) *Client {
	return &Client{normalized: dimse.NewNormalizedClient(assoc)}
}

// Notify validates attributes and sends them in one N-CREATE. An empty
// sopInstanceUID lets the SCP assign the notification UID, which the Result
// carries.
func (c *Client) Notify(ctx context.Context, sopInstanceUID string, attributes *object.Object) (Result, error) {
	if err := Validate(attributes); err != nil {
		return Result{}, err
	}
	exchange, err := c.normalized.CreateWithOptions(dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}}, dimse.NormalizedCreateRequest{
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

// NotifyNotification builds the Attribute List of n and sends it.
func (c *Client) NotifyNotification(ctx context.Context, sopInstanceUID string, n Notification) (Result, error) {
	attributes, err := Build(n)
	if err != nil {
		return Result{}, err
	}
	return c.Notify(ctx, sopInstanceUID, attributes)
}
