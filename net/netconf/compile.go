package netconf

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

type CompiledConnection struct {
	address     string
	tlsRequired bool
	registry    *ul.CalledAERegistry
}

func (c *CompiledConnection) Address() string {
	if c == nil {
		return ""
	}
	return c.address
}
func (c *CompiledConnection) TLSRequired() bool { return c != nil && c.tlsRequired }
func (c *CompiledConnection) Registry() *ul.CalledAERegistry {
	if c == nil {
		return nil
	}
	return c.registry
}
func (c *CompiledConnection) AETitles() []string {
	if c == nil {
		return nil
	}
	return c.registry.AETitles()
}
func (c *CompiledConnection) Profiles() []ul.CalledAEProfile {
	if c == nil {
		return nil
	}
	return c.registry.Profiles()
}

// CompileConnection freezes one installed connection and its installed acceptor
// AEs. Empty ID auto-selects only when the Device declares exactly one connection.
// Different TS preference lists for the same SOP in dual roles are refused.
func CompileConnection(ctx context.Context, d *Device, id string, options Options) (_ *CompiledConnection, err error) {
	defer func() { err = fieldError("connections.selection", err) }()
	o, err := normalized(options)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err = Validate(ctx, d, o); err != nil {
		return nil, err
	}
	if id == "" {
		if len(d.Connections) != 1 {
			return nil, ErrInvalid
		}
		id = d.Connections[0].ID
	}
	var selected *Connection
	for i := range d.Connections {
		if d.Connections[i].ID == id {
			selected = &d.Connections[i]
			break
		}
	}
	installed := active(d.Installed, true)
	if selected == nil || !installed || !active(selected.Installed, installed) {
		return nil, ErrInvalid
	}
	count := 0
	for _, a := range d.ApplicationEntities {
		if active(a.Installed, installed) && a.AssociationAcceptor && has(a.ConnectionIDs, id) {
			count++
		}
	}
	if count == 0 {
		return nil, ErrInvalid
	}
	// Admit both the temporary rule/map headers and frozen catalog cumulatively
	// before creating either. Counting unmerged dual-role entries is conservative.
	backing := budget{max: o.Limits.MaxCompiledBytes}
	if err = backing.add(256 + 256*count); err != nil {
		return nil, err
	}
	for _, a := range d.ApplicationEntities {
		if !active(a.Installed, installed) || !a.AssociationAcceptor || !has(a.ConnectionIDs, id) {
			continue
		}
		if err = backing.add(len(a.AETitle)); err != nil {
			return nil, err
		}
		for _, t := range a.TransferCapabilities {
			if err = backing.add(256 + 160 + 2*len(t.SOPClassUID) + 96*len(t.TransferSyntaxUIDs)); err != nil {
				return nil, err
			}
			for _, ts := range t.TransferSyntaxUIDs {
				if err = backing.add(len(ts)); err != nil {
					return nil, err
				}
			}
		}
	}
	profiles := make([]ul.CalledAEProfile, count)
	index := 0
	for _, a := range d.ApplicationEntities {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if !active(a.Installed, installed) || !a.AssociationAcceptor || !has(a.ConnectionIDs, id) {
			continue
		}
		positions := make(map[string]int, len(a.TransferCapabilities))
		rules := make([]ul.PresentationContextRule, 0, len(a.TransferCapabilities))
		for _, t := range a.TransferCapabilities {
			pos, found := positions[t.SOPClassUID]
			if !found {
				pos = len(rules)
				positions[t.SOPClassUID] = pos
				rules = append(rules, ul.PresentationContextRule{AbstractSyntaxUID: t.SOPClassUID, TransferSyntaxes: t.TransferSyntaxUIDs, Roles: &ul.RequestorRoles{}})
			} else if !same(rules[pos].TransferSyntaxes, t.TransferSyntaxUIDs) {
				return nil, ErrInvalid
			}
			if t.Role == RoleSCP {
				rules[pos].Roles.SCU = true
			} else {
				rules[pos].Roles.SCP = true
			}
		}
		profiles[index] = ul.CalledAEProfile{AETitle: ul.NormalizeAETitle(a.AETitle), Profile: ul.AssociationProfile{Rules: rules}}
		index++
	}
	registry, err := ul.NewCalledAERegistry(profiles, ul.CalledAERegistryOptions{MaxAEs: o.Limits.MaxAEs, MaxRules: o.Limits.MaxCapabilities, MaxRulesPerAE: o.Limits.MaxCapabilitiesPerAE, MaxTransferSyntaxes: o.Limits.MaxTransferSyntaxes, MaxTransferSyntaxesPerRule: o.Limits.MaxTransferSyntaxesPerCapability, MaxCompiledBytes: o.Limits.MaxCompiledBytes})
	if err != nil {
		if errors.Is(err, ul.ErrCalledAERegistryLimit) {
			return nil, ErrResourceLimit
		}
		return nil, ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &CompiledConnection{address: net.JoinHostPort(strings.Clone(selected.Hostname), strconv.Itoa(selected.Port)), tlsRequired: selected.TLSRequired, registry: registry}, nil
}
func has(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}
func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
