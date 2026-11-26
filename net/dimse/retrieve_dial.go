package dimse

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// RetrieveDialOptions describes how to establish an association suitable for
// scoped Query/Retrieve (C-MOVE or C-GET) operations.
//
// This is a convenience wrapper around ul.DialContext.
//
// Notes:
//   - Timeouts should be enforced by the provided context.
//   - Only one operation at a time is supported on a single association.
//   - Presentation contexts must include the appropriate Query/Retrieve SOP
//     Class UID(s).
type RetrieveDialOptions struct {
	Transport ul.TransportOptions
	Address   string

	CalledAETitle  string
	CallingAETitle string

	MaxPDU uint32

	// Contexts are the proposed presentation contexts.
	Contexts []ul.PresentationContext
	// RoleSelections proposes alternate SCU/SCP roles. C-GET callers use this
	// to accept Storage sub-operations as an SCP on the same association.
	RoleSelections []ul.RoleSelectionItem

	// Implementation identifiers are passed through to UL negotiation.
	ImplementationClassUID    string
	ImplementationVersionName string

	// TLSConfig secures the connection, for example with ul.ClientTLSConfig.
	TLSConfig *tls.Config
	// UserIdentity, when set, proposes User Identity negotiation (PS3.7
	// D.3.3.7). Its secondary field is a passcode and is never formatted
	// into errors.
	UserIdentity *ul.UserIdentityRequest
}

// DialRetrieveSCU establishes a UL association using the supplied options.
func DialRetrieveSCU(ctx context.Context, opts RetrieveDialOptions) (*ul.Association, error) {
	address := strings.TrimSpace(opts.Address)
	if address == "" {
		return nil, fmt.Errorf("dicom dimse: missing address")
	}
	if len(opts.Contexts) == 0 {
		return nil, fmt.Errorf("dicom dimse: no presentation contexts provided")
	}
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		Transport:                 opts.Transport,
		CalledAETitle:             opts.CalledAETitle,
		CallingAETitle:            opts.CallingAETitle,
		MaxPDU:                    opts.MaxPDU,
		Contexts:                  opts.Contexts,
		RoleSelections:            opts.RoleSelections,
		ImplementationClassUID:    opts.ImplementationClassUID,
		ImplementationVersionName: opts.ImplementationVersionName,
		TLSConfig:                 opts.TLSConfig,
		UserIdentity:              opts.UserIdentity,
	})
	if err != nil {
		return nil, fmt.Errorf("dicom dimse: dial retrieve SCU address=%q calledAET=%q callingAET=%q maxPDU=%d contexts=%d: %w", address, opts.CalledAETitle, opts.CallingAETitle, opts.MaxPDU, len(opts.Contexts), err)
	}
	return assoc, nil
}
