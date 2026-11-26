package dimse

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// CFindExtendedNegotiation is the Query/Retrieve C-FIND service-class
// application information of an SOP Class Extended Negotiation sub-item
// (PS3.4 C.5.1.1.1 and C.5.1.1.2): byte 1 relational queries, byte 2
// combined date-time matching, byte 3 fuzzy semantic matching of person names,
// byte 4 timezone query adjustment, byte 5 Enhanced Multi-Frame Image
// Conversion, byte 6 empty value matching and byte 7 multiple value matching.
// A requestor sets the options it wants; an acceptor answers with the subset
// it will apply.
type CFindExtendedNegotiation struct {
	RelationalQueries                 bool
	CombinedDateTimeMatching          bool
	FuzzySemanticMatching             bool
	TimezoneQueryAdjustment           bool
	EnhancedMultiFrameImageConversion bool
	EmptyValueMatching                bool
	MultipleValueMatching             bool
}

// ParseCFindExtendedNegotiation decodes C-FIND service-class application
// information. Bytes 2–7 are optional and absent bytes mean "not requested"
// (PS3.4 C.5.1.1.2: a one-byte answer leaves the other sub-fields 0); bytes
// after byte 7 are ignored. A byte other than 1 is read as 0.
func ParseCFindExtendedNegotiation(data []byte) (CFindExtendedNegotiation, error) {
	if len(data) == 0 {
		return CFindExtendedNegotiation{}, fmt.Errorf("dicom dimse: empty C-FIND extended negotiation")
	}
	flag := func(index int) bool { return index < len(data) && data[index] == 1 }
	return CFindExtendedNegotiation{
		RelationalQueries:        flag(0),
		CombinedDateTimeMatching: flag(1),
		FuzzySemanticMatching:    flag(2),
		TimezoneQueryAdjustment:  flag(3),

		EnhancedMultiFrameImageConversion: flag(4),
		EmptyValueMatching:                flag(5),
		MultipleValueMatching:             flag(6),
	}, nil
}

// Bytes encodes the options through the last one set, at least one byte.
func (n CFindExtendedNegotiation) Bytes() []byte {
	return encodeExtendedNegotiationFlags(n.flags())
}

func (n CFindExtendedNegotiation) flags() []bool {
	return []bool{
		n.RelationalQueries, n.CombinedDateTimeMatching, n.FuzzySemanticMatching, n.TimezoneQueryAdjustment,
		n.EnhancedMultiFrameImageConversion, n.EmptyValueMatching, n.MultipleValueMatching,
	}
}

// encodeExtendedNegotiationFlags writes one 0/1 byte per flag through the last
// flag set, at least one byte.
func encodeExtendedNegotiationFlags(flags []bool) []byte {
	length := 1
	for index, set := range flags {
		if set {
			length = index + 1
		}
	}
	data := make([]byte, length)
	for index := 0; index < length; index++ {
		if flags[index] {
			data[index] = 1
		}
	}
	return data
}

// Item returns the SOP Class Extended Negotiation sub-item that requests n
// for sopClassUID, for ul.DialOptions.ExtendedNegotiation.
func (n CFindExtendedNegotiation) Item(sopClassUID string) ul.SopClassExtendedNegotiationItem {
	return ul.SopClassExtendedNegotiationItem{SopClassUID: sopClassUID, Data: n.Bytes()}
}

func (n CFindExtendedNegotiation) intersect(supported CFindExtendedNegotiation) CFindExtendedNegotiation {
	return CFindExtendedNegotiation{
		RelationalQueries:        n.RelationalQueries && supported.RelationalQueries,
		CombinedDateTimeMatching: n.CombinedDateTimeMatching && supported.CombinedDateTimeMatching,
		FuzzySemanticMatching:    n.FuzzySemanticMatching && supported.FuzzySemanticMatching,
		TimezoneQueryAdjustment:  n.TimezoneQueryAdjustment && supported.TimezoneQueryAdjustment,

		EnhancedMultiFrameImageConversion: n.EnhancedMultiFrameImageConversion && supported.EnhancedMultiFrameImageConversion,
		EmptyValueMatching:                n.EmptyValueMatching && supported.EmptyValueMatching,
		MultipleValueMatching:             n.MultipleValueMatching && supported.MultipleValueMatching,
	}
}

// isQueryRetrieveFindSOPClass reports the C-FIND SOP Classes whose
// extended negotiation uses the PS3.4 C.5.1.1 layout.
func isQueryRetrieveFindSOPClass(sopClassUID string) bool {
	switch sopClassUID {
	case PatientRootFindSOPClassUID, StudyRootFindSOPClassUID, patientStudyOnlyFindSOPClassUID:
		return true
	default:
		return false
	}
}

// patientStudyOnlyFindSOPClassUID is the retired Patient/Study Only
// Query/Retrieve Information Model - FIND SOP Class.
const patientStudyOnlyFindSOPClassUID = "1.2.840.10008.5.1.4.1.2.3.1"

// CFindExtendedNegotiationHandler returns an acceptor policy for
// ul.AcceptOptions.ExtendedNegotiationHandler. For a Query/Retrieve C-FIND
// SOP Class it answers with the requested options that are also in supported,
// keeping the request's length (PS3.4 C.5.1.1.2); a malformed or unrelated
// sub-item is ignored, so the association proceeds without the options.
func CFindExtendedNegotiationHandler(supported CFindExtendedNegotiation) ul.ExtendedNegotiationHandler {
	return func(item ul.SopClassExtendedNegotiationItem) (ul.ExtendedNegotiationResult, error) {
		if !isQueryRetrieveFindSOPClass(item.SopClassUID) {
			return ul.ExtendedNegotiationResult{Action: ul.ExtendedNegotiationActionIgnore}, nil
		}
		requested, err := ParseCFindExtendedNegotiation(item.Data)
		if err != nil {
			return ul.ExtendedNegotiationResult{Action: ul.ExtendedNegotiationActionIgnore}, nil
		}
		accepted := requested.intersect(supported).Bytes()
		data := make([]byte, len(item.Data))
		copy(data, accepted)
		return ul.ExtendedNegotiationResult{Action: ul.ExtendedNegotiationActionAccept, Data: data}, nil
	}
}

// AcceptedCFindExtendedNegotiation returns the C-FIND options both sides
// agreed for sopClassUID on assoc: the acceptor's answer, read from
// AcceptedExtendedNegotiation on either side of the association. It is the
// zero value when nothing was negotiated.
func AcceptedCFindExtendedNegotiation(assoc *ul.Association, sopClassUID string) CFindExtendedNegotiation {
	if assoc == nil || !isQueryRetrieveFindSOPClass(sopClassUID) {
		return CFindExtendedNegotiation{}
	}
	for _, item := range assoc.AcceptedExtendedNegotiation {
		if item.SopClassUID != sopClassUID {
			continue
		}
		negotiated, err := ParseCFindExtendedNegotiation(item.Data)
		if err != nil {
			return CFindExtendedNegotiation{}
		}
		return negotiated
	}
	return CFindExtendedNegotiation{}
}
