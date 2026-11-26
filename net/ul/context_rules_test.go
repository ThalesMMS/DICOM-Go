package ul

import (
	"errors"
	"net"
	"testing"
)

// TestPresentationContextRuleRoles follows the requestor/acceptor role table
// of DCMTK's dul.h, which its tscuscp role test exercises: the reply is the
// roles both sides allow, a context without a common role is rejected (here
// the only one, so the association is rejected), and a requestor proposing no
// role takes the default SCU role.
func TestPresentationContextRuleRoles(t *testing.T) {
	scu, scp, both := &RequestorRoles{SCU: true}, &RequestorRoles{SCP: true}, &RequestorRoles{SCU: true, SCP: true}
	proposal := func(roles *RequestorRoles) *RoleSelectionItem {
		if roles == nil {
			return nil
		}
		return &RoleSelectionItem{SopClassUID: verificationSOPClassUID, SCURole: roles.SCU, SCPRole: roles.SCP}
	}
	for _, tc := range []struct {
		name          string
		requested     *RequestorRoles // nil: no role selection proposed
		policy        *RequestorRoles // nil: rule without role policy
		alwaysDefault bool
		reject        bool
		want          *RequestorRoles // nil: no reply (default roles)
	}{
		{"SCU vs SCP", scu, scp, false, true, nil},
		{"SCU vs SCU", scu, scu, false, false, scu},
		{"SCU vs SCU/SCP", scu, both, false, false, scu},
		{"SCU vs default", scu, nil, false, false, nil},
		{"SCP vs SCP", scp, scp, false, false, scp},
		{"SCP vs SCU", scp, scu, false, true, nil},
		{"SCP vs SCU/SCP", scp, both, false, false, scp},
		{"SCP vs default", scp, nil, false, false, nil},
		{"SCU/SCP vs SCP", both, scp, false, false, scp},
		{"SCU/SCP vs SCU", both, scu, false, false, scu},
		{"SCU/SCP vs SCU/SCP", both, both, false, false, both},
		{"SCU/SCP vs default", both, nil, false, false, nil},
		{"default vs SCP", nil, scp, false, true, nil},
		{"default vs SCP, always accept default", nil, scp, true, false, nil},
		{"default vs SCU", nil, scu, false, false, nil},
		{"default vs SCU/SCP", nil, both, false, false, nil},
		{"default vs default", nil, nil, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accept := AcceptOptions{
				AETitle: "ACCEPTOR", SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}, AlwaysAcceptDefaultRole: tc.alwaysDefault,
				PresentationContextRules: []PresentationContextRule{{AbstractSyntaxUID: verificationSOPClassUID, Roles: tc.policy}},
			}
			got, err := dialWithRole(t, accept, proposal(tc.requested))
			if tc.reject {
				if !errors.Is(err, ErrAssociationRejected) {
					t.Fatalf("Dial error %v, want the association rejected", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("reply %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].SCURole != tc.want.SCU || got[0].SCPRole != tc.want.SCP {
				t.Fatalf("reply %+v, want SCU=%v SCP=%v", got, tc.want.SCU, tc.want.SCP)
			}
		})
	}
}

func TestPresentationContextRuleTransferSyntaxes(t *testing.T) {
	const secondaryCapture = "1.2.840.10008.5.1.4.1.1.7"
	opts := AcceptOptions{
		SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
		SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		PresentationContextRules: []PresentationContextRule{
			{AbstractSyntaxUID: secondaryCapture, TransferSyntaxes: []string{ExplicitVRLittleEndian, ImplicitVRLittleEndian}},
			{AbstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.2", TransferSyntaxes: []string{ExplicitVRLittleEndian}},
		},
	}
	proposed := []PresentationContextProposed{
		{ID: 1, AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ExplicitVRLittleEndian, ImplicitVRLittleEndian}},
		{ID: 3, AbstractSyntaxUID: secondaryCapture, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian, ExplicitVRLittleEndian}},
		{ID: 5, AbstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.2", TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}},
		{ID: 7, AbstractSyntaxUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}},
	}
	results, _ := negotiatePresentationContexts(proposed, opts)
	want := []struct {
		result byte
		ts     string
	}{
		{PresentationContextAcceptance, ImplicitVRLittleEndian}, // global list
		{PresentationContextAcceptance, ExplicitVRLittleEndian}, // rule order wins
		{PresentationContextTransferSyntaxesNotSupported, ""},   // rule excludes Implicit
		{PresentationContextAbstractSyntaxNotSupported, ""},     // neither listed nor ruled
	}
	for i, result := range results {
		if result.Result != want[i].result || (want[i].ts != "" && result.TransferSyntaxUID != want[i].ts) {
			t.Errorf("context %d: result %d %s, want %d %s", result.ID, result.Result, result.TransferSyntaxUID, want[i].result, want[i].ts)
		}
	}
}

// TestCalledAEPresentationContextRules gives one listener answering two
// aliases a per-alias policy: a syntax ruled only on PROVIDER is supported
// there and refused on ALIAS, and a role allowed only on PROVIDER is granted
// there only. Titles compare after padding normalization, case-sensitively.
func TestCalledAEPresentationContextRules(t *testing.T) {
	const commitment = "1.2.840.10008.1.20.1"
	accept := AcceptOptions{
		AETitle:                   "PROVIDER",
		SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
		SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		PresentationContextRules:  []PresentationContextRule{{AbstractSyntaxUID: commitment, Roles: &RequestorRoles{SCP: true}}},
		CalledAEPresentationContextRules: map[string][]PresentationContextRule{
			"PROVIDER": {{AbstractSyntaxUID: commitment, Roles: &RequestorRoles{SCU: true, SCP: true}}},
			"OTHER":    nil,
		},
	}
	dial := func(t *testing.T, called string, roles []RoleSelectionItem) (bool, []RoleSelectionItem) {
		t.Helper()
		addr, done := acceptOnce(t, accept)
		assoc, err := Dial(addr, DialOptions{
			CalledAETitle: called, CallingAETitle: "REQUESTOR", RoleSelections: roles,
			Contexts: []PresentationContext{
				{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}},
				{AbstractSyntaxUID: commitment, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}},
			},
		})
		if err != nil {
			<-done
			t.Fatalf("called %q: %v", called, err)
		}
		found := false
		for _, context := range assoc.AcceptedContexts {
			found = found || context.AbstractSyntaxUID == commitment
		}
		granted := append([]RoleSelectionItem(nil), assoc.AcceptedRoleSelections...)
		_ = assoc.Close()
		<-done
		return found, granted
	}
	scu := []RoleSelectionItem{{SopClassUID: commitment, SCURole: true}}
	scp := []RoleSelectionItem{{SopClassUID: commitment, SCPRole: true}}
	for _, called := range []string{"PROVIDER", " PROVIDER  "} {
		if found, _ := dial(t, called, nil); !found {
			t.Fatalf("called %q: the default SCU role was refused on the provider alias", called)
		}
		if found, granted := dial(t, called, scu); !found || len(granted) != 1 || !granted[0].SCURole || granted[0].SCPRole {
			t.Fatalf("called %q: SCU role accepted=%t roles=%+v", called, found, granted)
		}
	}
	for _, called := range []string{"ALIAS", "provider"} {
		if found, _ := dial(t, called, nil); found {
			t.Fatalf("called %q: the default SCU role was accepted off the provider alias", called)
		}
		if found, _ := dial(t, called, scu); found {
			t.Fatalf("called %q: the SCU role was accepted off the provider alias", called)
		}
		if found, granted := dial(t, called, scp); !found || len(granted) != 1 || !granted[0].SCPRole || granted[0].SCURole {
			t.Fatalf("called %q: SCP role accepted=%t roles=%+v", called, found, granted)
		}
	}
	// An alias whose list is empty supports only the global syntaxes.
	if found, _ := dial(t, "OTHER", scp); found {
		t.Fatal("an alias with no rules accepted the ruled syntax")
	}
}

func TestCalledAEPresentationContextRulesValidation(t *testing.T) {
	valid := []PresentationContextRule{{AbstractSyntaxUID: verificationSOPClassUID}}
	for _, byTitle := range []map[string][]PresentationContextRule{
		{"": valid},
		{"BAD\\AE": valid},
		{"TOO_LONG_AE_TITLE_X": valid},
		{"TWIN": valid, " TWIN ": valid},
		{"TWIN": {{AbstractSyntaxUID: verificationSOPClassUID, Roles: &RequestorRoles{}}}},
	} {
		server, client := net.Pipe()
		_, err := Accept(server, AcceptOptions{AETitle: "ACCEPTOR", CalledAEPresentationContextRules: byTitle})
		_ = server.Close()
		_ = client.Close()
		if !errors.Is(err, ErrInvalidPresentationContextRule) {
			t.Errorf("rules %+v: Accept error %v", byTitle, err)
		}
	}
}

func TestPresentationContextRuleValidation(t *testing.T) {
	for _, rules := range [][]PresentationContextRule{
		{{AbstractSyntaxUID: " "}},
		{{AbstractSyntaxUID: verificationSOPClassUID}, {AbstractSyntaxUID: verificationSOPClassUID}},
		{{AbstractSyntaxUID: verificationSOPClassUID, Roles: &RequestorRoles{}}},
	} {
		server, client := net.Pipe()
		_, err := Accept(server, AcceptOptions{AETitle: "ACCEPTOR", PresentationContextRules: rules})
		_ = server.Close()
		_ = client.Close()
		if !errors.Is(err, ErrInvalidPresentationContextRule) {
			t.Errorf("rules %+v: Accept error %v", rules, err)
		}
	}
}
