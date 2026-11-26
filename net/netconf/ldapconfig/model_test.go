package ldapconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/netconf"
	ber "github.com/go-asn1-ber/asn1-ber"
	ldap "github.com/go-ldap/ldap/v3"
)

const fixtureRoot = "cn=Devices,cn=DICOM Configuration,dc=example"

func fixtureEntries(name string) (*ldap.Entry, []*ldap.Entry) {
	dn := "dicomDeviceName=" + ldap.EscapeDN(name) + "," + fixtureRoot
	connectionDN := "cn=plain," + dn
	device := ldap.NewEntry(dn, map[string][]string{"objectClass": {"top", "dicomDevice"}, "dicomDeviceName": {name}, "dicomInstalled": {"TRUE"}, "dicomDescription": {"Synthetic device"}})
	connection := ldap.NewEntry(connectionDN, map[string][]string{"objectClass": {"dicomNetworkConnection"}, "dicomHostname": {"127.0.0.1"}, "dicomPort": {"11112"}})
	children := []*ldap.Entry{connection}
	for _, title := range []string{"CT_A", "ct_a"} {
		aeDN := "dicomAETitle=" + title + "," + dn
		children = append(children, ldap.NewEntry(aeDN, map[string][]string{"objectClass": {"dicomNetworkAE"}, "dicomAETitle": {title}, "dicomNetworkConnectionReference": {connectionDN}, "dicomAssociationInitiator": {"FALSE"}, "dicomAssociationAcceptor": {"TRUE"}}))
		children = append(children, ldap.NewEntry("cn=verification,"+aeDN, map[string][]string{"objectClass": {"dicomTransferCapability"}, "dicomSOPClass": {"1.2.840.10008.1.1"}, "dicomTransferRole": {"SCP"}, "dicomTransferSyntax": {"1.2.840.10008.1.2.1", "1.2.840.10008.1.2"}}))
	}
	return device, children
}

func TestLDAPDirectoryModelPreservesDistinctTitlesReferencesAndMissingPort(t *testing.T) {
	o, _ := normalized(Options{})
	source, children := fixtureEntries("scanner")
	device, err := mapDevice(context.Background(), source, children, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(device.Model.ApplicationEntities) != 2 || device.Model.ApplicationEntities[0].AETitle != "CT_A" || device.Model.ApplicationEntities[1].AETitle != "ct_a" || len(device.Model.ApplicationEntities[0].TransferCapabilities) != 1 {
		t.Fatal("LDAP identities/capabilities lost")
	}
	encoded, err := json.Marshal(device.Model)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := netconf.Parse(context.Background(), bytes.NewReader(encoded), netconf.Options{})
	if err != nil || decoded.Connections[0].ID != device.Model.Connections[0].ID {
		t.Fatal("same JSON model not accepted", err)
	}
	children[0] = ldap.NewEntry(children[0].DN, map[string][]string{"objectClass": {"dicomNetworkConnection"}, "dicomHostname": {"127.0.0.1"}})
	device, err = mapDevice(context.Background(), source, children, o)
	if err != nil || device.Model.Connections[0].Port != 0 || len(device.Diagnostics) != 1 {
		t.Fatal("missing port invented", err)
	}
	children[1].Attributes = append(children[1].Attributes, &ldap.EntryAttribute{Name: "dicomInstalled", Values: []string{"FALSE"}})
	device, err = mapDevice(context.Background(), source, children, o)
	if err != nil || device.Model.ApplicationEntities[0].Installed == nil || *device.Model.ApplicationEntities[0].Installed {
		t.Fatal("explicit inactive AE lost", err)
	}
}

func TestLDAPDirectoryModelRejectsDanglingForeignAndContradictoryDeclarations(t *testing.T) {
	o, _ := normalized(Options{})
	for _, mutate := range []func(*ldap.Entry, []*ldap.Entry){
		func(_ *ldap.Entry, c []*ldap.Entry) { c[0].DN = "cn=plain,dicomDeviceName=other," + fixtureRoot },
		func(_ *ldap.Entry, c []*ldap.Entry) {
			c[2].DN = "cn=verification,dicomAETitle=missing,dicomDeviceName=scanner," + fixtureRoot
		},
		func(_ *ldap.Entry, c []*ldap.Entry) {
			c[1].Attributes = append(c[1].Attributes, &ldap.EntryAttribute{Name: "dicomAssociationAcceptor", Values: []string{"FALSE"}})
		},
		func(s *ldap.Entry, _ []*ldap.Entry) {
			s.Attributes = append(s.Attributes, &ldap.EntryAttribute{Name: "dicomDeviceName", Values: []string{"duplicate"}})
		},
	} {
		source, children := fixtureEntries("scanner")
		root, _ := ldap.ParseDN(fixtureRoot)
		mutate(source, children)
		_, admitErr := admitEntry(source, root, o)
		for _, child := range children {
			if _, err := admitEntry(child, root, o); err != nil {
				admitErr = err
			}
		}
		_, err := mapDevice(context.Background(), source, children, o)
		if admitErr == nil && err == nil {
			t.Fatal("invalid schema became a model")
		}
	}
	root, _ := ldap.ParseDN(fixtureRoot)
	foreign := ldap.NewEntry("dicomDeviceName=scanner,dc=foreign", nil)
	if _, err := admitEntry(foreign, root, o); !errors.Is(err, ErrInvalid) {
		t.Fatal("out-of-scope DN admitted", err)
	}
}

func TestLDAPDirectoryFramingRefusesLengthsAndDepthBeforeClient(t *testing.T) {
	o, _ := normalized(Options{})
	for _, data := range [][]byte{{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff}, {0x30, 0x80}, {0x30, 0x85}, {0x30, 0x03, 0x04, 0x02, 0}} {
		if _, err := readFrame(bytes.NewReader(data), o, o.MaxWireBytes); err == nil {
			t.Fatal("malformed frame admitted")
		}
	}
	deep := ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "x", "")
	for i := 0; i < o.MaxBERDepth+2; i++ {
		next := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
		next.AppendChild(deep)
		deep = next
	}
	if err := validateBER(deep.Bytes(), o); !errors.Is(err, ErrLimit) {
		t.Fatal("BER stack admission lost", err)
	}
}

func TestLDAPDirectoryOpaquePagingAndCriticalityCannotReachTypedClient(t *testing.T) {
	o, _ := normalized(Options{})
	deep := ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "x", "")
	for i := 0; i < o.MaxBERDepth+2; i++ {
		next := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
		next.AppendChild(deep)
		deep = next
	}
	for _, test := range []struct {
		payload  []byte
		critical *ber.Packet
		cause    error
	}{
		{deep.Bytes(), nil, ErrLimit},
		{[]byte{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff}, nil, ErrProtocol},
		{ldap.NewControlPaging(1).Encode().Children[1].Data.Bytes(), ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "not-boolean", ""), ErrProtocol},
	} {
		message := resultMessage(1, 5, 0, "")
		controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "")
		control := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
		control.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, ldap.ControlTypePaging, ""))
		if test.critical != nil {
			control.AppendChild(test.critical)
		}
		control.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, string(test.payload), ""))
		controls.AppendChild(control)
		message.AppendChild(controls)
		if _, err := validateEnvelope(message.Bytes(), o); !errors.Is(err, test.cause) {
			t.Fatalf("unsafe opaque control admitted: %v", err)
		}
	}
}

func TestLDAPDirectorySourceIdentityPreservesValueCaseAndCanonicalizesDNSpelling(t *testing.T) {
	a, _ := dnKey("dicomDeviceName=Scanner,cn=Devices,dc=example")
	b, _ := dnKey("DICOMDEVICENAME=Scanner,CN=Devices,DC=example")
	c, _ := dnKey("dicomDeviceName=scanner,cn=Devices,dc=example")
	if a != b || a == c {
		t.Fatal("source identity confused attribute names and case-sensitive values")
	}
}

func TestLDAPDirectoryConfigRedactsAndRefusesCredentialDowngrades(t *testing.T) {
	secret := "synthetic-secret-not-a-directory-error"
	c := Config{URL: "ldaps://127.0.0.1:636", BaseDN: fixtureRoot, BindDN: "cn=reader,dc=example", Password: secret}
	if err := ValidateConfig(c, Options{}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(c)
	if strings.Contains(string(encoded), secret) || strings.Contains(fmt.Sprintf("%#v", c), secret) {
		t.Fatal("credential exposed")
	}
	for _, u := range []string{"ldap://user:password@localhost", "ldaps://localhost?filter=x", "https://localhost", "ldaps://localhost/dc=other"} {
		c.URL = u
		if ValidateConfig(c, Options{}) == nil {
			t.Fatal("unconfigured URL scope accepted")
		}
	}
	c.URL = "ldap://127.0.0.1:389"
	c.AllowAnonymousCleartext = true
	if ValidateConfig(c, Options{}) == nil {
		t.Fatal("password allowed in cleartext")
	}
}
