package ldapconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/net/netconf"
	"github.com/ThalesMMS/dicom-go/net/ul"
	ldap "github.com/go-ldap/ldap/v3"
)

// Preserve DN value case, especially case-sensitive AE Titles. Attribute type
// case and AVA ordering are normalized; values are never blindly lowercased.
func dnKey(value string) (string, error) {
	dn, err := ldap.ParseDN(value)
	if err != nil || len(dn.RDNs) == 0 || len(dn.RDNs) > 32 {
		return "", ErrInvalid
	}
	rdns := make([][][2]string, len(dn.RDNs))
	for i, rdn := range dn.RDNs {
		for _, attr := range rdn.Attributes {
			rdns[i] = append(rdns[i], [2]string{strings.ToLower(attr.Type), attr.Value})
		}
		sort.Slice(rdns[i], func(a, b int) bool {
			if rdns[i][a][0] == rdns[i][b][0] {
				return rdns[i][a][1] < rdns[i][b][1]
			}
			return rdns[i][a][0] < rdns[i][b][0]
		})
	}
	encoded, _ := json.Marshal(rdns)
	return string(encoded), nil
}

func parentKey(value string) (string, error) {
	dn, err := ldap.ParseDN(value)
	if err != nil || len(dn.RDNs) < 2 {
		return "", ErrInvalid
	}
	return dnKey((&ldap.DN{RDNs: dn.RDNs[1:]}).String())
}

func admitEntry(entry *ldap.Entry, root *ldap.DN, o Options) (int, error) {
	if entry == nil || len(entry.DN) > 4096 || !utf8.ValidString(entry.DN) || len(entry.Attributes) > 64 {
		return 0, ErrLimit
	}
	dn, err := ldap.ParseDN(entry.DN)
	if err != nil || (!root.Equal(dn) && !root.AncestorOf(dn)) {
		return 0, ErrInvalid
	}
	if len(dn.RDNs)-len(root.RDNs) > o.MaxDepth {
		return 0, ErrLimit
	}
	weight := 256 + 8*len(entry.DN) + 64*len(entry.Attributes)
	seen := make(map[string]bool)
	for _, attr := range entry.Attributes {
		if attr == nil || len(attr.Name) > 128 || len(attr.Values) > 512 {
			return 0, ErrLimit
		}
		name := strings.ToLower(attr.Name)
		if seen[name] {
			return 0, ErrInvalid
		}
		seen[name] = true
		for _, value := range attr.Values {
			if len(value) > 8192 {
				return 0, ErrLimit
			}
			weight += 64 + 2*len(value)
			if weight > o.MaxEntryBytes || weight > o.MaxModelBytes {
				return 0, ErrLimit
			}
		}
	}
	return weight, nil
}

func single(entry *ldap.Entry, name string, required bool) (string, error) {
	values := entry.GetEqualFoldAttributeValues(name)
	if len(values) > 1 || (required && (len(values) == 0 || values[0] == "")) {
		return "", ErrInvalid
	}
	if len(values) == 0 {
		return "", nil
	}
	if !utf8.ValidString(values[0]) {
		return "", ErrInvalid
	}
	return values[0], nil
}

func boolean(entry *ldap.Entry, name string, required bool) (*bool, error) {
	value, err := single(entry, name, required)
	if err != nil || value == "" {
		return nil, err
	}
	var result bool
	switch strings.ToUpper(value) {
	case "TRUE":
		result = true
	case "FALSE":
		result = false
	default:
		return nil, ErrInvalid
	}
	return &result, nil
}

func objectClass(entry *ldap.Entry) (string, error) {
	result := ""
	for _, value := range entry.GetEqualFoldAttributeValues("objectClass") {
		for _, known := range []string{"dicomDevice", "dicomNetworkConnection", "dicomNetworkAE", "dicomTransferCapability"} {
			if strings.EqualFold(value, known) {
				if result != "" {
					return "", ErrInvalid
				}
				result = known
			}
		}
	}
	if result == "" {
		return "", ErrInvalid
	}
	return result, nil
}

func mapDevice(ctx context.Context, source *ldap.Entry, children []*ldap.Entry, o Options) (Device, error) {
	class, err := objectClass(source)
	if err != nil || class != "dicomDevice" {
		return Device{}, ErrInvalid
	}
	key, err := dnKey(source.DN)
	if err != nil {
		return Device{}, err
	}
	sourceHash := sha256.Sum256([]byte(key))
	result := Device{ID: hex.EncodeToString(sourceHash[:]), DN: source.DN, Model: netconf.Device{SchemaVersion: 1}}
	result.Model.DeviceName, err = single(source, "dicomDeviceName", true)
	if err != nil {
		return Device{}, err
	}
	result.Model.Description, err = single(source, "dicomDescription", false)
	if err != nil {
		return Device{}, err
	}
	result.Model.Installed, err = boolean(source, "dicomInstalled", true)
	if err != nil {
		return Device{}, err
	}
	connections := make(map[string]string)
	seen := make(map[string]bool)
	var aes, capabilities []*ldap.Entry
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return Device{}, err
		}
		childKey, err := dnKey(child.DN)
		if err != nil || seen[childKey] {
			return Device{}, ErrInvalid
		}
		seen[childKey] = true
		class, err := objectClass(child)
		if err != nil {
			return Device{}, err
		}
		parent, err := parentKey(child.DN)
		if err != nil {
			return Device{}, err
		}
		switch class {
		case "dicomNetworkConnection":
			if parent != key {
				return Device{}, ErrInvalid
			}
			fingerprint := sha256.Sum256([]byte(childKey))
			connection := netconf.Connection{ID: "ldap-" + hex.EncodeToString(fingerprint[:])}
			connections[childKey] = connection.ID
			connection.Hostname, err = single(child, "dicomHostname", true)
			if err != nil {
				return Device{}, err
			}
			port, err := single(child, "dicomPort", false)
			if err != nil {
				return Device{}, err
			}
			connection.Port, err = numericPort(port)
			if err != nil {
				return Device{}, err
			}
			if port == "" {
				result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "port-absent-initiator-only", Object: connection.ID})
			}
			connection.Installed, err = boolean(child, "dicomInstalled", false)
			if err != nil {
				return Device{}, err
			}
			connection.TLSRequired = len(child.GetEqualFoldAttributeValues("dicomTLSCipherSuite")) > 0
			result.Model.Connections = append(result.Model.Connections, connection)
		case "dicomNetworkAE":
			if parent != key {
				return Device{}, ErrInvalid
			}
			aes = append(aes, child)
		case "dicomTransferCapability":
			capabilities = append(capabilities, child)
		default:
			return Device{}, ErrInvalid
		}
	}
	if len(aes) == 0 && len(capabilities) == 0 {
		// A device without a DICOM Application Entity, such as a web
		// application or an audit record repository of a dcm4che
		// configuration, is not a discovery candidate. The zero schema
		// version marks it as not validated.
		result.Model.SchemaVersion = 0
		result.Model.Connections = nil
		return result, nil
	}
	knownAEs := make(map[string]bool)
	for _, entry := range aes {
		aeKey, _ := dnKey(entry.DN)
		knownAEs[aeKey] = true
	}
	for _, entry := range capabilities {
		parent, err := parentKey(entry.DN)
		if err != nil || !knownAEs[parent] {
			return Device{}, ErrInvalid
		}
	}
	for _, entry := range aes {
		if err := ctx.Err(); err != nil {
			return Device{}, err
		}
		title, err := single(entry, "dicomAETitle", true)
		if err != nil {
			return Device{}, err
		}
		ae := netconf.ApplicationEntity{AETitle: ul.NormalizeAETitle(title)}
		for _, ref := range entry.GetEqualFoldAttributeValues("dicomNetworkConnectionReference") {
			refKey, err := dnKey(ref)
			if err != nil || connections[refKey] == "" {
				return Device{}, ErrInvalid
			}
			ae.ConnectionIDs = append(ae.ConnectionIDs, connections[refKey])
		}
		initiator, err := boolean(entry, "dicomAssociationInitiator", true)
		if err != nil {
			return Device{}, err
		}
		acceptor, err := boolean(entry, "dicomAssociationAcceptor", true)
		if err != nil {
			return Device{}, err
		}
		ae.AssociationInitiator, ae.AssociationAcceptor = *initiator, *acceptor
		ae.Installed, err = boolean(entry, "dicomInstalled", false)
		if err != nil {
			return Device{}, err
		}
		aeKey, _ := dnKey(entry.DN)
		for _, entry := range capabilities {
			parent, _ := parentKey(entry.DN)
			if parent != aeKey {
				continue
			}
			tc := netconf.TransferCapability{}
			tc.SOPClassUID, err = single(entry, "dicomSOPClass", true)
			if err != nil {
				return Device{}, err
			}
			role, err := single(entry, "dicomTransferRole", true)
			if err != nil {
				return Device{}, err
			}
			tc.Role = netconf.Role(strings.ToUpper(role))
			tc.TransferSyntaxUIDs = append([]string(nil), entry.GetEqualFoldAttributeValues("dicomTransferSyntax")...)
			ae.TransferCapabilities = append(ae.TransferCapabilities, tc)
		}
		if len(ae.TransferCapabilities) == 0 {
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: "transfer-capabilities-absent", Object: ae.AETitle})
		}
		result.Model.ApplicationEntities = append(result.Model.ApplicationEntities, ae)
	}
	// Validate the entire declared device before applying an AE filter, so a
	// selector cannot conceal malformed references or contradictory capabilities.
	if err := netconf.Validate(ctx, &result.Model, o.Model); err != nil {
		return Device{}, err
	}
	if o.AETitle != "" {
		selected := result.Model.ApplicationEntities[:0]
		for _, ae := range result.Model.ApplicationEntities {
			if ae.AETitle == o.AETitle {
				selected = append(selected, ae)
			}
		}
		result.Model.ApplicationEntities = selected
	}
	return result, nil
}
