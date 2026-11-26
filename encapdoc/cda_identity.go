package encapdoc

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrCDAIdentityMismatch reports a contradiction between the DICOM HL7
// Instance Identifier and the ClinicalDocument id without exposing either.
var ErrCDAIdentityMismatch = fmt.Errorf("%w: HL7 Instance Identifier disagrees with the CDA document id", ErrInvalidDocument)

// CDAEnvelopeIdentifier reads the DICOM HL7 Instance Identifier (0040,E001)
// for comparison with the CDA payload. An absent or empty element returns an
// empty declaration. A present element must be a single readable ST value.
func CDAEnvelopeIdentifier(dataset *object.Object) (string, error) {
	element, ok := dataset.Get(tagHL7InstanceIdentifier)
	if !ok {
		return "", nil
	}
	if element.VR() == core.VRST {
		values, err := dataset.LookupStrings(tagHL7InstanceIdentifier)
		if err == nil {
			switch len(values) {
			case 0:
				return "", nil
			case 1:
				return values[0], nil
			}
		}
	}
	return "", fmt.Errorf("%w: unreadable CDA envelope HL7 Instance Identifier", ErrInvalidDocument)
}

func compareCDAIdentifiers(declared, source string) error {
	declared = core.TrimTextValue(core.VRST, declared)
	if declared == "" {
		return nil
	}
	declaredRoot, declaredExtension, declaredHasExtension := strings.Cut(declared, "^")
	sourceRoot, sourceExtension, sourceHasExtension := strings.Cut(core.TrimTextValue(core.VRST, source), "^")
	rootsMatch := declaredRoot == sourceRoot ||
		len(sourceRoot) == 36 && payloadOIDOrUUID(sourceRoot) && strings.EqualFold(declaredRoot, sourceRoot)
	if !rootsMatch || declaredHasExtension != sourceHasExtension || declaredExtension != sourceExtension {
		return ErrCDAIdentityMismatch
	}
	return nil
}

func cdaIdentifierAttributes(attributes []xml.Attr) (root, extension string, err error) {
	rootSeen, extensionSeen := false, false
	for _, attribute := range attributes {
		if attribute.Name.Space != "" {
			continue
		}
		switch attribute.Name.Local {
		case "root":
			if rootSeen {
				return "", "", ErrInvalidDocument
			}
			root, rootSeen = attribute.Value, true
		case "extension":
			if extensionSeen {
				return "", "", ErrInvalidDocument
			}
			extension, extensionSeen = attribute.Value, true
		}
	}
	return root, extension, nil
}
