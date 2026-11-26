package netconf

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"testing"
)

func TestAcceptorProfileConversionInvertsRoles(t *testing.T) {
	p := ul.AssociationProfile{Rules: []ul.PresentationContextRule{{AbstractSyntaxUID: "1.2.840.10008.1.1", TransferSyntaxes: []string{ul.ExplicitVRLittleEndian}, Roles: &ul.RequestorRoles{SCU: true, SCP: true}}}, RoleSelections: []ul.RoleSelectionItem{{SopClassUID: "1.2.840.10008.1.1", SCURole: true, SCPRole: true}}}
	ae, err := FromAssociationProfile(context.Background(), "AE", []string{"local"}, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ae.TransferCapabilities) != 2 || ae.TransferCapabilities[0].Role != RoleSCP || ae.TransferCapabilities[1].Role != RoleSCU {
		t.Fatalf("converted %+v", ae)
	}
	ae.TransferCapabilities[0].TransferSyntaxUIDs[0] = "1.2"
	if p.Rules[0].TransferSyntaxes[0] != ul.ExplicitVRLittleEndian {
		t.Fatal("profile mutated")
	}
	p.RoleSelections[0].SCPRole = false
	if _, err = FromAssociationProfile(context.Background(), "AE", []string{"local"}, p, Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("inconsistent roles %v", err)
	}
	p = ul.AssociationProfile{Contexts: []ul.PresentationContext{{AbstractSyntaxUID: "1.2"}}}
	if _, err = FromAssociationProfile(context.Background(), "AE", []string{"local"}, p, Options{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("requestor accepted %v", err)
	}
}
