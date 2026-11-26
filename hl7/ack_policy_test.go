package hl7

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReadAckPolicyWireMatrix(t *testing.T) {
	conditions := []struct {
		name  string
		value string
		want  AckCondition
		send  [3]bool
	}{
		{name: "absent"},
		{name: "null", value: Null},
		{name: "always", value: "AL", want: AckAlways, send: [3]bool{true, true, true}},
		{name: "never", value: "NE", want: AckNever},
		{name: "errors", value: "ER", want: AckOnError, send: [3]bool{false, true, true}},
		{name: "success", value: "SU", want: AckOnSuccess, send: [3]bool{true, false, false}},
	}
	commitCodes := []AckCode{AckCommitAccept, AckCommitError, AckCommitReject}
	applicationCodes := []AckCode{AckAccept, AckError, AckReject}
	const header = "MSH|^~\\&|SRC|SF|DST|DF|||ADT^A08|INPUT|P|2.5.1"
	for _, accept := range conditions {
		for _, application := range conditions {
			t.Run(accept.name+"/"+application.name, func(t *testing.T) {
				wire := header
				if accept.value != "" || application.value != "" {
					wire += "|||" + accept.value + "|" + application.value
				}
				message, err := Parse([]byte(wire + "\rPID|1\r"))
				if err != nil {
					t.Fatal(err)
				}
				policy, err := ReadAckPolicy(message)
				want := AckPolicy{Accept: accept.want, Application: application.want}
				enhanced := accept.want != "" || application.want != ""
				if err != nil || policy != want || policy.Enhanced() != enhanced {
					t.Fatalf("policy = %+v, err = %v, want %+v", policy, err, want)
				}
				for i, code := range commitCodes {
					if policy.Wants(code) != accept.send[i] {
						t.Errorf("Wants(%s) = %t, want %t", code, policy.Wants(code), accept.send[i])
					}
				}
				for i, code := range applicationCodes {
					wantSend := !enhanced || application.send[i]
					if policy.Wants(code) != wantSend {
						t.Errorf("Wants(%s) = %t, want %t", code, policy.Wants(code), wantSend)
					}
				}
				if policy.Wants("XX") {
					t.Error("unknown code requested")
				}
			})
		}
	}
}

func TestReadAckPolicyRejectsInvalidFields(t *testing.T) {
	const header = "MSH|^~\\&|SRC|SF|DST|DF|||ADT^A08|INPUT|P|2.5.1|||"
	for _, test := range []struct {
		name   string
		fields string
		field  int
	}{
		{name: "unknown", fields: "SECRET|AL", field: 15},
		{name: "lowercase", fields: "al|NE", field: 15},
		{name: "space", fields: "NE|SU ", field: 16},
		{name: "repeated", fields: "AL~SU|", field: 15},
		{name: "composite", fields: "AL|SU^", field: 16},
		{name: "subcomponent", fields: "AL|SU&ER", field: 16},
		{name: "empty composite", fields: "^|NE", field: 15},
	} {
		t.Run(test.name, func(t *testing.T) {
			message, err := Parse([]byte(header + test.fields + "\r"))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := ReadAckPolicy(message)
			var detail *Error
			if policy != (AckPolicy{}) || !errors.As(err, &detail) ||
				detail.Code != ErrorCodeTableValueNotFound ||
				detail.Location != (ErrorLocation{Segment: "MSH", Sequence: 1, Field: test.field}) ||
				strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("policy = %+v, err = %v", policy, err)
			}
		})
	}
	_, err := ReadAckPolicy(nil)
	var detail *Error
	if !errors.As(err, &detail) || detail.Location.Segment != "MSH" || detail.Code != ErrorCodeSegmentSequence {
		t.Fatalf("missing MSH error = %v", err)
	}
}

func TestNewACKCommitCodesAndErrorSegment(t *testing.T) {
	message, err := Parse([]byte("MSH|^~\\&|SRC|SF|DST|DF|||ADT^A08|INPUT|T|2.5.1|||AL|NE||8859/1\r"))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []AckCode{AckCommitAccept, AckCommitError, AckCommitReject} {
		t.Run(string(code), func(t *testing.T) {
			reply := NewACK(message, code, AckOptions{
				ControlID: "OUTPUT", Time: time.Date(2025, 4, 3, 12, 6, 5, 0, time.UTC),
				ErrorLocation: ErrorLocation{Segment: "MSH", Field: 9},
			})
			wire, err := reply.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			reply, err = Parse(wire)
			if err != nil {
				t.Fatal(err)
			}
			header := reply.Header()
			if header.ControlID != "OUTPUT" || header.DateTime != "20250403120605" ||
				header.SendingApplication.NamespaceID != "DST" || header.SendingFacility.NamespaceID != "DF" ||
				header.ReceivingApplication.NamespaceID != "SRC" || header.ReceivingFacility.NamespaceID != "SF" ||
				header.Type != (MessageType{Code: "ACK", Trigger: "A08", Structure: "ACK"}) ||
				header.Version != "2.5.1" || header.ProcessingID != "T" || header.CharacterSet != "8859/1" {
				t.Fatalf("response MSH = %+v", header)
			}
			policy, err := ReadAckPolicy(reply)
			if err != nil || policy.Enhanced() {
				t.Fatalf("response policy = %+v, err = %v", policy, err)
			}
			ack, err := ParseAcknowledgment(reply)
			if err != nil || ack.Code != code || ack.ControlID != "INPUT" {
				t.Fatalf("ack = %+v, err = %v", ack, err)
			}
			segment, hasERR := reply.Segment("ERR")
			if code == AckCommitAccept {
				if hasERR || ack.ErrorCode != "" {
					t.Fatalf("success carried ERR: %q", wire)
				}
			} else if !hasERR || ack.ErrorCode != ErrorCodeApplicationInternalError ||
				segment.Value(2, 1, 1) != "MSH" || segment.Value(2, 3, 1) != "9" {
				t.Fatalf("missing or incorrect ERR: %q", wire)
			}
		})
	}
}
