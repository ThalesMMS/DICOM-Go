package dimse

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// CRetrieveExtendedNegotiation is the Query/Retrieve C-MOVE and C-GET
// service-class application information of an SOP Class Extended Negotiation
// sub-item (PS3.4 C.5.2.1 and C.5.3.1): byte 1 relational retrieval and byte
// 2 Enhanced Multi-Frame Image Conversion. Byte 2 is optional; the acceptor
// answers a one-byte request with one byte.
type CRetrieveExtendedNegotiation struct {
	RelationalRetrieval               bool
	EnhancedMultiFrameImageConversion bool
}

// ParseCRetrieveExtendedNegotiation decodes C-MOVE/C-GET service-class
// application information. An absent byte 2 means "not supported"; bytes
// after byte 2 are ignored and a byte other than 1 is read as 0.
func ParseCRetrieveExtendedNegotiation(data []byte) (CRetrieveExtendedNegotiation, error) {
	if len(data) == 0 {
		return CRetrieveExtendedNegotiation{}, fmt.Errorf("dicom dimse: empty C-MOVE/C-GET extended negotiation")
	}
	return CRetrieveExtendedNegotiation{
		RelationalRetrieval:               data[0] == 1,
		EnhancedMultiFrameImageConversion: len(data) > 1 && data[1] == 1,
	}, nil
}

// Bytes encodes the options through the last one set, at least one byte.
func (n CRetrieveExtendedNegotiation) Bytes() []byte {
	return encodeExtendedNegotiationFlags([]bool{n.RelationalRetrieval, n.EnhancedMultiFrameImageConversion})
}

// Item returns the SOP Class Extended Negotiation sub-item that requests n
// for a C-MOVE or C-GET sopClassUID, for ul.DialOptions.ExtendedNegotiation.
func (n CRetrieveExtendedNegotiation) Item(sopClassUID string) ul.SopClassExtendedNegotiationItem {
	return ul.SopClassExtendedNegotiationItem{SopClassUID: sopClassUID, Data: n.Bytes()}
}

func (n CRetrieveExtendedNegotiation) intersect(supported CRetrieveExtendedNegotiation) CRetrieveExtendedNegotiation {
	return CRetrieveExtendedNegotiation{
		RelationalRetrieval:               n.RelationalRetrieval && supported.RelationalRetrieval,
		EnhancedMultiFrameImageConversion: n.EnhancedMultiFrameImageConversion && supported.EnhancedMultiFrameImageConversion,
	}
}

// StorageLevelOfSupport is byte 1 of the Storage service-class application
// information (PS3.4 B.3.1.1).
type StorageLevelOfSupport byte

const (
	StorageLevel0SCP    StorageLevelOfSupport = 0
	StorageLevel1SCP    StorageLevelOfSupport = 1
	StorageLevel2SCP    StorageLevelOfSupport = 2
	StorageLevelSCUOnly StorageLevelOfSupport = 3
)

// StorageDigitalSignatureSupport is byte 3 of the Storage service-class
// application information (PS3.4 B.3.1.2); zero is unspecified or SCU only.
type StorageDigitalSignatureSupport byte

const (
	StorageSignatureUnspecified StorageDigitalSignatureSupport = 0
	StorageSignatureLevel1      StorageDigitalSignatureSupport = 1
	StorageSignatureLevel2      StorageDigitalSignatureSupport = 2
	StorageSignatureLevel3      StorageDigitalSignatureSupport = 3
)

// StorageElementCoercion is byte 5 of the Storage service-class application
// information (PS3.4 B.3.1.3).
type StorageElementCoercion byte

const (
	StorageDoesNotCoerce   StorageElementCoercion = 0
	StorageMayCoerce       StorageElementCoercion = 1
	StorageCoercionSCUOnly StorageElementCoercion = 2
)

// StorageExtendedNegotiation is the Storage service-class application
// information of an SOP Class Extended Negotiation sub-item (PS3.4 B.3.1):
// byte 1 level of storage support, byte 3 level of digital signature support
// and byte 5 element coercion; bytes 2, 4 and 6 are reserved and sent as 00H.
// Unlike the Query/Retrieve options it is not an intersection: each side
// states its own capabilities.
type StorageExtendedNegotiation struct {
	LevelOfSupport          StorageLevelOfSupport
	DigitalSignatureSupport StorageDigitalSignatureSupport
	ElementCoercion         StorageElementCoercion
}

// StorageSCUExtendedNegotiation returns the PS3.4 B.3.1 defaults a Storage
// SCU states: SCU only, unspecified signature support, SCU only coercion.
func StorageSCUExtendedNegotiation() StorageExtendedNegotiation {
	return StorageExtendedNegotiation{
		LevelOfSupport:          StorageLevelSCUOnly,
		DigitalSignatureSupport: StorageSignatureUnspecified,
		ElementCoercion:         StorageCoercionSCUOnly,
	}
}

// ParseStorageExtendedNegotiation decodes Storage service-class application
// information. A missing field takes its PS3.4 B.3.1 default (SCU only,
// unspecified signature support, SCU only coercion); reserved bytes are not
// validated, bytes after byte 6 are ignored and an undefined field value is an
// error.
func ParseStorageExtendedNegotiation(data []byte) (StorageExtendedNegotiation, error) {
	if len(data) == 0 {
		return StorageExtendedNegotiation{}, fmt.Errorf("dicom dimse: empty Storage extended negotiation")
	}
	n := StorageSCUExtendedNegotiation()
	n.LevelOfSupport = StorageLevelOfSupport(data[0])
	if len(data) > 2 {
		n.DigitalSignatureSupport = StorageDigitalSignatureSupport(data[2])
	}
	if len(data) > 4 {
		n.ElementCoercion = StorageElementCoercion(data[4])
	}
	if err := n.validate(); err != nil {
		return StorageExtendedNegotiation{}, err
	}
	return n, nil
}

func (n StorageExtendedNegotiation) validate() error {
	if n.LevelOfSupport > StorageLevelSCUOnly {
		return fmt.Errorf("dicom dimse: Storage level of support %d", n.LevelOfSupport)
	}
	if n.DigitalSignatureSupport > StorageSignatureLevel3 {
		return fmt.Errorf("dicom dimse: Storage digital signature support %d", n.DigitalSignatureSupport)
	}
	if n.ElementCoercion > StorageCoercionSCUOnly {
		return fmt.Errorf("dicom dimse: Storage element coercion %d", n.ElementCoercion)
	}
	return nil
}

// Bytes encodes the six-byte Storage service-class application information.
func (n StorageExtendedNegotiation) Bytes() []byte {
	return []byte{byte(n.LevelOfSupport), 0, byte(n.DigitalSignatureSupport), 0, byte(n.ElementCoercion), 0}
}

// Item returns the SOP Class Extended Negotiation sub-item stating n for a
// Storage sopClassUID, for ul.DialOptions.ExtendedNegotiation.
func (n StorageExtendedNegotiation) Item(sopClassUID string) ul.SopClassExtendedNegotiationItem {
	return ul.SopClassExtendedNegotiationItem{SopClassUID: sopClassUID, Data: n.Bytes()}
}

// ExtendedNegotiationPolicy is an acceptor's answer to SOP Class Extended
// Negotiation for the Query/Retrieve and Storage service classes. A nil field
// ignores the requests of that service class, so the association proceeds
// without the options (PS3.4: no answer means not supported).
type ExtendedNegotiationPolicy struct {
	// Find lists the C-FIND options the acceptor applies; the answer is the
	// requested subset at the request's length.
	Find *CFindExtendedNegotiation
	// Retrieve lists the C-MOVE/C-GET options the acceptor supports.
	Retrieve *CRetrieveExtendedNegotiation
	// Storage is the acceptor's own Storage capability, returned for every
	// Storage SOP Class that carried a request.
	Storage *StorageExtendedNegotiation
	// StorageSOPClassUIDs adds private or new Storage SOP Classes to the
	// standard ones (the DefaultStorageSOPClassUIDs catalog).
	StorageSOPClassUIDs []string
}

// Handler returns the policy as a ul.AcceptOptions.ExtendedNegotiationHandler.
// Malformed or unrelated sub-items are ignored, never rejected.
func (p ExtendedNegotiationPolicy) Handler() ul.ExtendedNegotiationHandler {
	extraStorage := append([]string(nil), p.StorageSOPClassUIDs...)
	return func(item ul.SopClassExtendedNegotiationItem) (ul.ExtendedNegotiationResult, error) {
		ignore := ul.ExtendedNegotiationResult{Action: ul.ExtendedNegotiationActionIgnore}
		switch {
		case isQueryRetrieveFindSOPClass(item.SopClassUID):
			if p.Find == nil {
				return ignore, nil
			}
			return CFindExtendedNegotiationHandler(*p.Find)(item)
		case isQueryRetrieveRetrieveSOPClass(item.SopClassUID):
			if p.Retrieve == nil {
				return ignore, nil
			}
			requested, err := ParseCRetrieveExtendedNegotiation(item.Data)
			if err != nil {
				return ignore, nil
			}
			data := make([]byte, min(len(item.Data), 2))
			copy(data, requested.intersect(*p.Retrieve).Bytes())
			return ul.ExtendedNegotiationResult{Action: ul.ExtendedNegotiationActionAccept, Data: data}, nil
		case isStorageSOPClass(item.SopClassUID, extraStorage):
			if p.Storage == nil {
				return ignore, nil
			}
			if _, err := ParseStorageExtendedNegotiation(item.Data); err != nil {
				return ignore, nil
			}
			return ul.ExtendedNegotiationResult{Action: ul.ExtendedNegotiationActionAccept, Data: p.Storage.Bytes()}, nil
		default:
			return ignore, nil
		}
	}
}

// AcceptedCRetrieveExtendedNegotiation returns the C-MOVE/C-GET options both
// sides agreed for sopClassUID on assoc; the zero value when nothing was
// negotiated.
func AcceptedCRetrieveExtendedNegotiation(assoc *ul.Association, sopClassUID string) CRetrieveExtendedNegotiation {
	data, ok := acceptedExtendedNegotiationData(assoc, sopClassUID)
	if !ok || !isQueryRetrieveRetrieveSOPClass(sopClassUID) {
		return CRetrieveExtendedNegotiation{}
	}
	negotiated, err := ParseCRetrieveExtendedNegotiation(data)
	if err != nil {
		return CRetrieveExtendedNegotiation{}
	}
	return negotiated
}

// AcceptedStorageExtendedNegotiation returns the acceptor's Storage
// capabilities for sopClassUID on assoc and whether the acceptor answered.
// With no answer the requestor makes no assumption (PS3.4 B.3.1).
func AcceptedStorageExtendedNegotiation(assoc *ul.Association, sopClassUID string) (StorageExtendedNegotiation, bool) {
	data, ok := acceptedExtendedNegotiationData(assoc, sopClassUID)
	if !ok {
		return StorageExtendedNegotiation{}, false
	}
	negotiated, err := ParseStorageExtendedNegotiation(data)
	if err != nil {
		return StorageExtendedNegotiation{}, false
	}
	return negotiated, true
}

func acceptedExtendedNegotiationData(assoc *ul.Association, sopClassUID string) ([]byte, bool) {
	if assoc == nil {
		return nil, false
	}
	for _, item := range assoc.AcceptedExtendedNegotiation {
		if item.SopClassUID == sopClassUID {
			return item.Data, true
		}
	}
	return nil, false
}

// Retired Patient/Study Only Query/Retrieve Information Model MOVE and GET.
const (
	patientStudyOnlyMoveSOPClassUID = "1.2.840.10008.5.1.4.1.2.3.2"
	patientStudyOnlyGetSOPClassUID  = "1.2.840.10008.5.1.4.1.2.3.3"
)

// isQueryRetrieveRetrieveSOPClass reports the C-MOVE/C-GET SOP Classes whose
// extended negotiation uses the PS3.4 C.5.2/C.5.3 layout.
func isQueryRetrieveRetrieveSOPClass(sopClassUID string) bool {
	switch sopClassUID {
	case PatientRootMoveSOPClassUID, StudyRootMoveSOPClassUID, PatientRootGetSOPClassUID, StudyRootGetSOPClassUID,
		patientStudyOnlyMoveSOPClassUID, patientStudyOnlyGetSOPClassUID:
		return true
	default:
		return false
	}
}

func isStorageSOPClass(sopClassUID string, extra []string) bool {
	for _, uid := range extra {
		if uid == sopClassUID {
			return true
		}
	}
	for _, uid := range defaultStorageSOPClassUIDs {
		if uid == sopClassUID {
			return true
		}
	}
	return false
}
