package vendors

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
	"github.com/ThalesMMS/dicom-go/vendors/headers"
)

// PrivateHeaderKind names a vendor private header that DecodePrivateHeaders
// can render, the set gdcmdump prints with --csa, --mrprotocol, --pdb,
// --elscint and --vepro, plus the Siemens MedCom History.
type PrivateHeaderKind string

const (
	// SiemensCSAImage is CSA Image Header Info (0029,xx10).
	SiemensCSAImage PrivateHeaderKind = "siemens-csa-image"
	// SiemensCSASeries is CSA Series Header Info (0029,xx20).
	SiemensCSASeries PrivateHeaderKind = "siemens-csa-series"
	// SiemensMRProtocol is the ASCCONV MR protocol inside the CSA series
	// header (MrPhoenixProtocol or MrProtocol).
	SiemensMRProtocol PrivateHeaderKind = "siemens-mr-protocol"
	// SiemensMedComHistory is MedCom History Information (0029,xx20) of
	// "SIEMENS MEDCOM HEADER"; it records former patient names and IDs.
	SiemensMedComHistory PrivateHeaderKind = "siemens-medcom-history"
	// GEProtocolDataBlockHeader is the GE Protocol Data Block (0025,xx1B).
	GEProtocolDataBlockHeader PrivateHeaderKind = "ge-protocol-data-block"
	// ElscintProtocolHeader is the ELSCINT1 protocol information
	// (01F7,xx26).
	ElscintProtocolHeader PrivateHeaderKind = "elscint-protocol"
	// VEPROHeader is the VEPRO VIF/VIM data (0055,xx20); its data fields
	// often hold patient demographics.
	VEPROHeader PrivateHeaderKind = "vepro-data"
	// PhilipsSDSHeader is Philips Protocol Data (2005,xx32).
	PhilipsSDSHeader PrivateHeaderKind = "philips-sds"
	// ToshibaCT3Header is CT Private Data 2 (7005,xx10).
	ToshibaCT3Header PrivateHeaderKind = "toshiba-ct3"
	// ToshibaMECMR3Header is Original Data (700d,xx08).
	ToshibaMECMR3Header PrivateHeaderKind = "toshiba-mec-mr3"
	// PMTFHeader is PMTF INFORMATION DATA (0029,xx01).
	PMTFHeader PrivateHeaderKind = "pmtf"
	// CanonPMTFHeader is CANON_MEC_MR3 (0029,xx01).
	CanonPMTFHeader PrivateHeaderKind = "canon-pmtf"
	// ToshibaPMTFHeader is TOSHIBA_MEC_MR3 (0029,xx01/xx02).
	ToshibaPMTFHeader PrivateHeaderKind = "toshiba-pmtf"
	// CanonPASHeader is PAS Reproduct Information (700d,xx19).
	CanonPASHeader PrivateHeaderKind = "canon-pas"
)

// PrivateHeaderInfo describes a kind of private header.
type PrivateHeaderInfo struct {
	Kind  PrivateHeaderKind
	Title string
	// MayIdentify marks headers that usually carry personal data (patient
	// names, identifiers, demographics), so callers warn before decoding.
	MayIdentify bool
	group       uint16
	creators    []string
	offset      uint8
}

// PrivateHeaderKinds lists every kind in decoding order.
var PrivateHeaderKinds = append([]PrivateHeaderInfo{
	{Kind: SiemensCSAImage, Title: "Siemens CSA Image Header", group: 0x0029, creators: []string{"SIEMENS CSA HEADER"}, offset: 0x10},
	{Kind: SiemensCSASeries, Title: "Siemens CSA Series Header", group: 0x0029, creators: []string{"SIEMENS CSA HEADER"}, offset: 0x20},
	{Kind: SiemensMRProtocol, Title: "Siemens MR Protocol (ASCCONV)", group: 0x0029, creators: []string{"SIEMENS CSA HEADER"}, offset: 0x20},
	{Kind: SiemensMedComHistory, Title: "Siemens MedCom History", MayIdentify: true, group: 0x0029, creators: []string{"SIEMENS MEDCOM HEADER"}, offset: 0x20},
	{Kind: GEProtocolDataBlockHeader, Title: "GE Protocol Data Block", group: 0x0025, creators: []string{"GEMS_SERS_01"}, offset: 0x1B},
	{Kind: ElscintProtocolHeader, Title: "ELSCINT1 Protocol Information", group: 0x01F7, creators: []string{"ELSCINT1"}, offset: 0x26},
	{Kind: VEPROHeader, Title: "VEPRO Data", MayIdentify: true, group: 0x0055, creators: []string{"VEPRO VIF 3.0 DATA", "VEPRO VIM 5.0 DATA"}, offset: 0x20},
}, additionalHeaderInfos()...)

func additionalHeaderInfos() []PrivateHeaderInfo {
	definitions := headers.Definitions()
	infos := make([]PrivateHeaderInfo, len(definitions))
	for i, def := range definitions {
		infos[i] = PrivateHeaderInfo{Kind: PrivateHeaderKind(def.Kind), Title: def.Title, MayIdentify: true, group: def.Group, creators: []string{def.Creator}, offset: def.Offset}
	}
	return infos
}

// PrivateHeaderInfoOf returns the description of kind.
func PrivateHeaderInfoOf(kind PrivateHeaderKind) (PrivateHeaderInfo, bool) {
	for _, info := range PrivateHeaderKinds {
		if info.Kind == kind {
			return info, true
		}
	}
	return PrivateHeaderInfo{}, false
}

// PrivateHeaderLocation is a private header present in a data set.
type PrivateHeaderLocation struct {
	PrivateHeaderInfo
	Tag core.Tag
}

// DetectPrivateHeaders lists the private headers present in dataset, located
// through their private creators, without decoding them, so a caller can
// warn about the headers that may identify the patient before decoding. The
// MR protocol is listed with the CSA series header that holds it; whether it
// has an ASCCONV block is known only after decoding.
func DetectPrivateHeaders(dataset *object.Object) []PrivateHeaderLocation {
	if dataset == nil {
		return nil
	}
	var out []PrivateHeaderLocation
	for _, info := range PrivateHeaderKinds {
		for _, creator := range info.creators {
			tag, err := dataset.PrivateTag(info.group, creator, info.offset)
			if err == nil && dataset.Has(tag) {
				out = append(out, PrivateHeaderLocation{PrivateHeaderInfo: info, Tag: tag})
				break
			}
		}
	}
	return out
}

// PrivateHeaderOptions bound DecodePrivateHeaders.
type PrivateHeaderOptions struct {
	// Kinds selects the headers to decode; empty decodes every header
	// present.
	Kinds []PrivateHeaderKind
	// Limits bound the blob parsers (see Limits).
	Limits Limits
	// MaxLines bounds the rendered lines of one header (default 20000);
	// further lines are dropped and Truncated is set.
	MaxLines int
}

// PrivateHeader is one decoded private header rendered as text lines.
type PrivateHeader struct {
	PrivateHeaderLocation
	Lines     []string
	Truncated bool
	// Err reports a header that is present but could not be decoded
	// (wrapping ErrInvalid, ErrLimit or the siemens package errors); Lines
	// is then empty.
	Err error
}

// DecodePrivateHeaders decodes the private headers present in dataset (see
// DetectPrivateHeaders) and renders each as text lines in the manner of
// gdcmdump: CSA elements as "Name VR VM [values]", the MR protocol and the
// GE Protocol Data Block as "key = value", MedCom History by section,
// ELSCINT1 by group and item, and VEPRO fields by name. The headers are
// private and VEPRO and MedCom data hold patient names (MayIdentify), so
// callers decode only on an explicit request.
func DecodePrivateHeaders(dataset *object.Object, opts PrivateHeaderOptions) []PrivateHeader {
	if opts.MaxLines <= 0 {
		opts.MaxLines = 20000
	}
	selected := map[PrivateHeaderKind]bool{}
	for _, kind := range opts.Kinds {
		selected[kind] = true
	}
	var out []PrivateHeader
	for _, location := range DetectPrivateHeaders(dataset) {
		if len(selected) > 0 && !selected[location.Kind] {
			continue
		}
		lines, err := renderPrivateHeader(dataset, location, opts.Limits)
		if location.Kind == SiemensMRProtocol && errors.Is(err, siemens.ErrNoASCCONV) {
			continue // a series header without a protocol, not an error
		}
		header := PrivateHeader{PrivateHeaderLocation: location, Err: err}
		if err == nil {
			if len(lines) > opts.MaxLines {
				lines, header.Truncated = lines[:opts.MaxLines], true
			}
			header.Lines = lines
		}
		out = append(out, header)
	}
	return out
}

func renderPrivateHeader(dataset *object.Object, location PrivateHeaderLocation, limits Limits) ([]string, error) {
	limits = limits.normalized()
	siemensOptions := siemens.LookupOptions{Limits: siemens.CSALimits{MaxBytes: limits.MaxBytes}}
	switch location.Kind {
	case PhilipsSDSHeader, ToshibaCT3Header, ToshibaMECMR3Header, PMTFHeader, CanonPMTFHeader, ToshibaPMTFHeader, CanonPASHeader:
		return headers.RenderPrivateHeader(dataset, headers.Kind(location.Kind), location.Tag, headers.Limits(limits))
	case SiemensCSAImage:
		header, err := siemens.ImageHeader(dataset, siemensOptions)
		if err != nil {
			return nil, err
		}
		return csaLines(header.CSAHeader), nil
	case SiemensCSASeries:
		header, err := siemens.SeriesHeader(dataset, siemensOptions)
		if err != nil {
			return nil, err
		}
		return csaLines(header.CSAHeader), nil
	case SiemensMRProtocol:
		protocol, err := siemens.SeriesProtocol(dataset, siemensOptions)
		if err != nil {
			return nil, err
		}
		lines := make([]string, 0, protocol.Len())
		for _, entry := range protocol.Entries() {
			lines = append(lines, entry.Key+" = "+entry.Value)
		}
		return lines, nil
	case SiemensMedComHistory:
		history, err := siemens.MedComHistoryOf(dataset, siemens.MedComLimits{MaxBytes: limits.MaxBytes, MaxLines: limits.MaxEntries})
		if err != nil {
			return nil, err
		}
		lines := []string{"Version: " + history.Version}
		for _, section := range history.Sections {
			lines = append(lines, section.Name)
			for _, record := range section.Records {
				lines = append(lines, "  "+strings.Join(record, " | "))
			}
		}
		return lines, nil
	case GEProtocolDataBlockHeader:
		block, err := GEProtocolDataBlockOf(dataset, limits)
		if err != nil {
			return nil, err
		}
		var lines []string
		if block.Truncated {
			lines = append(lines, "(gzip stream ends before its trailer; checksum not verified)")
		}
		if block.XML || len(block.Entries) == 0 {
			return append(lines, textLines(block.Text)...), nil
		}
		for _, entry := range block.Entries {
			lines = append(lines, entry.Key+" = "+entry.Value)
		}
		return lines, nil
	case ElscintProtocolHeader:
		protocol, err := ElscintProtocolOf(dataset, limits)
		if err != nil {
			return nil, err
		}
		if len(protocol.Groups) == 0 {
			return textLines(protocol.Text), nil
		}
		var lines []string
		for _, group := range protocol.Groups {
			for i, item := range group.Items {
				name := group.Name
				if group.Multiple {
					name = fmt.Sprintf("%s[%d]", group.Name, i)
				}
				if len(item) == 0 {
					lines = append(lines, name+": (empty)")
				}
				for _, entry := range item {
					lines = append(lines, name+": "+entry.Key+" = "+strings.Join(entry.Values, `\`))
				}
			}
		}
		return lines, nil
	case VEPROHeader:
		data, err := VEPRODataOf(dataset, limits)
		if err != nil {
			return nil, err
		}
		lines := []string{
			"ID: " + data.ID, "Version: " + data.Version, "User Name: " + data.UserName,
		}
		for i, address := range data.UserAddress {
			if address != "" {
				lines = append(lines, fmt.Sprintf("User Address %d: %s", i+1, address))
			}
		}
		lines = append(lines,
			"Recording Date: "+data.RecordingDate, "Recording Time: "+data.RecordingTime,
			"Recording Place: "+data.RecordingPlace, "Recording Source: "+data.RecordingSource)
		for i, field := range data.DataFields {
			if field != "" {
				lines = append(lines, fmt.Sprintf("Data Field %d: %s", i+1, field))
			}
		}
		return append(lines, "Study Instance UID: "+data.StudyInstanceUID, "Series UID: "+data.SeriesUID, "Modality: "+data.Modality), nil
	}
	return nil, fmt.Errorf("%w: unknown private header %q", ErrNotFound, location.Kind)
}

// csaLines renders a CSA header: named elements for SV10/NOSV, the embedded
// attributes for the DATASET layout and the text of INTERFILE/XML headers.
func csaLines(header *siemens.CSAHeader) []string {
	if header == nil {
		return nil
	}
	lines := []string{"Format: " + string(header.Format)}
	switch {
	case header.Format == siemens.CSAFormatZeroed:
		return append(lines, "(all bytes zero)")
	case header.Dataset != nil:
		for _, element := range header.Dataset.Elements() {
			value := fmt.Sprintf("(%d bytes)", element.Length())
			if values, ok := header.Dataset.GetStrings(element.Tag()); ok {
				value = "[" + strings.Join(values, `\`) + "]"
			}
			lines = append(lines, fmt.Sprintf("%s %s %s", element.Tag(), element.VR(), value))
		}
		return lines
	case header.Text != "":
		return append(lines, textLines(header.Text)...)
	}
	for _, element := range header.Elements {
		lines = append(lines, fmt.Sprintf("%s %s %d [%s]", element.Name, element.VR, element.VM, strings.Join(element.Strings(), `\`)))
	}
	return lines
}

func textLines(text string) []string {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n\x00 ")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}
