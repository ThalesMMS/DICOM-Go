package ldapconfig

import (
	"context"
	"crypto/tls"
	"errors"
	"github.com/ThalesMMS/dicom-go/net/netconf"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	ber "github.com/go-asn1-ber/asn1-ber"
	ldap "github.com/go-ldap/ldap/v3"
)

const deviceFilter = "(objectClass=dicomDevice)"
const childrenFilter = "(|(objectClass=dicomNetworkConnection)(objectClass=dicomNetworkAE)(objectClass=dicomTransferCapability))"

var attributes = []string{"objectClass", "dicomDeviceName", "dicomDescription", "dicomInstalled", "dicomHostname", "dicomPort", "dicomTLSCipherSuite", "dicomAETitle", "dicomNetworkConnectionReference", "dicomAssociationInitiator", "dicomAssociationAcceptor", "dicomSOPClass", "dicomTransferRole", "dicomTransferSyntax"}

// Read owns one connection for the complete bounded transaction. Results are
// published only after every selected device and reference has validated.
func Read(ctx context.Context, c Config, options Options) (Snapshot, error) {
	o, err := normalized(options)
	if err != nil {
		return Snapshot{}, err
	}
	u, root, err := validated(c)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	err = withLDAPSession(ctx, c, u, o, func(session *ldapSession) error {
		snapshot, err = readDevices(session, c.BaseDN, root, o)
		return err
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

type ldapSession struct {
	ctx    context.Context
	client *ldap.Conn
	guard  *boundedConn
}

func withLDAPSession(ctx context.Context, c Config, u *url.URL, o Options, run func(*ldapSession) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	if err := transactionContextError(ctx); err != nil {
		return err
	}
	port := u.Port()
	if port == "" {
		port = "389"
		if u.Scheme == "ldaps" {
			port = "636"
		}
	}
	raw, err := sourceport.DialContext(&net.Dialer{Timeout: o.Timeout})(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		if contextErr := transactionContextError(ctx); contextErr != nil {
			return contextErr
		}
		return ErrTransport
	}
	defer raw.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = raw.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	transport := net.Conn(raw)
	secure := u.Scheme == "ldaps" || !c.AllowAnonymousCleartext
	if secure {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.Hostname()}
		if c.TLS != nil {
			tlsConfig = c.TLS.Clone()
			if tlsConfig.MinVersion < tls.VersionTLS12 {
				tlsConfig.MinVersion = tls.VersionTLS12
			}
			if tlsConfig.ServerName == "" {
				tlsConfig.ServerName = u.Hostname()
			}
		}
		if u.Scheme == "ldap" {
			if err := negotiateStartTLS(raw, o); err != nil {
				if contextErr := transactionContextError(ctx); contextErr != nil {
					return contextErr
				}
				return err
			}
		}
		encrypted := tls.Client(raw, tlsConfig)
		if err := encrypted.HandshakeContext(ctx); err != nil {
			if contextErr := transactionContextError(ctx); contextErr != nil {
				return contextErr
			}
			return ErrTransport
		}
		transport = encrypted
	}
	guard := &boundedConn{Conn: transport, options: o}
	client := ldap.NewConn(guard, secure)
	client.SetTimeout(o.Timeout)
	client.Start()
	defer client.Close()
	session := &ldapSession{ctx: ctx, client: client, guard: guard}
	if c.BindDN != "" {
		if err := client.Bind(c.BindDN, c.Password); err != nil {
			return session.normalizeError(err)
		}
	}
	if err := run(session); err != nil {
		return err
	}
	return transactionContextError(ctx)
}

// Socket deadlines can wake I/O before the context timer publishes Err.
// Both use this transaction's deadline, so its elapsed absolute value owns
// classification even while the timer callback is waiting to run.
func transactionContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (s *ldapSession) normalizeError(err error) error {
	if contextErr := transactionContextError(s.ctx); contextErr != nil {
		return contextErr
	}
	for _, safe := range []error{ErrLimit, ErrProtocol} {
		if errors.Is(s.guard.lastError(), safe) {
			return safe
		}
	}
	switch {
	case ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials):
		return ErrAuthentication
	case ldap.IsErrorWithCode(err, ldap.LDAPResultReferral):
		return ErrReferral
	case ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded),
		ldap.IsErrorWithCode(err, ldap.LDAPResultAdminLimitExceeded):
		return ErrLimit
	default:
		return ErrTransport
	}
}

func readDevices(session *ldapSession, baseDN string, root *ldap.DN, o Options) (Snapshot, error) {
	ctx := session.ctx
	pages, entries, bytes := 0, 0, 0
	search := func(base, filter string) ([]*ldap.Entry, error) {
		var result []*ldap.Entry
		cookie := []byte(nil)
		// The size limit stays the same on every page of one search: a
		// server such as ApacheDS ends a paged search whose request changes.
		sizeLimit := o.MaxEntries - entries + 1
		for {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			pages++
			if pages > o.MaxPages {
				return nil, ErrLimit
			}
			paging := ldap.NewControlPaging(uint32(o.PageSize))
			paging.SetCookie(cookie)
			request := ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, sizeLimit, int((o.Timeout+time.Second-1)/time.Second), false, filter, attributes, []ldap.Control{paging})
			response, err := session.client.Search(request)
			if err != nil {
				return nil, session.normalizeError(err)
			}
			if len(response.Referrals) > 0 {
				return nil, ErrReferral
			}
			for _, entry := range response.Entries {
				entries++
				if entries > o.MaxEntries {
					return nil, ErrLimit
				}
				weight, err := admitEntry(entry, root, o)
				if err != nil {
					return nil, err
				}
				if weight > o.MaxModelBytes-bytes {
					return nil, ErrLimit
				}
				bytes += weight
				result = append(result, entry)
			}
			control := ldap.FindControl(response.Controls, ldap.ControlTypePaging)
			if control == nil {
				break
			}
			page, ok := control.(*ldap.ControlPaging)
			if !ok || len(page.Cookie) > 4096 {
				return nil, ErrProtocol
			}
			if len(page.Cookie) == 0 {
				break
			}
			// The cookie is opaque (RFC 2696) and a server may return the
			// same one for every page, as ApacheDS does. A page that makes
			// no progress ends the search; MaxPages bounds the rest.
			if len(response.Entries) == 0 {
				return nil, ErrProtocol
			}
			cookie = append(cookie[:0], page.Cookie...)
		}
		return result, nil
	}
	filter := deviceFilter
	if o.DeviceName != "" {
		filter = "(&" + deviceFilter + "(dicomDeviceName=" + ldap.EscapeFilter(o.DeviceName) + "))"
	}
	devices, err := search(baseDN, filter)
	if err != nil {
		return Snapshot{}, err
	}
	if len(devices) > o.MaxDevices {
		return Snapshot{}, ErrLimit
	}
	snapshot := Snapshot{Edition: NormativeEdition}
	seenDevices := make(map[string]bool)
	for _, entry := range devices {
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		key, err := dnKey(entry.DN)
		if err != nil || seenDevices[key] {
			return Snapshot{}, ErrInvalid
		}
		seenDevices[key] = true
		if o.DeviceName != "" {
			name, err := single(entry, "dicomDeviceName", true)
			if err != nil || name != o.DeviceName {
				return Snapshot{}, ErrInvalid
			}
		}
		children, err := search(entry.DN, childrenFilter)
		if err != nil {
			return Snapshot{}, err
		}
		device, err := mapDevice(ctx, entry, children, o)
		if err != nil {
			if ctx.Err() != nil || !(errors.Is(err, ErrInvalid) || errors.Is(err, netconf.ErrInvalid)) {
				return Snapshot{}, err
			}
			// A selected device must be valid. In a whole directory one
			// incomplete or contradictory device is reported and the
			// others stay readable.
			if o.DeviceName != "" {
				return Snapshot{}, ErrInvalid
			}
			snapshot.Skipped = append(snapshot.Skipped, Diagnostic{Code: "device-invalid", Object: skippedDeviceName(entry)})
			continue
		}
		if len(device.Model.ApplicationEntities) > 0 {
			snapshot.Devices = append(snapshot.Devices, device)
		} else if device.Model.SchemaVersion == 0 {
			snapshot.Skipped = append(snapshot.Skipped, Diagnostic{Code: "application-entities-absent", Object: skippedDeviceName(entry)})
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// Negotiate StartTLS before starting the client reader so LDAP admission sits
// above TLS for all bind/search responses, without a process-global BER limit.
func negotiateStartTLS(conn net.Conn, o Options) error {
	request := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	request.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, 1, ""))
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, 23, nil, "")
	op.AppendChild(ber.NewString(ber.ClassContext, ber.TypePrimitive, 0, "1.3.6.1.4.1.1466.20037", ""))
	request.AppendChild(op)
	data := request.Bytes()
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil || n == 0 {
			return ErrTransport
		}
		data = data[n:]
	}
	frame, err := readFrame(conn, o, min(o.MaxWireBytes, 16<<10))
	if err != nil {
		if errors.Is(err, ErrLimit) {
			return ErrLimit
		}
		return ErrTransport
	}
	tag, err := validateEnvelope(frame, o)
	if err != nil || tag != 24 {
		return ErrProtocol
	}
	p, _ := ber.DecodePacketErr(frame)
	if p.Children[0].Value.(int64) != 1 {
		return ErrProtocol
	}
	if p.Children[1].Children[0].Value.(int64) != 0 {
		return ErrTransport
	}
	return nil
}

// An omitted port remains zero (initiator-only), never an invented DICOM port.
func numericPort(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > 65535 {
		return 0, ErrInvalid
	}
	return n, nil
}

// skippedDeviceName names a device that is left out of a snapshot.
func skippedDeviceName(entry *ldap.Entry) string {
	if name, err := single(entry, "dicomDeviceName", true); err == nil {
		return name
	}
	return entry.DN
}
