// Package netconf provides a bounded declarative network catalog. It describes
// capabilities; applications still decide which DIMSE services they implement.
package netconf

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

var ErrInvalid = errors.New("dicom netconf: invalid configuration")
var ErrResourceLimit = errors.New("dicom netconf: resource limit")

// ConfigurationError identifies a fixed logical field, never its value or an
// input filename. Unwrap preserves the invalid/limit sentinel for errors.Is.
type ConfigurationError struct {
	field string
	cause error
}

func (e *ConfigurationError) LogicalField() string { return e.field }
func (e *ConfigurationError) Unwrap() error        { return e.cause }
func (e *ConfigurationError) Error() string {
	if errors.Is(e.cause, ErrResourceLimit) {
		return ErrResourceLimit.Error() + ": " + e.field
	}
	return ErrInvalid.Error() + ": " + e.field
}
func fieldError(field string, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrResourceLimit) {
		return err
	}
	var existing *ConfigurationError
	if errors.As(err, &existing) {
		return err
	}
	return &ConfigurationError{field: field, cause: err}
}

type Role string

const (
	RoleSCU Role = "SCU"
	RoleSCP Role = "SCP"
)

type Device struct {
	SchemaVersion       int                 `json:"schemaVersion"`
	DeviceName          string              `json:"deviceName"`
	Description         string              `json:"description,omitempty"`
	Installed           *bool               `json:"installed,omitempty"`
	Connections         []Connection        `json:"connections"`
	ApplicationEntities []ApplicationEntity `json:"applicationEntities"`
}
type Connection struct {
	ID          string `json:"id"`
	Hostname    string `json:"hostname"`
	Port        int    `json:"port"`
	Installed   *bool  `json:"installed,omitempty"`
	TLSRequired bool   `json:"tlsRequired"`
}
type ApplicationEntity struct {
	AETitle              string               `json:"aeTitle"`
	ConnectionIDs        []string             `json:"connectionIDs"`
	AssociationInitiator bool                 `json:"associationInitiator"`
	AssociationAcceptor  bool                 `json:"associationAcceptor"`
	Installed            *bool                `json:"installed,omitempty"`
	TransferCapabilities []TransferCapability `json:"transferCapabilities"`
}
type TransferCapability struct {
	SOPClassUID        string   `json:"sopClassUID"`
	Role               Role     `json:"role"`
	TransferSyntaxUIDs []string `json:"transferSyntaxUIDs"`
}

func active(v *bool, inherited bool) bool {
	if v == nil {
		return inherited
	}
	return *v
}
func textOK(s string, max int, empty bool) bool {
	if (!empty && s == "") || len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func hostOK(s string, max int) bool {
	if !textOK(s, max, false) {
		return false
	}
	for _, r := range s {
		if r > 127 || r == ' ' || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

// Validate checks the caller-owned model without changing it. All logical
// backing is admitted before auxiliary maps or detached catalogs are created.
func Validate(ctx context.Context, d *Device, options Options) error {
	o, err := normalized(options)
	if err != nil {
		return fieldError("limits", err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if d == nil {
		return fieldError("device", ErrInvalid)
	}
	if err = modelFootprint(ctx, d, o.Limits); err != nil {
		return fieldError("limits.model", err)
	}
	if d.SchemaVersion != 1 || !textOK(d.DeviceName, o.Limits.MaxNameBytes, false) || !textOK(d.Description, o.Limits.MaxDescriptionBytes, true) || len(d.Connections) == 0 || len(d.ApplicationEntities) == 0 {
		return fieldError("device", ErrInvalid)
	}
	connections := make(map[string]bool, len(d.Connections))
	for _, c := range d.Connections {
		if err = ctx.Err(); err != nil {
			return err
		}
		if !textOK(c.ID, o.Limits.MaxNameBytes, false) || strings.TrimSpace(c.ID) != c.ID || !hostOK(c.Hostname, o.Limits.MaxHostnameBytes) || c.Port < 0 || c.Port > 65535 || connections[c.ID] {
			return fieldError("connections[]", ErrInvalid)
		}
		connections[c.ID] = true
	}
	aes := make(map[string]bool, len(d.ApplicationEntities))
	for _, ae := range d.ApplicationEntities {
		if err = ctx.Err(); err != nil {
			return err
		}
		title := ul.NormalizeAETitle(ae.AETitle)
		if ul.ValidateAETitle(title) != nil || aes[title] || len(ae.ConnectionIDs) == 0 {
			return fieldError("applicationEntities[]", ErrInvalid)
		}
		aes[title] = true
		refs := make(map[string]bool, len(ae.ConnectionIDs))
		for _, id := range ae.ConnectionIDs {
			if !connections[id] || refs[id] {
				return fieldError("applicationEntities[].connectionIDs[]", ErrInvalid)
			}
			refs[id] = true
		}
		pairs := make(map[string]uint8, len(ae.TransferCapabilities))
		for _, tc := range ae.TransferCapabilities {
			var bit uint8
			switch tc.Role {
			case RoleSCU:
				bit = 1
			case RoleSCP:
				bit = 2
			default:
				return fieldError("applicationEntities[].transferCapabilities[].role", ErrInvalid)
			}
			if !core.IsValidUID(tc.SOPClassUID) || len(tc.TransferSyntaxUIDs) == 0 || pairs[tc.SOPClassUID]&bit != 0 {
				return fieldError("applicationEntities[].transferCapabilities[]", ErrInvalid)
			}
			pairs[tc.SOPClassUID] |= bit
			seen := make(map[string]bool, len(tc.TransferSyntaxUIDs))
			for _, ts := range tc.TransferSyntaxUIDs {
				if !core.IsValidUID(ts) || seen[ts] {
					return fieldError("applicationEntities[].transferCapabilities[].transferSyntaxUIDs[]", ErrInvalid)
				}
				seen[ts] = true
			}
		}
	}
	return nil
}
