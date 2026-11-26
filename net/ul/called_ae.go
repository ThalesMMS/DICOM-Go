package ul

import (
	"errors"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

var ErrCalledAERegistry = errors.New("dicom ul: invalid Called AE registry")
var ErrCalledAERegistryLimit = errors.New("dicom ul: Called AE registry resource limit")

// CalledAEProfile names an acceptor profile. Roles describe the requestor.
// Extended reply bytes and requestor Contexts are deliberately unsupported.
type CalledAEProfile struct {
	AETitle string
	Profile AssociationProfile
}
type CalledAERegistryOptions struct {
	MaxAEs, MaxRules, MaxRulesPerAE, MaxTransferSyntaxes, MaxTransferSyntaxesPerRule, MaxCompiledBytes int
}

// CalledAERegistry is immutable. Profiles and AETitles return detached views.
// The registry performs no I/O, callbacks or catalog reload during negotiation.
type CalledAERegistry struct {
	entries []CalledAEProfile
	byTitle map[string]int
}

func registryOptions(o CalledAERegistryOptions) (CalledAERegistryOptions, error) {
	d := CalledAERegistryOptions{64, 4096, 1024, 16384, 256, 8 << 20}
	p := [][2]*int{{&o.MaxAEs, &d.MaxAEs}, {&o.MaxRules, &d.MaxRules}, {&o.MaxRulesPerAE, &d.MaxRulesPerAE}, {&o.MaxTransferSyntaxes, &d.MaxTransferSyntaxes}, {&o.MaxTransferSyntaxesPerRule, &d.MaxTransferSyntaxesPerRule}, {&o.MaxCompiledBytes, &d.MaxCompiledBytes}}
	for _, v := range p {
		if *v[0] < 0 || *v[0] > math.MaxInt/1024 {
			return o, ErrCalledAERegistryLimit
		}
		if *v[0] == 0 {
			*v[0] = *v[1]
		}
	}
	return o, nil
}
func NewCalledAERegistry(input []CalledAEProfile, options CalledAERegistryOptions) (*CalledAERegistry, error) {
	o, err := registryOptions(options)
	if err != nil {
		return nil, err
	}
	if len(input) == 0 {
		return nil, ErrCalledAERegistry
	}
	if len(input) > o.MaxAEs {
		return nil, ErrCalledAERegistryLimit
	}
	used, rules, syntax := 0, 0, 0
	add := func(n int) error {
		if n < 0 || n > o.MaxCompiledBytes-used {
			return ErrCalledAERegistryLimit
		}
		used += n
		return nil
	}
	// Complete cumulative preflight precedes every catalog map/slice/string copy.
	for _, e := range input {
		if err = add(256 + len(e.AETitle)); err != nil {
			return nil, err
		}
		if len(e.Profile.Rules) > o.MaxRulesPerAE || len(e.Profile.Rules) > o.MaxRules-rules {
			return nil, ErrCalledAERegistryLimit
		}
		rules += len(e.Profile.Rules)
		if len(e.Profile.Contexts) != 0 || len(e.Profile.ExtendedNegotiation) != 0 || len(e.Profile.Rules) == 0 || ValidateAETitle(NormalizeAETitle(e.AETitle)) != nil {
			return nil, ErrCalledAERegistry
		}
		if !registryRolesAgree(e.Profile) {
			return nil, ErrCalledAERegistry
		}
		for _, r := range e.Profile.Rules {
			if len(r.TransferSyntaxes) > o.MaxTransferSyntaxesPerRule || len(r.TransferSyntaxes) > o.MaxTransferSyntaxes-syntax {
				return nil, ErrCalledAERegistryLimit
			}
			syntax += len(r.TransferSyntaxes)
			if err = add(160 + len(r.AbstractSyntaxUID) + 80*len(r.TransferSyntaxes)); err != nil {
				return nil, err
			}
			for _, ts := range r.TransferSyntaxes {
				if err = add(len(ts)); err != nil {
					return nil, err
				}
			}
		}
	}
	result := &CalledAERegistry{entries: make([]CalledAEProfile, len(input)), byTitle: make(map[string]int, len(input))}
	for i, e := range input {
		title := NormalizeAETitle(e.AETitle)
		if _, exists := result.byTitle[title]; exists {
			return nil, ErrCalledAERegistry
		}
		seen := make(map[string]bool, len(e.Profile.Rules))
		for _, r := range e.Profile.Rules {
			if !core.IsValidUID(r.AbstractSyntaxUID) || seen[r.AbstractSyntaxUID] || len(r.TransferSyntaxes) == 0 || r.Roles != nil && (!r.Roles.SCU && !r.Roles.SCP) {
				return nil, ErrCalledAERegistry
			}
			seen[r.AbstractSyntaxUID] = true
			tsSeen := make(map[string]bool, len(r.TransferSyntaxes))
			for _, ts := range r.TransferSyntaxes {
				if !core.IsValidUID(ts) || tsSeen[ts] {
					return nil, ErrCalledAERegistry
				}
				tsSeen[ts] = true
			}
		}
		result.entries[i] = cloneCalledAEProfile(e)
		result.entries[i].AETitle = strings.Clone(title)
		result.byTitle[result.entries[i].AETitle] = i
	}
	return result, nil
}

// AssociationConfig acceptor profiles retain explicit requestor RoleSelections
// as well as equivalent Rules. Admit that representation without introducing a
// second role catalog; the frozen registry retains the equivalent Rules only.
func registryRolesAgree(p AssociationProfile) bool {
	if len(p.RoleSelections) > len(p.Rules) {
		return false
	}
	for i, role := range p.RoleSelections {
		if !core.IsValidUID(role.SopClassUID) || (!role.SCURole && !role.SCPRole) {
			return false
		}
		for j := 0; j < i; j++ {
			if p.RoleSelections[j].SopClassUID == role.SopClassUID {
				return false
			}
		}
		found := false
		for _, rule := range p.Rules {
			if rule.AbstractSyntaxUID == role.SopClassUID {
				if rule.Roles == nil || rule.Roles.SCU != role.SCURole || rule.Roles.SCP != role.SCPRole {
					return false
				}
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func cloneCalledAEProfile(e CalledAEProfile) CalledAEProfile {
	out := CalledAEProfile{AETitle: e.AETitle}
	out.Profile.Rules = make([]PresentationContextRule, len(e.Profile.Rules))
	for i, r := range e.Profile.Rules {
		c := r
		c.AbstractSyntaxUID = strings.Clone(r.AbstractSyntaxUID)
		c.TransferSyntaxes = make([]string, len(r.TransferSyntaxes))
		for j, ts := range r.TransferSyntaxes {
			c.TransferSyntaxes[j] = strings.Clone(ts)
		}
		if r.Roles != nil {
			roles := *r.Roles
			c.Roles = &roles
		}
		out.Profile.Rules[i] = c
	}
	return out
}
func (r *CalledAERegistry) AETitles() []string {
	if r == nil {
		return nil
	}
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.AETitle
	}
	return out
}
func (r *CalledAERegistry) Profiles() []CalledAEProfile {
	if r == nil {
		return nil
	}
	out := make([]CalledAEProfile, len(r.entries))
	for i, e := range r.entries {
		out[i] = cloneCalledAEProfile(e)
	}
	return out
}
func (r *CalledAERegistry) lookup(title string) (CalledAEProfile, bool) {
	if r == nil {
		return CalledAEProfile{}, false
	}
	i, ok := r.byTitle[NormalizeAETitle(title)]
	if !ok {
		return CalledAEProfile{}, false
	}
	return r.entries[i], true
}
func validateCalledAEOptions(o AcceptOptions) error {
	if o.CalledAERegistry == nil {
		return nil
	}
	if len(o.CalledAERegistry.entries) == 0 || len(o.SupportedAbstractSyntaxes) != 0 || len(o.SupportedTransferSyntaxes) != 0 || len(o.PresentationContextRules) != 0 || len(o.RoleSelections) != 0 || o.AcceptAnyAbstractSyntax || o.AlwaysAcceptDefaultRole || o.CommonExtendedNegotiationHandler != nil || o.RequireMatchingCalledAE || len(o.CalledAEPresentationContextRules) != 0 {
		return ErrCalledAERegistry
	}
	return nil
}
