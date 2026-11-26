package ul

import (
	"errors"
	"fmt"
	"strings"
)

// RequestorRoles are the roles an association requestor may take for a SOP
// Class, as in an SCP/SCU Role Selection sub-item (PS3.7 D.3.3.4).
type RequestorRoles struct {
	SCU bool
	SCP bool
}

// PresentationContextRule configures one abstract syntax at the acceptor, in
// the manner of a DCMTK association configuration profile.
type PresentationContextRule struct {
	AbstractSyntaxUID string
	// TransferSyntaxes are accepted for this abstract syntax in preference
	// order (or the proposer's order with PreferProposedTransferSyntaxOrder);
	// empty uses AcceptOptions.SupportedTransferSyntaxes.
	TransferSyntaxes []string
	// Roles, when set, is the role policy: the context is accepted only with
	// a role both sides allow, and the reply carries that role. A requestor
	// that proposes no role selection takes the default SCU role, so a
	// policy without SCU refuses it unless AlwaysAcceptDefaultRole is set.
	// Nil keeps the default roles and sends no role selection reply.
	Roles *RequestorRoles
}

// ErrInvalidPresentationContextRule reports an unusable
// AcceptOptions.PresentationContextRules entry.
var ErrInvalidPresentationContextRule = errors.New("dicom ul: invalid presentation context rule")

func validatePresentationContextRules(rules []PresentationContextRule) error {
	seen := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		uid := strings.TrimSpace(rule.AbstractSyntaxUID)
		if uid == "" {
			return fmt.Errorf("%w: empty abstract syntax", ErrInvalidPresentationContextRule)
		}
		if _, ok := seen[uid]; ok {
			return fmt.Errorf("%w: duplicate abstract syntax %q", ErrInvalidPresentationContextRule, uid)
		}
		seen[uid] = struct{}{}
		if rule.Roles != nil && !rule.Roles.SCU && !rule.Roles.SCP {
			return fmt.Errorf("%w: %q allows no role", ErrInvalidPresentationContextRule, uid)
		}
	}
	return nil
}

// validateCalledAEPresentationContextRules checks
// AcceptOptions.CalledAEPresentationContextRules: valid AE title keys that
// stay distinct after padding normalization and valid rule lists.
func validateCalledAEPresentationContextRules(byTitle map[string][]PresentationContextRule) error {
	seen := make(map[string]struct{}, len(byTitle))
	for key, rules := range byTitle {
		title := NormalizeAETitle(key)
		if err := ValidateAETitle(title); err != nil {
			return fmt.Errorf("%w: called AE: %w", ErrInvalidPresentationContextRule, err)
		}
		if _, ok := seen[title]; ok {
			return fmt.Errorf("%w: duplicate called AE %q", ErrInvalidPresentationContextRule, title)
		}
		seen[title] = struct{}{}
		if err := validatePresentationContextRules(rules); err != nil {
			return fmt.Errorf("called AE %q: %w", title, err)
		}
	}
	return nil
}

// calledAEPresentationContextRules returns the rules configured for the
// Called AE title, compared like EqualAETitles.
func calledAEPresentationContextRules(byTitle map[string][]PresentationContextRule, calledAE string) ([]PresentationContextRule, bool) {
	if len(byTitle) == 0 {
		return nil, false
	}
	if rules, ok := byTitle[calledAE]; ok {
		return rules, true
	}
	title := NormalizeAETitle(calledAE)
	for key, rules := range byTitle {
		if NormalizeAETitle(key) == title {
			return rules, true
		}
	}
	return nil, false
}

func presentationContextRule(opts AcceptOptions, abstractSyntaxUID string) (PresentationContextRule, bool) {
	for _, rule := range opts.PresentationContextRules {
		if strings.TrimSpace(rule.AbstractSyntaxUID) == abstractSyntaxUID {
			return rule, true
		}
	}
	return PresentationContextRule{}, false
}

// ruleRoleAccepts applies a rule's role policy to the requestor's proposal
// (nil when it sent no role selection for the SOP Class), following the
// requestor/acceptor table of DCMTK's dul.h: a proposal keeps the roles both
// sides allow and fails when none remain; no proposal means the default SCU
// role.
func ruleRoleAccepts(policy RequestorRoles, proposed *RoleSelectionItem, alwaysAcceptDefault bool) bool {
	if proposed == nil {
		return policy.SCU || alwaysAcceptDefault
	}
	return (proposed.SCURole && policy.SCU) || (proposed.SCPRole && policy.SCP)
}

func requestedRoleFor(requested []RoleSelectionItem, sopClassUID string) *RoleSelectionItem {
	for i := range requested {
		if requested[i].SopClassUID == sopClassUID {
			return &requested[i]
		}
	}
	return nil
}
