package ldapconfig

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	ldap "github.com/go-ldap/ldap/v3"
)

type syntheticDirectory struct {
	listener    net.Listener
	serverTLS   *tls.Config
	clientTLS   *tls.Config
	mu          sync.Mutex
	connections []net.Conn
	ops         []ber.Tag
	filters     []*ber.Packet
	devices     []*ldap.Entry
	children    map[string][]*ldap.Entry
	mode        string
	identity    *identityWireFixture
	workers     sync.WaitGroup
	errors      chan error
}

func newSyntheticDirectory(t *testing.T, mode string) *syntheticDirectory {
	t.Helper()
	return newSyntheticDirectoryFixture(t, mode, nil)
}

func newSyntheticDirectoryFixture(t *testing.T, mode string, identity *identityWireFixture) *syntheticDirectory {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &syntheticDirectory{listener: listener, mode: mode, identity: identity, errors: make(chan error, 8), children: make(map[string][]*ldap.Entry), serverTLS: &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}, clientTLS: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	for _, name := range []string{"scanner*(lab)", "workstation"} {
		device, children := fixtureEntries(name)
		s.devices = append(s.devices, device)
		s.children[device.DN] = children
	}
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.connections = append(s.connections, conn)
			s.mu.Unlock()
			s.workers.Add(1)
			go func() {
				defer s.workers.Done()
				defer conn.Close()
				if err := s.serve(conn); err != nil {
					s.errors <- err
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		s.mu.Lock()
		for _, conn := range s.connections {
			_ = conn.Close()
		}
		s.mu.Unlock()
		done := make(chan struct{})
		go func() { s.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("synthetic directory did not drain")
		}
		close(s.errors)
		for err := range s.errors {
			t.Error("synthetic directory protocol", err)
		}
	})
	return s
}

func resultMessage(id int64, tag ber.Tag, code int64, diagnostic string) *ber.Packet {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "")
	op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, ""))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, diagnostic, ""))
	packet.AppendChild(op)
	return packet
}

func entryMessage(id int64, entry *ldap.Entry) *ber.Packet {
	packet := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	packet.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, 4, nil, "")
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, entry.DN, ""))
	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	for _, attr := range entry.Attributes {
		pair := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
		pair.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, attr.Name, ""))
		values := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
		for _, value := range attr.Values {
			values.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, value, ""))
		}
		pair.AppendChild(values)
		attrs.AppendChild(pair)
	}
	op.AppendChild(attrs)
	packet.AppendChild(op)
	return packet
}

func writePacket(conn net.Conn, packet *ber.Packet) error {
	data := packet.Bytes()
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func (s *syntheticDirectory) serve(raw net.Conn) error {
	_ = raw.SetDeadline(time.Now().Add(10 * time.Second))
	conn := net.Conn(raw)
	o, _ := normalized(Options{})
	boundDN := ""
	// session-paging keeps the position of each paged search on the
	// server, returns the same cookie for every page and ends a search
	// whose size limit changes between pages, as ApacheDS does.
	type pagingSession struct {
		offset int
		limit  int64
	}
	sessions := make(map[string]pagingSession)
	if s.mode != "starttls" {
		encrypted := tls.Server(raw, s.serverTLS)
		if err := encrypted.Handshake(); err != nil {
			return nil
		}
		conn = encrypted
	}
	for {
		frame, err := readFrame(conn, o, o.MaxWireBytes)
		if err != nil {
			return nil
		} // client cancellation/refusal owns socket close
		request, err := ber.DecodePacketErr(frame)
		if err != nil {
			return err
		}
		id := request.Children[0].Value.(int64)
		op := request.Children[1]
		s.mu.Lock()
		s.ops = append(s.ops, op.Tag)
		s.mu.Unlock()
		if s.identity != nil && op.Tag != 23 {
			stop, err := s.identity.respond(conn, id, op, &boundDN)
			if stop || err != nil {
				return err
			}
			continue
		}
		switch op.Tag {
		case 23:
			if s.mode != "starttls" || string(op.Children[0].Data.Bytes()) != "1.3.6.1.4.1.1466.20037" {
				return ErrProtocol
			}
			if err := writePacket(conn, resultMessage(id, 24, 0, "")); err != nil {
				return nil
			}
			encrypted := tls.Server(raw, s.serverTLS)
			if err := encrypted.Handshake(); err != nil {
				return nil
			}
			conn = encrypted
		case 0:
			if s.mode == "auth-failure" {
				if err := writePacket(conn, resultMessage(id, 1, 49, "synthetic-secret-not-an-error")); err != nil {
					return nil
				}
				continue
			}
			if op.Children[1].Value.(string) != "cn=reader,dc=example" || string(op.Children[2].Data.Bytes()) != "synthetic-directory-password" {
				return ErrAuthentication
			}
			if err := writePacket(conn, resultMessage(id, 1, 0, "")); err != nil {
				return nil
			}
		case 3:
			if s.mode == "stall" {
				_, _ = io.Copy(io.Discard, conn)
				return nil
			}
			if s.mode == "huge" {
				_, _ = conn.Write([]byte{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff})
				continue
			}
			if s.mode == "referral" {
				if err := writePacket(conn, resultMessage(id, 5, 10, "ldap://unconfigured.invalid")); err != nil {
					return nil
				}
				continue
			}
			base := op.Children[0].Value.(string)
			s.mu.Lock()
			s.filters = append(s.filters, op.Children[6])
			s.mu.Unlock()
			items := s.children[base]
			if base == fixtureRoot {
				items = s.devices
				if op.Children[6].Tag == 0 { // AND: objectClass and exact escaped value
					value := string(op.Children[6].Children[1].Children[1].Data.Bytes())
					items = nil
					for _, device := range s.devices {
						if device.GetAttributeValue("dicomDeviceName") == value {
							items = append(items, device)
						}
					}
				}
			}
			control, err := ldap.DecodeControl(request.Children[2].Children[0])
			if err != nil {
				return err
			}
			paging := control.(*ldap.ControlPaging)
			offset := 0
			if len(paging.Cookie) > 0 && s.mode != "session-paging" {
				offset, err = strconv.Atoi(string(paging.Cookie))
				if err != nil {
					return err
				}
			}
			if s.mode == "session-paging" {
				limit := op.Children[3].Value.(int64)
				state := sessions[base]
				if len(paging.Cookie) == 0 {
					state = pagingSession{limit: limit}
				} else if state.limit != limit || string(paging.Cookie) != "session" {
					if err := writePacket(conn, resultMessage(id, 5, 0, "")); err != nil {
						return nil
					}
					continue
				}
				offset = state.offset
				sessions[base] = pagingSession{offset: min(len(items), offset+int(paging.PagingSize)), limit: limit}
			}
			end := min(len(items), offset+int(paging.PagingSize))
			for _, entry := range items[offset:end] {
				if err := writePacket(conn, entryMessage(id, entry)); err != nil {
					return nil
				}
			}
			done := resultMessage(id, 5, 0, "")
			responsePaging := ldap.NewControlPaging(uint32(len(items)))
			if end < len(items) {
				responsePaging.SetCookie([]byte(strconv.Itoa(end)))
				if s.mode == "session-paging" {
					responsePaging.SetCookie([]byte("session"))
				}
			}
			controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "")
			controls.AppendChild(responsePaging.Encode())
			done.AppendChild(controls)
			if err := writePacket(conn, done); err != nil {
				return nil
			}
		default:
			return ErrProtocol // no add, modify, delete, rename or registry write
		}
	}
}

func (s *syntheticDirectory) config() Config {
	scheme := "ldaps"
	if s.mode == "starttls" {
		scheme = "ldap"
	}
	return Config{URL: scheme + "://" + s.listener.Addr().String(), BaseDN: fixtureRoot, BindDN: "cn=reader,dc=example", Password: "synthetic-directory-password", TLS: s.clientTLS}
}

func TestLDAPDirectoryRealTLSAndStartTLSReadMultipleDevicesAndPagedAEs(t *testing.T) {
	for _, mode := range []string{"ldaps", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			s := newSyntheticDirectory(t, mode)
			result, err := Read(context.Background(), s.config(), Options{PageSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			if result.Edition != NormativeEdition || len(result.Devices) != 2 || len(result.Devices[0].Model.ApplicationEntities) != 2 || len(result.Devices[1].Model.Connections) != 1 {
				t.Fatal("directory model incomplete")
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, op := range s.ops {
				if op != 0 && op != 3 && op != 23 {
					t.Fatal("directory modified")
				}
			}
		})
	}
}

func TestLDAPDirectoryEscapedEqualityAndBoundedFailures(t *testing.T) {
	s := newSyntheticDirectory(t, "ldaps")
	result, err := Read(context.Background(), s.config(), Options{DeviceName: "scanner*(lab)", AETitle: "ct_a", PageSize: 1})
	if err != nil || len(result.Devices) != 1 || len(result.Devices[0].Model.ApplicationEntities) != 1 || result.Devices[0].Model.ApplicationEntities[0].AETitle != "ct_a" {
		t.Fatal("literal filter or exact AE title lost", err)
	}
	s.mu.Lock()
	filter := s.filters[0]
	s.mu.Unlock()
	if filter.Tag != 0 || filter.Children[1].Tag != 3 || string(filter.Children[1].Children[1].Data.Bytes()) != "scanner*(lab)" {
		t.Fatal("filter value became LDAP syntax")
	}
	for _, options := range []Options{{MaxPages: 1, PageSize: 1}, {MaxDevices: 1}, {MaxEntries: 1}, {MaxModelBytes: 1}, {MaxPacketBytes: 16}} {
		if value, err := Read(context.Background(), s.config(), options); err == nil || len(value.Devices) != 0 {
			t.Fatal("limit published partial directory", err)
		}
	}
}

func TestLDAPDirectoryAuthenticationReferralMalformedAndCancellationAreValueFree(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want error
	}{{"auth-failure", ErrAuthentication}, {"referral", ErrReferral}, {"huge", ErrLimit}, {"stall", context.DeadlineExceeded}} {
		t.Run(tc.mode, func(t *testing.T) {
			s := newSyntheticDirectory(t, tc.mode)
			snapshot, err := Read(context.Background(), s.config(), Options{Timeout: 150 * time.Millisecond})
			if !errors.Is(err, tc.want) || len(snapshot.Devices) != 0 {
				t.Fatal("directory failure classification", err)
			}
			if strings.Contains(err.Error(), "synthetic-secret") || strings.Contains(err.Error(), "unconfigured") || strings.Contains(err.Error(), "cn=reader") {
				t.Fatal("server diagnostic or credentials leaked")
			}
		})
	}
	s := newSyntheticDirectory(t, "stall")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Read(ctx, s.config(), Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled call connected", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ops) != 0 {
		t.Fatal("cancelled call sent LDAP")
	}
}

func TestLDAPDirectoryPagedSearchWithServerSideCursor(t *testing.T) {
	s := newSyntheticDirectory(t, "session-paging")
	result, err := Read(context.Background(), s.config(), Options{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Each device has five children: one page each must not truncate them.
	if len(result.Devices) != 2 || len(result.Devices[0].Model.ApplicationEntities) != 2 ||
		len(result.Devices[0].Model.ApplicationEntities[1].TransferCapabilities) != 1 || len(result.Devices[1].Model.ApplicationEntities) != 2 {
		t.Fatalf("paged directory model is incomplete: %+v", result.Devices)
	}
}

func TestLDAPDirectoryReadSkipsInvalidAndNonDICOMDevices(t *testing.T) {
	s := newSyntheticDirectory(t, "ldaps")
	// A device whose AE references a connection that does not exist, and a
	// device without any Application Entity (a web application device).
	dangling, children := fixtureEntries("dangling")
	for _, child := range children {
		if len(child.GetAttributeValues("dicomNetworkConnectionReference")) > 0 {
			for _, attribute := range child.Attributes {
				if attribute.Name == "dicomNetworkConnectionReference" {
					attribute.Values = []string{"cn=missing," + dangling.DN}
				}
			}
		}
	}
	webOnly, webChildren := fixtureEntries("web-only")
	s.mu.Lock()
	s.devices = append(s.devices, dangling, webOnly)
	s.children[dangling.DN] = children
	s.children[webOnly.DN] = webChildren[:1] // its connection only
	s.mu.Unlock()

	result, err := Read(context.Background(), s.config(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Devices) != 2 {
		t.Fatalf("the valid devices were not read: %d devices", len(result.Devices))
	}
	want := map[string]string{"dangling": "device-invalid", "web-only": "application-entities-absent"}
	if len(result.Skipped) != len(want) {
		t.Fatalf("skipped devices = %+v", result.Skipped)
	}
	for _, skipped := range result.Skipped {
		if want[skipped.Object] != skipped.Code {
			t.Fatalf("skipped devices = %+v", result.Skipped)
		}
	}
	// An explicitly selected device must be valid.
	if _, err := Read(context.Background(), s.config(), Options{DeviceName: "dangling"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("selected invalid device error = %v", err)
	}
	if selected, err := Read(context.Background(), s.config(), Options{DeviceName: "web-only"}); err != nil || len(selected.Devices) != 0 {
		t.Fatalf("selected device without AE = %+v, %v", selected, err)
	}
}
