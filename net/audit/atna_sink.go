package audit

import (
	"context"
	"net"
	"strings"
)

// ATNASink adapts in-process Events to ATNA messages logged to Logger
// (#1159). DIMSE operations become legacy envelopes using PS3.15 event IDs:
// C-STORE a DICOM Instances Transferred, C-FIND a Query, C-MOVE and C-GET a
// Begin Transferring DICOM Instances; a rejected or aborted association is
// a Security Alert (Node Authentication). Other events are not audited.
// Events carry no study or patient identity, so messages hold the
// participants and SOP Class only. These telemetry-derived envelopes are not
// specialized clinical profile conformance; use the explicit ClinicalPolicy,
// Build* clinical builders and AssociationAudit API for that separate contract.
type ATNASink struct {
	Logger *SyslogLogger
	Source Source
	// OnError, when set, receives messages that could not be built or
	// spooled.
	OnError func(error)
}

// EmitAuditEvent implements Sink.
func (s ATNASink) EmitAuditEvent(_ context.Context, event Event) {
	message, ok := s.Message(event)
	if !ok || s.Logger == nil {
		return
	}
	if err := s.Logger.Log(message); err != nil && s.OnError != nil {
		s.OnError(err)
	}
}

// Message translates event; ok is false for events that are not audited.
func (s ATNASink) Message(event Event) (Message, bool) {
	common := Common{Time: event.Time, Source: s.Source, Outcome: OutcomeSuccess}
	if event.Kind == OperationFailed || event.Kind == AssociationRejected || event.Kind == AssociationAborted {
		common.Outcome = OutcomeMinorFailure
		common.OutcomeDescription = event.ErrorClass
	}
	local := Participant{UserID: event.LocalAETitle, AlternativeUserID: event.LocalAETitle}
	remote := Participant{UserID: event.RemoteAETitle, AlternativeUserID: event.RemoteAETitle, Host: remoteHost(event.RemoteAddr)}
	if local.UserID == "" {
		local.UserID = s.Source.ID
	}
	if remote.UserID == "" {
		remote.UserID = remote.Host
	}
	switch event.Kind {
	case AssociationRejected, AssociationAborted:
		subject := remote.Host
		if subject == "" {
			subject = remote.UserID
		}
		return SecurityAlert(common, TypeNodeAuthentication, local, subject), local.UserID != ""
	case OperationSucceeded, OperationFailed:
	default:
		return Message{}, false
	}
	if local.UserID == "" || remote.UserID == "" {
		return Message{}, false
	}
	remote.Requestor = true
	switch strings.ToUpper(strings.TrimSpace(event.Service)) {
	case "C-STORE":
		return InstancesTransferred(Transfer{Common: common, Source: remote, Destination: local}, ActionCreate), true
	case "C-FIND":
		return Query(QueryRequest{Common: common, Source: remote, Destination: local, SOPClassUID: event.SOPClassUID}), event.SOPClassUID != ""
	case "C-MOVE", "C-GET":
		return BeginTransferring(Transfer{Common: common, Source: local, Destination: remote}), true
	}
	return Message{}, false
}

func remoteHost(address string) string {
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}
