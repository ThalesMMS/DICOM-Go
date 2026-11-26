package audit

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func clinicalText(v string) bool {
	if !utf8.ValidString(v) || strings.TrimSpace(v) == "" {
		return false
	}
	for _, r := range v {
		if !(r == 9 || r == 10 || r == 13 || r >= 0x20 && r <= 0xd7ff || r >= 0xe000 && r <= 0xfffd || r >= 0x10000 && r <= 0x10ffff) {
			return false
		}
	}
	return true
}
func validCode(c Code) bool {
	return clinicalText(c.Value) && clinicalText(c.CodeSystemName) && clinicalText(c.OriginalText)
}
func roleCount(m Message, code string) int {
	n := 0
	for _, a := range m.Participants {
		for _, r := range a.RoleIDCodes {
			if r.Value == code && r.CodeSystemName == "DCM" {
				n++
			}
		}
	}
	return n
}

// ValidateClinical checks supported PS3.15 2026d A.5.2/A.5.3 profiles in
// addition to structural encoding. It refuses unknown profiles rather than
// treating a generic schema-valid message as specialized conformance.
func (m Message) ValidateClinical(policy ClinicalPolicy) error {
	p, err := policy.Normalize()
	if err != nil {
		return err
	}
	if len(m.Event.TypeCodes) > 8 || len(m.Sources) > 1 {
		return clinicalError("limit", "Codes")
	}
	if !validCode(m.Event.ID) || m.Event.ID.CodeSystemName != "DCM" {
		return clinicalError("code", "Event.ID")
	}
	if _, err := time.Parse(time.RFC3339Nano, m.Event.DateTime); err != nil {
		return clinicalError("time", "Event.DateTime")
	}
	if !strings.HasSuffix(m.Event.DateTime, "Z") && !strings.Contains(m.Event.DateTime[10:], "+") && !strings.Contains(m.Event.DateTime[10:], "-") {
		return clinicalError("time", "Event.DateTime")
	}
	switch m.Event.OutcomeIndicator {
	case OutcomeSuccess, OutcomeMinorFailure, OutcomeSerious, OutcomeMajorFailure:
	default:
		return clinicalError("outcome", "Event.Outcome")
	}
	if len(m.Sources) != 1 || !clinicalText(m.Sources[0].ID) {
		return clinicalError("cardinality", "Sources")
	}
	if len(m.Participants) < 1 || len(m.Participants) > 8 {
		return clinicalError("cardinality", "Participants")
	}
	if len(m.Objects) > p.Limits.MaxObjects {
		return clinicalError("limit", "Objects")
	}
	if len(m.Sources[0].TypeCodes) > 8 {
		return clinicalError("limit", "Sources.TypeCodes")
	}
	for _, c := range m.Event.TypeCodes {
		if !validCode(c) {
			return clinicalError("code", "Event.Types")
		}
	}
	for _, c := range m.Sources[0].TypeCodes {
		if c.Value == "" || !clinicalEnum(c.Value, 9) && !validCode(c) {
			return clinicalError("code", "Sources.TypeCodes")
		}
	}
	for _, c := range append(append([]Code{m.Event.ID}, m.Event.TypeCodes...), m.Sources[0].TypeCodes...) {
		if len(c.Value)+len(c.CodeSystemName)+len(c.OriginalText) > p.Limits.MaxIdentityBytes {
			return clinicalError("limit", "Codes")
		}
	}
	if m.Event.OutcomeDescription != "" && !clinicalText(m.Event.OutcomeDescription) {
		return clinicalError("text", "Event.OutcomeDescription")
	}
	if m.Sources[0].EnterpriseSiteID != "" && !clinicalText(m.Sources[0].EnterpriseSiteID) {
		return clinicalError("text", "Sources.Site")
	}
	identityBytes := len(m.Event.OutcomeDescription) + len(m.Sources[0].ID) + len(m.Sources[0].EnterpriseSiteID)
	requestors := 0
	for i, a := range m.Participants {
		field := "Participants[" + strconv.Itoa(i) + "]"
		if len(a.RoleIDCodes) > 8 {
			return clinicalError("limit", field+".Roles")
		}
		if !clinicalText(a.UserID) {
			return clinicalError("required", field+".UserID")
		}
		if a.UserIsRequestor {
			requestors++
		}
		if (a.NetworkAccessPointID == "") != (a.NetworkAccessPointTypeCode == "") {
			return clinicalError("network", field)
		}
		if a.NetworkAccessPointTypeCode != "" && (len(a.NetworkAccessPointTypeCode) != 1 || !strings.Contains("12345", a.NetworkAccessPointTypeCode)) {
			return clinicalError("network", field)
		}
		for _, v := range a.RoleIDCodes {
			if !validCode(v) || len(v.Value)+len(v.CodeSystemName)+len(v.OriginalText) > p.Limits.MaxIdentityBytes {
				return clinicalError("code", field+".Roles")
			}
		}
		if a.MediaIdentifier != nil && (!validCode(a.MediaIdentifier.Type) || len(a.MediaIdentifier.Type.Value)+len(a.MediaIdentifier.Type.CodeSystemName)+len(a.MediaIdentifier.Type.OriginalText) > p.Limits.MaxIdentityBytes) {
			return clinicalError("code", field+".MediaType")
		}
		for _, v := range []string{a.UserID, a.AlternativeUserID, a.UserName, a.NetworkAccessPointID} {
			if v != "" && !clinicalText(v) {
				return clinicalError("text", field)
			}
			identityBytes += len(v)
		}
	}
	if requestors > 1 {
		return clinicalError("requestor", "Participants")
	}
	patients, studies, queries := 0, 0, 0
	for i, o := range m.Objects {
		field := "Objects[" + strconv.Itoa(i) + "]"
		if !clinicalText(o.ID) || !validCode(o.IDTypeCode) || len(o.IDTypeCode.Value)+len(o.IDTypeCode.CodeSystemName)+len(o.IDTypeCode.OriginalText) > p.Limits.MaxIdentityBytes {
			return clinicalError("required", field+".ID")
		}
		if (o.Name == "") == (o.Query == "") {
			return clinicalError("name_or_query", field)
		}
		if o.Name != "" && !clinicalText(o.Name) {
			return clinicalError("text", field+".Name")
		}
		identityBytes += len(o.ID) + len(o.Name)
		if o.Query != "" {
			if !p.IncludeQuery {
				return clinicalError("opt_in", field+".Query")
			}
			if len(o.Query) > base64.StdEncoding.EncodedLen(p.Limits.MaxQueryBytes) {
				return clinicalError("limit", field+".Query")
			}
			raw, err := base64.StdEncoding.DecodeString(o.Query)
			if err != nil || len(raw) == 0 {
				return clinicalError("query", field+".Query")
			}
			if len(raw) > p.Limits.MaxQueryBytes {
				return clinicalError("limit", field+".Query")
			}
		}
		if !clinicalEnum(o.TypeCode, 4) || !clinicalEnum(o.Role, 26) || !clinicalEnum(o.LifeCycle, 15) {
			return clinicalError("enum", field)
		}
		if len(o.Details) > 64 || len(o.Description.Accessions) > p.Limits.MaxObjects || len(o.Description.ContainsStudy.Studies) > p.Limits.MaxGroups {
			return clinicalError("limit", field+".Description")
		}
		for _, v := range o.Description.ContainsStudy.Studies {
			if !clinicalUID(v.UID) {
				return clinicalError("uid", field+".ContainsStudy")
			}
			identityBytes += len(v.UID)
		}
		for _, d := range o.Details {
			if len(d.Value) > base64.StdEncoding.EncodedLen(p.Limits.MaxIdentityBytes) {
				return clinicalError("limit", field+".Details")
			}
			raw, err := base64.StdEncoding.DecodeString(d.Value)
			if !clinicalText(d.Type) || err != nil || len(raw) == 0 {
				return clinicalError("detail", field+".Details")
			}
			identityBytes += len(d.Type) + len(raw)
		}
		for _, a := range o.Description.Accessions {
			if !clinicalText(a.Number) {
				return clinicalError("required", field+".Accession")
			}
			identityBytes += len(a.Number)
		}
		if len(o.Description.SOPClasses) > p.Limits.MaxClasses {
			return clinicalError("limit", field+".SOPClasses")
		}
		for _, c := range o.Description.SOPClasses {
			if !clinicalUID(c.UID) || c.NumberOfInstances <= 0 || c.NumberOfInstances > p.Limits.MaxInstances || len(c.Instances) > c.NumberOfInstances {
				return clinicalError("instances", field+".SOPClasses")
			}
			identityBytes += len(c.UID)
			for _, v := range c.Instances {
				if !clinicalUID(v.UID) {
					return clinicalError("uid", field+".Instances")
				}
				identityBytes += len(v.UID)
			}
		}
		if o.Role == "1" {
			patients++
			if !p.IncludeIdentities {
				return clinicalError("opt_in", field)
			}
			if o.TypeCode != "1" || o.IDTypeCode.Value != "2" || o.IDTypeCode.CodeSystemName != "RFC-3881" || o.Name == "" {
				return clinicalError("patient", field)
			}
		}
		if o.IDTypeCode.Value == idStudyInstanceUID.Value && o.IDTypeCode.CodeSystemName == "DCM" {
			studies++
			if !p.IncludeIdentities {
				return clinicalError("opt_in", field)
			}
			if !clinicalUID(o.ID) || o.TypeCode != "2" || o.Role != "3" || len(o.Description.ContainsStudy.Studies) > 0 {
				return clinicalError("study", field)
			}
			if (len(o.Description.Accessions) > 0 || o.Description.Encrypted != nil || o.Description.Anonymized != nil) && len(o.Description.SOPClasses) == 0 {
				return clinicalError("sop_classes", field)
			}
		}
		if o.Query != "" && o.IDTypeCode.Value == idSOPClassUID.Value {
			queries++
			if o.TypeCode != "2" || o.Role != "3" || o.IDTypeCode.CodeSystemName != "DCM" {
				return clinicalError("query", field)
			}
			syntax := ""
			for _, d := range o.Details {
				if d.Type == "TransferSyntax" {
					raw, _ := base64.StdEncoding.DecodeString(d.Value)
					if syntax != "" {
						return clinicalError("query", field)
					}
					syntax = string(raw)
				}
			}
			raw, _ := base64.StdEncoding.DecodeString(o.Query)
			if !clinicalUID(o.ID) {
				return clinicalError("uid", field+".ID")
			}
			if err := validateQueryData(p, raw, syntax); err != nil {
				return err
			}
		}
	}
	if identityBytes > p.Limits.MaxIdentityBytes {
		return clinicalError("limit", "Identity")
	}
	id, action := m.Event.ID.Value, m.Event.ActionCode
	actionIn := func(allowed string) bool { return len(action) == 1 && strings.Contains(allowed, action) }
	record := func() error {
		if patients != 1 || len(m.Participants) > 2 || len(m.Objects) != patients+studies {
			return clinicalError("cardinality", "Record")
		}
		return nil
	}
	switch id {
	case "110102", "110104":
		if patients != 1 || studies < 1 || len(m.Objects) != patients+studies || roleCount(m, RoleSource.Value) != 1 || roleCount(m, RoleDestination.Value) != 1 || len(m.Participants) > 3 {
			return clinicalError("cardinality", "Transfer")
		}
		if id == "110102" && !actionIn("E") || id == "110104" && !actionIn("CRU") {
			return clinicalError("action", "Event.Action")
		}
	case "110103", "110105":
		if err := record(); err != nil {
			return err
		}
		if studies < 1 {
			return clinicalError("cardinality", "Studies")
		}
		if id == "110103" && !actionIn("CRUD") || id == "110105" && !actionIn("D") {
			return clinicalError("action", "Event.Action")
		}
	case "110109", "110110", "110111":
		if err := record(); err != nil {
			return err
		}
		if !actionIn("CRUD") {
			return clinicalError("action", "Event.Action")
		}
		if id != "110111" && studies != 0 {
			return clinicalError("cardinality", "Studies")
		}
	case "110107", "110106":
		role, party, allowed := RoleSourceMedia.Value, RoleDestination.Value, "C"
		if id == "110106" {
			role, party, allowed = RoleDestinationMedia.Value, RoleSource.Value, "R"
		}
		if patients < 1 || roleCount(m, role) != 1 || roleCount(m, party) < 1 || requestors != 1 {
			return clinicalError("cardinality", "Media")
		}
		if !actionIn(allowed) {
			return clinicalError("action", "Event.Action")
		}
		for _, a := range m.Participants {
			for _, r := range a.RoleIDCodes {
				if r.Value == role && (a.MediaIdentifier == nil || a.UserIsRequestor) {
					return clinicalError("media", "Participants")
				}
			}
		}
	case "110112":
		if !p.IncludeQuery {
			return clinicalError("opt_in", "Query")
		}
		if queries != 1 || len(m.Objects) != 1 || len(m.Participants) != 2 || roleCount(m, RoleSource.Value) != 1 || roleCount(m, RoleDestination.Value) != 1 || requestors != 1 {
			return clinicalError("cardinality", "Query")
		}
		if !actionIn("E") {
			return clinicalError("action", "Event.Action")
		}
	case "110101":
		if len(m.Objects) != 1 || len(m.Participants) > 2 || !actionIn("R") {
			return clinicalError("cardinality", "AuditLog")
		}
		o := m.Objects[0]
		u, err := url.Parse(o.ID)
		if err != nil || u.Scheme == "" || o.Name != "Security Audit Log" || o.IDTypeCode.Value != "12" || o.TypeCode != "2" || o.Role != "13" {
			return clinicalError("audit_log", "Objects[0]")
		}
	default:
		return clinicalError("profile", "Event.ID")
	}
	return nil
}

func clinicalEnum(v string, max int) bool {
	if v == "" {
		return true
	}
	n, err := strconv.Atoi(v)
	return err == nil && n >= 1 && n <= max && strconv.Itoa(n) == v
}
