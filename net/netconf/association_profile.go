package netconf

import (
	"context"
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// FromAssociationProfile converts an explicitly selected acceptor profile.
// Requestor roles are inverted to local roles. Raw extended replies are refused;
// the model describes services but does not assert their implementation.
func FromAssociationProfile(ctx context.Context, title string, connectionIDs []string, p ul.AssociationProfile, options Options) (_ ApplicationEntity, err error) {
	defer func() { err = fieldError("associationProfile", err) }()
	o, err := normalized(options)
	if err != nil {
		return ApplicationEntity{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err = ctx.Err(); err != nil {
		return ApplicationEntity{}, err
	}
	if len(p.Contexts) != 0 || len(p.ExtendedNegotiation) != 0 || len(p.Rules) == 0 || len(connectionIDs) == 0 || len(connectionIDs) > o.Limits.MaxConnections {
		return ApplicationEntity{}, ErrInvalid
	}
	// Registry admission validates and bounds the complete detached profile first.
	// Resolve profile RoleSelections only when they agree with the rule policy.
	if len(p.Rules) > o.Limits.MaxCapabilitiesPerAE || len(p.Rules) > o.Limits.MaxCapabilities {
		return ApplicationEntity{}, ErrResourceLimit
	}
	if len(p.RoleSelections) > len(p.Rules) {
		return ApplicationEntity{}, ErrInvalid
	}
	// Preflight the expanded local-role representation before any detached
	// registry/view/result. Dual roles retain two independently owned TS lists.
	expanded := budget{max: o.Limits.MaxCompiledBytes}
	if err = expanded.add(256 + len(title) + 16*len(connectionIDs)); err != nil {
		return ApplicationEntity{}, err
	}
	for i, id := range connectionIDs {
		if !textOK(id, o.Limits.MaxNameBytes, false) || strings.TrimSpace(id) != id {
			return ApplicationEntity{}, ErrInvalid
		}
		for j := 0; j < i; j++ {
			if id == connectionIDs[j] {
				return ApplicationEntity{}, ErrInvalid
			}
		}
		if err = expanded.add(len(id)); err != nil {
			return ApplicationEntity{}, err
		}
	}
	capCount, tsCount := 0, 0
	for _, rule := range p.Rules {
		copies := 1
		if rule.Roles != nil && rule.Roles.SCU && rule.Roles.SCP {
			copies = 2
		}
		if copies > o.Limits.MaxCapabilitiesPerAE-capCount || copies > o.Limits.MaxCapabilities-capCount || len(rule.TransferSyntaxes) > o.Limits.MaxTransferSyntaxesPerCapability {
			return ApplicationEntity{}, ErrResourceLimit
		}
		capCount += copies
		for i := 0; i < copies; i++ {
			if len(rule.TransferSyntaxes) > o.Limits.MaxTransferSyntaxes-tsCount {
				return ApplicationEntity{}, ErrResourceLimit
			}
			tsCount += len(rule.TransferSyntaxes)
			if err = expanded.add(160 + len(rule.AbstractSyntaxUID) + 24*len(rule.TransferSyntaxes)); err != nil {
				return ApplicationEntity{}, err
			}
			for _, ts := range rule.TransferSyntaxes {
				if err = expanded.add(len(ts)); err != nil {
					return ApplicationEntity{}, err
				}
			}
		}
	}
	used := make([]bool, len(p.RoleSelections))
	for i, role := range p.RoleSelections {
		for j := 0; j < i; j++ {
			if p.RoleSelections[j].SopClassUID == role.SopClassUID {
				return ApplicationEntity{}, ErrInvalid
			}
		}
	}
	for _, r := range p.Rules {
		for i, role := range p.RoleSelections {
			if role.SopClassUID == r.AbstractSyntaxUID {
				if used[i] || r.Roles == nil || r.Roles.SCU != role.SCURole || r.Roles.SCP != role.SCPRole {
					return ApplicationEntity{}, ErrInvalid
				}
				used[i] = true
			}
		}
	}
	for _, v := range used {
		if !v {
			return ApplicationEntity{}, ErrInvalid
		}
	}
	p.RoleSelections = nil
	reg, err := ul.NewCalledAERegistry([]ul.CalledAEProfile{{AETitle: title, Profile: p}}, ul.CalledAERegistryOptions{MaxAEs: o.Limits.MaxAEs, MaxRules: o.Limits.MaxCapabilities, MaxRulesPerAE: o.Limits.MaxCapabilitiesPerAE, MaxTransferSyntaxes: o.Limits.MaxTransferSyntaxes, MaxTransferSyntaxesPerRule: o.Limits.MaxTransferSyntaxesPerCapability, MaxCompiledBytes: o.Limits.MaxCompiledBytes})
	if err != nil {
		if errors.Is(err, ul.ErrCalledAERegistryLimit) {
			return ApplicationEntity{}, ErrResourceLimit
		}
		return ApplicationEntity{}, ErrInvalid
	}
	profile := reg.Profiles()[0]
	count := 0
	for _, r := range profile.Profile.Rules {
		if r.Roles == nil {
			count++
		} else {
			if r.Roles.SCU {
				count++
			}
			if r.Roles.SCP {
				count++
			}
		}
	}
	if count > o.Limits.MaxCapabilitiesPerAE || count > o.Limits.MaxCapabilities {
		return ApplicationEntity{}, ErrResourceLimit
	}
	a := ApplicationEntity{AETitle: profile.AETitle, ConnectionIDs: append([]string(nil), connectionIDs...), AssociationAcceptor: true, TransferCapabilities: make([]TransferCapability, count)}
	i := 0
	for _, r := range profile.Profile.Rules {
		if r.Roles == nil || r.Roles.SCU {
			a.TransferCapabilities[i] = TransferCapability{SOPClassUID: r.AbstractSyntaxUID, Role: RoleSCP, TransferSyntaxUIDs: append([]string(nil), r.TransferSyntaxes...)}
			i++
		}
		if r.Roles != nil && r.Roles.SCP {
			a.TransferCapabilities[i] = TransferCapability{SOPClassUID: r.AbstractSyntaxUID, Role: RoleSCU, TransferSyntaxUIDs: append([]string(nil), r.TransferSyntaxes...)}
			i++
		}
	}
	if err = ctx.Err(); err != nil {
		return ApplicationEntity{}, err
	}
	return a, nil
}
