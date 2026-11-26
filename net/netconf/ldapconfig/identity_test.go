package ldapconfig

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	ldap "github.com/go-ldap/ldap/v3"
)

const (
	usersBase   = "ou=people,dc=example"
	groupsBase  = "ou=groups,dc=example"
	loginDN     = "cn=Exam Reader," + usersBase
	loginUID    = `a*)(uid=*)\é`
	loginSecret = "synthetic-login-password"
)

type identityWireFixture struct {
	users, groups     []*ldap.Entry
	groupResult       int64
	truncateGroups    bool
	unexpectedPaging  bool
	groupSearchActive chan struct{}
}

func standardIdentityFixture() identityWireFixture {
	return identityWireFixture{
		users:  []*ldap.Entry{ldap.NewEntry(loginDN, map[string][]string{"uid": {loginUID}})},
		groups: []*ldap.Entry{testIdentityGroup("second,"+groupsBase, loginDN), testIdentityGroup("first,"+groupsBase, loginDN)},
	}
}

func testIdentityGroup(dnSuffix, member string) *ldap.Entry {
	return ldap.NewEntry("cn="+dnSuffix, map[string][]string{"objectClass": {"top", "groupOfNames"}, "member": {member}})
}

func (f *identityWireFixture) respond(conn net.Conn, id int64, op *ber.Packet, bound *string) (bool, error) {
	// Keep the test peer strict about which identity may perform each search.
	switch op.Tag {
	case 0:
		dn, _ := op.Children[1].Value.(string)
		password := string(op.Children[2].Data.Bytes())
		code := int64(49)
		switch {
		case dn == "cn=reader,dc=example" && password == "synthetic-directory-password":
			*bound, code = dn, 0
		case dn == loginDN && password == loginSecret && *bound != loginDN:
			*bound, code = dn, 0
		}
		return false, writePacket(conn, resultMessage(id, 1, code, "synthetic-password-must-be-redacted"))
	case 3:
		base, _ := op.Children[0].Value.(string)
		if op.Children[1].Value != int64(ldap.ScopeWholeSubtree) || op.Children[2].Value != int64(ldap.NeverDerefAliases) {
			return true, ErrProtocol
		}
		filter := op.Children[6]
		items := f.users
		code := int64(0)
		if base == usersBase {
			if *bound == loginDN || !testEqualityFilter(filter, "uid", loginUID) {
				return true, ErrProtocol
			}
		} else if base == groupsBase {
			membership := filter.Tag == 0 && len(filter.Children) == 2 &&
				testEqualityFilter(filter.Children[0], "objectClass", "groupOfNames") &&
				testEqualityFilter(filter.Children[1], "member", loginDN)
			if *bound != loginDN || !membership {
				return true, ErrProtocol
			}
			if f.groupSearchActive != nil {
				close(f.groupSearchActive)
				_, _ = io.Copy(io.Discard, conn)
				return true, nil
			}
			items, code = f.groups, f.groupResult
		} else {
			return true, ErrProtocol
		}
		for _, item := range items {
			if err := writePacket(conn, entryMessage(id, item)); err != nil {
				return true, nil
			}
		}
		if base == groupsBase && f.truncateGroups {
			return true, nil
		}
		done := resultMessage(id, 5, code, "private search result detail")
		if base == groupsBase && f.unexpectedPaging {
			page := ldap.NewControlPaging(1)
			page.SetCookie([]byte("pending"))
			controls := ber.Encode(ber.ClassContext, ber.TypeConstructed, 0, nil, "")
			controls.AppendChild(page.Encode())
			done.AppendChild(controls)
		}
		return false, writePacket(conn, done)
	default:
		return true, ErrProtocol
	}
}

func testEqualityFilter(packet *ber.Packet, attribute, value string) bool {
	return packet.Tag == 3 && len(packet.Children) == 2 &&
		string(packet.Children[0].Data.Bytes()) == attribute && string(packet.Children[1].Data.Bytes()) == value
}

func identityTestConfig(peer *syntheticDirectory) Config {
	cfg := peer.config()
	cfg.BaseDN = usersBase
	return cfg
}

func TestLDAPIdentityVerifiedTLSAndLiteralFilters(t *testing.T) {
	for _, mode := range []string{"ldaps", "starttls"} {
		t.Run(mode, func(t *testing.T) {
			fixture := standardIdentityFixture()
			peer := newSyntheticDirectoryFixture(t, mode, &fixture)
			c := identityTestConfig(peer)
			if mode == "ldaps" {
				c.BindDN, c.Password = "", ""
			}
			identity, err := Authenticate(context.Background(), c, loginUID, loginSecret,
				AuthenticationOptions{GroupBaseDN: groupsBase, MaxGroups: 2})
			want := Identity{DN: loginDN, Groups: []string{"cn=first," + groupsBase, "cn=second," + groupsBase}}
			if err != nil || !reflect.DeepEqual(identity, want) {
				t.Fatalf("identity = %#v, error = %v", identity, err)
			}
			// Read, bind as the end user, and group search stay inside TLS.
			wantOps := []ber.Tag{3, 0, 3}
			if mode == "starttls" {
				wantOps = []ber.Tag{23, 0, 3, 0, 3}
			}
			peer.mu.Lock()
			defer peer.mu.Unlock()
			if !reflect.DeepEqual(peer.ops, wantOps) {
				t.Fatalf("LDAP operations = %v, want %v", peer.ops, wantOps)
			}
		})
	}
}

func TestLDAPIdentityRefusesIncompleteOrAmbiguousMembership(t *testing.T) {
	tests := []struct {
		name      string
		change    func(*identityWireFixture)
		maxGroups int
		want      error
	}{
		{name: "missing user", change: func(f *identityWireFixture) { f.users = nil }, want: ErrIdentity},
		{name: "duplicate user", change: func(f *identityWireFixture) { f.users = append(f.users, f.users[0]) }, want: ErrIdentity},
		{name: "nonliteral uid", change: func(f *identityWireFixture) {
			f.users = []*ldap.Entry{ldap.NewEntry(loginDN, map[string][]string{"uid": {"different"}})}
		}, want: ErrIdentity},
		{name: "group limit ignored by peer", maxGroups: 1, want: ErrLimit},
		{name: "server cutoff after a permitted group", change: func(f *identityWireFixture) {
			f.groups = f.groups[:1]
			f.groupResult = ldap.LDAPResultSizeLimitExceeded
		}, want: ErrLimit},
		{name: "server refuses after a permitted group", change: func(f *identityWireFixture) {
			f.groups = f.groups[:1]
			f.groupResult = ldap.LDAPResultUnavailable
		}, want: ErrTransport},
		{name: "connection closes after partial entries", change: func(f *identityWireFixture) {
			f.groups = f.groups[:1]
			f.truncateGroups = true
		}, want: ErrTransport},
		{name: "unsolicited paging cookie", change: func(f *identityWireFixture) { f.unexpectedPaging = true }, want: ErrProtocol},
		{name: "other subtree", change: func(f *identityWireFixture) {
			f.groups[0] = testIdentityGroup("outside,ou=external,dc=example", loginDN)
		}, want: ErrInvalid},
		{name: "false membership", change: func(f *identityWireFixture) {
			f.groups[0] = testIdentityGroup("second,"+groupsBase, "cn=Someone Else,"+usersBase)
		}, want: ErrInvalid},
		{name: "duplicated group", change: func(f *identityWireFixture) {
			f.groups[0] = ldap.NewEntry("CN=first,"+groupsBase, map[string][]string{
				"objectClass": {"groupOfNames"}, "member": {loginDN},
			})
		}, want: ErrInvalid},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := standardIdentityFixture()
			if test.change != nil {
				test.change(&fixture)
			}
			peer := newSyntheticDirectoryFixture(t, "ldaps", &fixture)
			got, err := Authenticate(context.Background(), identityTestConfig(peer), loginUID, loginSecret,
				AuthenticationOptions{GroupBaseDN: groupsBase, MaxGroups: test.maxGroups})
			if !errors.Is(err, test.want) || got.DN != "" || got.Groups != nil ||
				strings.Contains(err.Error(), "private search result detail") {
				t.Fatalf("identity = %#v, error = %v, want %v", got, err, test.want)
			}
		})
	}
}

func TestLDAPIdentityRefusesInvalidTrustAndCredentials(t *testing.T) {
	fixture := standardIdentityFixture()
	peer := newSyntheticDirectoryFixture(t, "ldaps", &fixture)
	c := identityTestConfig(peer)
	options := AuthenticationOptions{GroupBaseDN: groupsBase}
	login := func(cfg Config, secret string, want error) {
		t.Helper()
		got, err := Authenticate(context.Background(), cfg, loginUID, secret, options)
		if !errors.Is(err, want) || got.DN != "" || got.Groups != nil ||
			strings.Contains(err.Error(), "synthetic-password-must-be-redacted") {
			t.Fatalf("identity = %#v, error = %v, want %v", got, err, want)
		}
	}
	login(c, "", ErrAuthentication)
	badTrust := c
	badTrust.TLS = &tls.Config{RootCAs: x509.NewCertPool()}
	login(badTrust, loginSecret, ErrTransport)
	badTrust.TLS.InsecureSkipVerify = true
	login(badTrust, loginSecret, ErrInvalid)
	peer.mu.Lock()
	ops := len(peer.ops)
	peer.mu.Unlock()
	if ops != 0 {
		t.Fatalf("untrusted connection processed %d LDAP requests", ops)
	}
	login(c, "incorrect", ErrAuthentication)
}

func TestLDAPIdentityCancellationClosesGroupSearch(t *testing.T) {
	fixture := standardIdentityFixture()
	fixture.groupSearchActive = make(chan struct{})
	peer := newSyntheticDirectoryFixture(t, "ldaps", &fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		id, err := Authenticate(ctx, identityTestConfig(peer), loginUID, loginSecret,
			AuthenticationOptions{GroupBaseDN: groupsBase})
		if id.DN != "" || id.Groups != nil {
			err = ErrIdentity
		}
		finished <- err
	}()
	select {
	case <-fixture.groupSearchActive:
	case <-ctx.Done():
		t.Fatal("group search did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled group lookup = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("group search did not release its connection")
	}
}

func TestLDAPIdentityDNKeyPreservesValueCase(t *testing.T) {
	a, err := IdentityDNKey(`OU=people+CN=Clinic\20Reader,dc=example`)
	if err != nil {
		t.Fatal(err)
	}
	b, err := IdentityDNKey(`cn=Clinic Reader+ou=people,DC=example`)
	if err != nil || b != a {
		t.Fatalf("equivalent DN forms: %q, %q, %v", a, b, err)
	}
	c, err := IdentityDNKey(`cn=clinic reader+ou=people,DC=example`)
	if err != nil || c == a {
		t.Fatalf("case-sensitive DN policy values conflated: %q, %q, %v", a, c, err)
	}
}

func TestLDAPIdentityOpenLDAP(t *testing.T) {
	path := os.Getenv("DICOMGO_OPENLDAP_IDENTITY_FIXTURE")
	if path == "" {
		t.Skip("set DICOMGO_OPENLDAP_IDENTITY_FIXTURE to the synthetic loopback OpenLDAP fixture JSON")
	}
	read := func(path string) []byte {
		t.Helper()
		value, err := os.ReadFile(path)
		if err != nil {
			t.Fatal("cannot read OpenLDAP fixture input")
		}
		return value
	}
	var fixture struct {
		LDAPPort, LDAPSport                                   int
		CAPath, WrongCAPath, PasswordPath, SearchPasswordPath string
	}
	if err := json.Unmarshal(read(path), &fixture); err != nil || fixture.LDAPPort < 1 || fixture.LDAPSport < 1 {
		t.Fatal("invalid OpenLDAP fixture")
	}
	roots := func(path string) *x509.CertPool {
		t.Helper()
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(read(path)) {
			t.Fatal("invalid fixture trust material")
		}
		return pool
	}
	const suffix = ",dc=test"
	password := string(read(fixture.PasswordPath))
	for _, endpoint := range []struct {
		scheme string
		port   int
	}{
		{"ldaps", fixture.LDAPSport}, {"ldap", fixture.LDAPPort},
	} {
		t.Run(endpoint.scheme, func(t *testing.T) {
			cfg := Config{
				URL:    endpoint.scheme + "://127.0.0.1:" + strconv.Itoa(endpoint.port),
				BaseDN: usersBase + suffix,
				TLS:    &tls.Config{RootCAs: roots(fixture.CAPath)},
			}
			if endpoint.scheme == "ldap" {
				cfg.BindDN = "cn=manager,dc=example,dc=test"
				cfg.Password = string(read(fixture.SearchPasswordPath))
			}
			options := AuthenticationOptions{GroupBaseDN: groupsBase + suffix, MaxGroups: 2}
			expected := Identity{"cn=Alice," + usersBase + suffix, []string{
				"cn=readers," + groupsBase + suffix,
				"cn=second," + groupsBase + suffix,
			}}
			got, err := Authenticate(context.Background(), cfg, "alice", password, options)
			if err != nil || !reflect.DeepEqual(got, expected) {
				t.Fatalf("OpenLDAP identity=%+v error=%v", got, err)
			}
			literal, err := Authenticate(context.Background(), cfg, loginUID, password, options)
			if err != nil || literal.DN != "cn=Escaped,"+usersBase+suffix || len(literal.Groups) != 1 {
				t.Fatalf("OpenLDAP literal search identity=%+v error=%v", literal, err)
			}
			for _, test := range []struct {
				name, user, secret string
				max                int
				roots              *x509.CertPool
				want               error
			}{
				{"oversized", "alice", password, 1, cfg.TLS.RootCAs, ErrLimit},
				{"missing", "*) (uid=*)", password, 2, cfg.TLS.RootCAs, ErrIdentity},
				{"bad trust", "alice", password, 2, roots(fixture.WrongCAPath), ErrTransport},
				{"bad credentials", "alice", "invalid", 2, cfg.TLS.RootCAs, ErrAuthentication},
			} {
				t.Run(test.name, func(t *testing.T) {
					options.MaxGroups = test.max
					cfg.TLS = &tls.Config{RootCAs: test.roots}
					got, err := Authenticate(context.Background(), cfg, test.user, test.secret, options)
					if !errors.Is(err, test.want) || got.DN != "" || got.Groups != nil {
						t.Fatalf("OpenLDAP identity=%+v error=%v, want %v", got, err, test.want)
					}
				})
			}
		})
	}
}
