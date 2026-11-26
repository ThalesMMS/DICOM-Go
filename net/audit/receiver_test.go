package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestReceiverTLSAndUDPRealSockets(t *testing.T) {
	for _, network := range []string{SyslogTLS, SyslogUDP} {
		t.Run(network, func(t *testing.T) {
			records := make(chan SyslogRecord, 1)
			receiver, err := NewReceiver(ReceiverOptions{MaxRecordBytes: 32768, Handle: func(_ context.Context, r SyslogRecord) error { records <- r; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			logger := &SyslogLogger{Network: network, AppName: "controlled", Hostname: "TESTHOST"}
			if network == SyslogTLS {
				certificate, pool := testCertificate(t)
				listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})
				if err != nil {
					t.Fatal(err)
				}
				logger.Address = listener.Addr().String()
				logger.TLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
				go func() { done <- receiver.Serve(ctx, listener) }()
			} else {
				conn, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				logger.Address = conn.LocalAddr().String()
				go func() { done <- receiver.ServeUDP(ctx, conn) }()
			}
			if err = logger.Log(atnaMessages()["application start"]); err != nil {
				t.Fatal(err)
			}
			if err = logger.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-records:
				if r.Priority != 85 || r.AppName != "controlled" || !bytes.Contains(r.Message, []byte("<AuditMessage")) {
					t.Fatal("receiver lost header/payload")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("socket record not received")
			}
			cancel()
			if err = <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			logger.Close()
		})
	}
}
func TestReceiverRejectsLengthBeforePayloadReadAndHandlesFragments(t *testing.T) {
	for _, prefix := range []string{"0 ", "01 ", "99999999999999999999 ", "32769 ", "x "} {
		reader := bufio.NewReader(strings.NewReader(prefix))
		if _, err := readOctetRecord(reader, 32768); err == nil {
			t.Fatal("invalid count accepted")
		}
	}
	raw := []byte("<85>1 - host app proc DICOM+RFC3881 [id a=\"x\\] y\"] " + "\xef\xbb\xbf" + "<AuditMessage>é</AuditMessage>")
	r, err := ParseSyslogRecord(raw, 32768)
	if err != nil || r.StructuredData != "[id a=\"x\\] y\"]" || string(r.Message) != "<AuditMessage>é</AuditMessage>" {
		t.Fatalf("SD/UTF8 parse: %v", err)
	}
	raw[len(raw)-1] = 'X'
	if r.Message[len(r.Message)-1] == 'X' {
		t.Fatal("parse input aliased")
	}
	if _, err := ParseSyslogRecord([]byte("<85>1 - host app proc id -"), 32768); err != nil {
		t.Fatal("optional MSG refused")
	}
	for _, bad := range []string{"<999>1 - h a p i - x", "<85>2 - h a p i - x", "<85>1 invalid h a p i - x", "<85>1 - h a p i [unclosed x", "<85>1 - h a p i - \xef\xbb\xbf\xff"} {
		if _, err := ParseSyslogRecord([]byte(bad), 32768); err == nil {
			t.Fatal("invalid header accepted")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	records, receiver := startAuditReceiver(t, listener)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	record := (&SyslogLogger{AppName: "split"}).record(5, time.Now(), []byte("controlled"))
	prefix := []byte(stringInt(len(record)) + " ")
	for _, part := range [][]byte{prefix[:1], prefix[1:], record[:3], record[3:]} {
		if _, err = conn.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-records:
	case <-time.After(5 * time.Second):
		t.Fatal("fragmented record lost")
	}
	// The receiver counts a record after its handler returns, so the count
	// can trail the record by a moment.
	deadline := time.Now().Add(5 * time.Second)
	for receiver.Stats().Received != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := receiver.Stats().Received; got != 1 {
		t.Fatalf("received count %d, want 1", got)
	}
}
func stringInt(n int) string { // avoid a second framing helper in production
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}

func TestReceiverStructuredDataRFC5424ValuesAndIdentity(t *testing.T) {
	valid := map[string]string{
		"controls":       "[id value=\"NUL\x00LF\nDEL\x7f\"]",
		"unknownEscape":  `[id value="left\qright"]`,
		"escaped":        `[id value="quote\" slash\\ bracket\]"]`,
		"unicode":        "[id value=\"é界😀\U0010ffff\"]",
		"repeatedParam":  `[id value="one" value="two"][other value="three"]`,
		"distinctIDCase": `[id][ID]`,
	}
	for name, sd := range valid {
		t.Run(name, func(t *testing.T) {
			raw := []byte("<85>1 - h a p i " + sd + " message")
			r, err := ParseSyslogRecord(raw, 32768)
			if err != nil || r.StructuredData != sd || !bytes.Equal(r.Raw, raw) || string(r.Message) != "message" {
				t.Fatal("valid structured data was refused or its bytes changed")
			}
		})
	}
	invalid := map[string]string{
		"duplicateID":     `[id value="one"][id value="two"]`,
		"overlongUTF8":    "[id value=\"\xc0\x80\"]",
		"continuation":    "[id value=\"\x80\"]",
		"surrogate":       "[id value=\"\xed\xa0\x80\"]",
		"aboveMaxUnicode": "[id value=\"\xf4\x90\x80\x80\"]",
		"truncatedUTF8":   "[id value=\"\xc3\"]",
		"unescapedClose":  `[id value="a]b"]`,
	}
	for name, sd := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSyslogRecord([]byte("<85>1 - h a p i "+sd), 32768); !errors.Is(err, ErrSyslogRecord) {
				t.Fatal("invalid structured data was accepted")
			}
		})
	}
}

func TestReceiverPriorityAndTimestampRFC5424Grammar(t *testing.T) {
	for _, priority := range []string{"0", "85", "191"} {
		if _, err := ParseSyslogRecord([]byte("<"+priority+">1 - h a p i -"), 32768); err != nil {
			t.Fatal("valid priority refused")
		}
	}
	for _, priority := range []string{"+85", "085", "00", "-0", "192", "8a"} {
		if _, err := ParseSyslogRecord([]byte("<"+priority+">1 - h a p i -"), 32768); err == nil {
			t.Error("invalid priority accepted")
		}
	}
	valid := []string{"-", "2026-10-09T00:00:00Z", "2026-10-09T23:59:59.1Z", "2026-10-09T23:59:59.123456-07:00", "2026-10-09T23:59:59+23:59"}
	for _, timestamp := range valid {
		if _, err := ParseSyslogRecord([]byte("<85>1 "+timestamp+" h a p i -"), 32768); err != nil {
			t.Fatal("valid RFC5424 timestamp refused")
		}
	}
	invalid := map[string]string{
		"fractionTooLong": "2026-10-09T23:59:59.1234567Z",
		"emptyFraction":   "2026-10-09T23:59:59.Z",
		"commaFraction":   "2026-10-09T23:59:59,1Z",
		"shortHour":       "2026-10-09T1:59:59Z",
		"offsetHour":      "2026-10-09T23:59:59+24:00",
		"offsetMinute":    "2026-10-09T23:59:59+01:60",
		"lowerT":          "2026-10-09t23:59:59Z",
		"lowerZ":          "2026-10-09T23:59:59z",
		"leapSecond":      "2026-10-09T23:59:60Z",
		"badDate":         "2026-04-31T23:59:59Z",
	}
	for name, timestamp := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSyslogRecord([]byte("<85>1 "+timestamp+" h a p i -"), 32768); err == nil {
				t.Fatal("invalid RFC5424 timestamp accepted")
			}
		})
	}
}

func TestReceiverOctetFramingPreservesStructuredDataControls(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	records, _ := startAuditReceiver(t, listener)
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sd := "[id value=\"\x00\n\x7fé\\q\"]"
	raw := []byte("<85>1 - h a p i " + sd + " message")
	framed := append([]byte(stringInt(len(raw))+" "), raw...)
	if _, err = conn.Write(framed); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-records:
		if r.StructuredData != sd || !bytes.Equal(r.Raw, raw) {
			t.Fatal("socket framing changed accepted bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("valid control/unknown-escape record not received")
	}
}
