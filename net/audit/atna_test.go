package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var atnaTime = time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

func atnaMessages() map[string]Message {
	common := Common{Time: atnaTime, Source: Source{ID: "TWIN", SiteID: "SITE"}}
	failed := common
	failed.Outcome, failed.OutcomeDescription = OutcomeSerious, "association rejected"
	scu := Participant{UserID: "STORESCU", AlternativeUserID: "STORESCU", Host: "192.0.2.10"}
	scp := Participant{UserID: "TWIN", AlternativeUserID: "TWIN", Host: "viewer.example.test"}
	study := Study{UID: "1.2.826.0.1.3680043.10.543.1159.1", Accessions: []string{"ACC1"}, SOPClasses: []SOPClass{{UID: "1.2.840.10008.5.1.4.1.1.2", NumberOfInstances: 3}}}
	patient := Patient{ID: "SYNTH-1159", Name: "SYNTH^PATIENT"}
	launcher := Participant{UserID: "operator", UserName: "Operator"}
	return map[string]Message{
		"application start": ApplicationActivity(common, TypeApplicationStart, scp, &launcher),
		"application stop":  ApplicationActivity(common, TypeApplicationStop, scp, nil),
		"begin transferring": BeginTransferring(Transfer{Common: common, Source: scp, Destination: scu,
			Studies: []Study{study}, Patient: patient}),
		"instances transferred": InstancesTransferred(Transfer{Common: common, Source: scu, Destination: scp,
			Studies: []Study{study}, Patient: patient}, ActionCreate),
		"instances accessed": InstancesAccessed(Access{Common: common, User: launcher, Studies: []Study{study}, Patient: patient}, ActionRead),
		"query": Query(QueryRequest{Common: common, Source: scu, Destination: scp, SOPClassUID: "1.2.840.10008.5.1.4.1.2.2.1",
			Query: []byte{0x08, 0x00, 0x52, 0x00}, TransferSyntaxUID: "1.2.840.10008.1.2.1"}),
		"export":         Export(ExportRequest{Common: common, User: launcher, Media: "file:///export/study.zip", Studies: []Study{study}, Patient: patient}),
		"login":          UserAuthentication(common, TypeLogin, launcher, &scp),
		"logout":         UserAuthentication(common, TypeLogout, launcher, nil),
		"security alert": SecurityAlert(failed, TypeNodeAuthentication, scp, "192.0.2.10"),
	}
}

func TestATNAMessagesEncode(t *testing.T) {
	for name, message := range atnaMessages() {
		t.Run(name, func(t *testing.T) {
			data, err := message.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasPrefix(data, []byte(xml.Header)) {
				t.Fatal("no XML declaration")
			}
			var back Message
			if err := xml.Unmarshal(data, &back); err != nil {
				t.Fatal(err)
			}
			if back.Event.ID != message.Event.ID || back.Event.DateTime != "2026-10-07T09:30:00Z" || len(back.Participants) != len(message.Participants) {
				t.Fatalf("round trip = %+v", back.Event)
			}
			for _, participant := range back.Participants {
				if participant.UserID == "" {
					t.Fatal("participant without UserID")
				}
			}
		})
	}
	query, _ := atnaMessages()["query"].Marshal()
	if !bytes.Contains(query, []byte(`<ParticipantObjectQuery>`+base64.StdEncoding.EncodeToString([]byte{0x08, 0x00, 0x52, 0x00}))) {
		t.Fatalf("query data set not base64 encoded:\n%s", query)
	}
	transfer, _ := atnaMessages()["begin transferring"].Marshal()
	for _, want := range []string{`csd-code="110102"`, `codeSystemName="DCM"`, `ParticipantObjectTypeCodeRole="1"`, `NetworkAccessPointTypeCode="2"`, `<Accession Number="ACC1">`} {
		if !bytes.Contains(transfer, []byte(want)) {
			t.Fatalf("begin transferring lacks %s:\n%s", want, transfer)
		}
	}
}

func TestATNAMessageRejectsMissingParts(t *testing.T) {
	for name, message := range map[string]Message{
		"no participant":     {Event: EventIdentification{ID: EventQuery, DateTime: "x", OutcomeIndicator: "0"}, Sources: []AuditSource{{ID: "S"}}},
		"no source":          {Event: EventIdentification{ID: EventQuery, DateTime: "x", OutcomeIndicator: "0"}, Participants: []ActiveParticipant{{UserID: "U"}}},
		"empty export media": Export(ExportRequest{Common: Common{Source: Source{ID: "S"}}, User: Participant{UserID: "U"}}),
	} {
		if _, err := message.Marshal(); !errors.Is(err, ErrAuditMessage) {
			t.Fatalf("%s: error %v", name, err)
		}
	}
}

// TestATNAMessagesValidateAgainstSchema validates every builder's output
// with xmllint against a DICOM audit message XSD given by
// DICOMGO_AUDIT_XSD (the schema is not redistributed here).
func TestATNAMessagesValidateAgainstSchema(t *testing.T) {
	schema := os.Getenv("DICOMGO_AUDIT_XSD")
	if schema == "" {
		t.Skip("set DICOMGO_AUDIT_XSD to a DICOM audit message XSD to validate the messages")
	}
	xmllint, err := exec.LookPath("xmllint")
	if err != nil {
		t.Skip("xmllint not on PATH")
	}
	dir := t.TempDir()
	for name, message := range atnaMessages() {
		data, err := message.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".xml")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(xmllint, "--noout", "--schema", schema, path).CombinedOutput(); err != nil {
			t.Errorf("%s does not validate: %v\n%s\n%s", name, err, out, data)
		}
	}
}
