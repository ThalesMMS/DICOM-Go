package ul

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const configFixture = `# synthetic service-neutral profile
[[TransferSyntaxes]]
[Native]
TransferSyntax1 = LittleEndianExplicit
TransferSyntax2 = LittleEndianImplicit
[Other]
TransferSyntax1 = 1.2.3.40
[[PresentationContexts]]
[Contexts]
PresentationContext1 = CTImageStorage\Native
PresentationContext2 = CTImageStorage\Other
PresentationContext3 = 1.2.3.4\Native
[[SCPSCURoleSelection]]
[Roles]
Role1 = CTImageStorage\BOTH
Role2 = 1.2.3.4\SCP
[[ExtendedNegotiation]]
[Extended]
ExtendedNegotiation1 = CTImageStorage\aB0001
[[Profiles]]
[Example]
PresentationContexts = Contexts
SCPSCURoleSelection = Roles
ExtendedNegotiation = Extended
`

func parseConfigTest(t *testing.T, source string, options AssociationConfigOptions) *AssociationConfig {
	t.Helper()
	c, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), options)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAssociationConfigModesRolesAndOwnership(t *testing.T) {
	c := parseConfigTest(t, configFixture, AssociationConfigOptions{})
	p, err := c.Profile("e x a m p l e", AssociationConfigModeRequestor)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Contexts) != 3 || p.Contexts[0].ID != 1 || p.Contexts[2].ID != 5 || p.Contexts[1].AbstractSyntaxUID != p.Contexts[0].AbstractSyntaxUID || !reflect.DeepEqual(p.Contexts[0].TransferSyntaxUIDs, []string{"1.2.840.10008.1.2.1", "1.2.840.10008.1.2"}) {
		t.Fatalf("contexts %#v", p.Contexts)
	}
	if !p.RoleSelections[0].SCURole || !p.RoleSelections[0].SCPRole || p.RoleSelections[1].SCURole || !p.RoleSelections[1].SCPRole || !reflect.DeepEqual(p.ExtendedNegotiation[0].Data, []byte{0xab, 0, 1}) {
		t.Fatal("requestor-relative roles or bytes")
	}
	handler := p.ExtendedNegotiationHandler()
	p.ExtendedNegotiation[0].Data[0] = 0
	p.Contexts[0].TransferSyntaxUIDs[0] = "changed"
	p.RoleSelections[0].SCURole = false
	again, err := c.Profile("Example", AssociationConfigModeRequestor)
	if err != nil || again.Contexts[0].TransferSyntaxUIDs[0] != "1.2.840.10008.1.2.1" || again.ExtendedNegotiation[0].Data[0] != 0xab || !again.RoleSelections[0].SCURole {
		t.Fatal("mutable profile aliased config")
	}
	r, _ := handler(SopClassExtendedNegotiationItem{SopClassUID: again.Contexts[0].AbstractSyntaxUID})
	r.Data[0] = 0
	r, _ = handler(SopClassExtendedNegotiationItem{SopClassUID: again.Contexts[0].AbstractSyntaxUID})
	if r.Data[0] != 0xab {
		t.Fatal("handler alias")
	}
	r, _ = handler(SopClassExtendedNegotiationItem{SopClassUID: "1.2.3.9"})
	if r.Action != ExtendedNegotiationActionIgnore {
		t.Fatal("unconfigured SOP response")
	}
	if _, err := c.Profile("Example", AssociationConfigModeAcceptor); !errors.Is(err, ErrAssociationConfig) {
		t.Fatal("duplicate SOP acceptor", err)
	}
	single := strings.Replace(configFixture, "PresentationContext2 = CTImageStorage\\Other\nPresentationContext3 = 1.2.3.4\\Native", "PresentationContext2 = 1.2.3.4\\Native", 1)
	singleConfig := parseConfigTest(t, single, AssociationConfigOptions{})
	a, err := singleConfig.Profile("Example", AssociationConfigModeAcceptor)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Rules) != 2 || a.Rules[0].Roles == nil || !a.Rules[0].Roles.SCU || !a.Rules[0].Roles.SCP || a.Rules[1].Roles.SCU || !a.Rules[1].Roles.SCP {
		t.Fatalf("acceptor role inversion %#v", a.Rules)
	}
	a.Rules[0].Roles.SCU = false
	b, _ := singleConfig.Profile("Example", AssociationConfigModeAcceptor)
	if !b.Rules[0].Roles.SCU {
		t.Fatal("role ownership")
	}
}

func minimalConfig(contexts int, repeated bool) string {
	var b strings.Builder
	b.WriteString("[[TransferSyntaxes]]\n[N]\nTransferSyntax1 = 1.2.840.10008.1.2.1\n[[PresentationContexts]]\n[C]\n")
	for i := 0; i < contexts; i++ {
		sop := fmt.Sprintf("1.2.3.%d", i+1)
		if repeated {
			sop = "1.2.3.1"
		}
		fmt.Fprintf(&b, "PresentationContext%d = %s\\N\n", i+1, sop)
	}
	b.WriteString("[[Profiles]]\n[P]\nPresentationContexts = C\n")
	return b.String()
}

func TestAssociationConfigProtocolAndCatalogBounds(t *testing.T) {
	for _, n := range []int{128, 129, 215} {
		c := parseConfigTest(t, minimalConfig(n, false), AssociationConfigOptions{})
		p, err := c.Profile("P", AssociationConfigModeRequestor)
		if n == 128 {
			if err != nil || p.Contexts[127].ID != 255 {
				t.Fatal(n, err)
			}
		} else if !errors.Is(err, ErrAssociationConfigLimit) {
			t.Fatal(n, err)
		}
		a, err := c.Profile("P", AssociationConfigModeAcceptor)
		if err != nil || len(a.Rules) != n {
			t.Fatal("catalog", n, err)
		}
	}
	c := parseConfigTest(t, minimalConfig(128, true), AssociationConfigOptions{})
	if _, err := c.Profile("P", AssociationConfigModeRequestor); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Profile("P", AssociationConfigModeAcceptor); !errors.Is(err, ErrAssociationConfig) {
		t.Fatal(err)
	}
	if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(minimalConfig(216, false)), AssociationConfigOptions{Limits: AssociationConfigLimits{MaxContextRules: 215}}); !errors.Is(err, ErrAssociationConfigLimit) {
		t.Fatal(err)
	}
	c = parseConfigTest(t, minimalConfig(1, false), AssociationConfigOptions{Limits: AssociationConfigLimits{MaxCompiledBytes: 1}})
	if _, err := c.Profile("P", AssociationConfigModeRequestor); !errors.Is(err, ErrAssociationConfigLimit) {
		t.Fatal("compiled admission", err)
	}
}

func TestAssociationConfigMalformedAndLimits(t *testing.T) {
	base := minimalConfig(1, false)
	cases := map[string]string{
		"duplicate key": strings.Replace(base, "TransferSyntax1 = 1.2.840.10008.1.2.1", "TransferSyntax1 = 1.2.840.10008.1.2.1\ntransfersyntax1 = 1.2.840.10008.1.2", 1),
		"gap":           strings.Replace(base, "TransferSyntax1", "TransferSyntax2", 1), "leading index zero": strings.Replace(base, "TransferSyntax1", "TransferSyntax01", 1),
		"unknown key": strings.Replace(base, "TransferSyntax1", "Unknown1", 1), "unknown group": strings.Replace(base, "TransferSyntaxes", "Roles", 1),
		"missing reference": strings.Replace(base, "PresentationContexts = C", "PresentationContexts = Absent", 1), "extra separator": strings.Replace(base, "1.2.3.1\\N", "1.2.3.1\\N\\N", 1),
		"nul": base + "\x00", "control": base + "\x7f", "bad utf8": base + "\xff", "canonical UID": strings.Replace(base, "1.2.3.1\\N", "1.02.3.1\\N", 1),
		"duplicate section": strings.Replace(base, "[[PresentationContexts]]", "[N]\nTransferSyntax1 = 1.2.3\n[[PresentationContexts]]", 1),
		"symbol case":       strings.Replace(base, "1.2.840.10008.1.2.1", "littleEndianExplicit", 1),
		"bad hex":           strings.Replace(configFixture, "aB0001", "00q0", 1), "odd hex": strings.Replace(configFixture, "aB0001", "001", 1),
		"missing role SOP": strings.Replace(configFixture, "Role2 = 1.2.3.4", "Role2 = 1.2.3.99", 1), "bad role": strings.Replace(configFixture, "\\BOTH", "\\NONE", 1),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), AssociationConfigOptions{}); !errors.Is(err, ErrAssociationConfig) {
				t.Fatalf("want invalid, got %v", err)
			}
		})
	}
	limits := []AssociationConfigLimits{{MaxBytes: int64(len(base) - 1)}, {MaxLineBytes: 10}, {MaxValueBytes: 5}, {MaxNameBytes: 2}, {MaxSections: 1}, {MaxEntries: 1}, {MaxProfiles: 1}, {MaxContextRules: 1}, {MaxTransferSyntaxes: 1}, {MaxRoleItems: 1}, {MaxExtendedItemBytes: 2}, {MaxExtendedBytes: 2}, {MaxExtendedItems: 1}}
	for i, l := range limits {
		source := configFixture
		if i == 0 {
			source = base
		}
		if i == 6 {
			source += "[P2]\nPresentationContexts = Contexts\n"
		}
		if i == 12 {
			source = strings.Replace(source, "ExtendedNegotiation1 = CTImageStorage\\aB0001", "ExtendedNegotiation1 = CTImageStorage\\aB0001\nExtendedNegotiation2 = 1.2.3.4\\ff", 1)
		}
		if i == 7 {
			source = minimalConfig(2, false)
		}
		_, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), AssociationConfigOptions{Limits: l})
		if !errors.Is(err, ErrAssociationConfigLimit) {
			t.Fatalf("budget %d: %v", i, err)
		}
	}
	for _, l := range []AssociationConfigLimits{{MaxBytes: -1}, {MaxEntries: -1}, {MaxExtendedItemBytes: 65535}} {
		if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(base), AssociationConfigOptions{Limits: l}); !errors.Is(err, ErrAssociationConfigLimit) {
			t.Fatal(err)
		}
	}
}

type failingAssociationConfigReader struct{ err error }

func (r failingAssociationConfigReader) Read([]byte) (int, error) { return 0, r.err }

func TestAssociationConfigCancellationReaderAndLineForms(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ParseAssociationConfig(ctx, strings.NewReader(""), AssociationConfigOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cause := errors.New("private reader data")
	_, err := ParseAssociationConfig(context.Background(), failingAssociationConfigReader{cause}, AssociationConfigOptions{})
	if !errors.Is(err, cause) || strings.Contains(err.Error(), "private") {
		t.Fatal("reader error ownership/redaction", err)
	}
	_, err = ReadAssociationConfigFile(context.Background(), "/absent-sensitive-config-path", AssociationConfigOptions{})
	if !errors.Is(err, ErrAssociationConfig) || strings.Contains(err.Error(), "sensitive") {
		t.Fatal(err)
	}
	for _, ending := range []string{"\n", "\r\n", "\r"} {
		source := strings.ReplaceAll(minimalConfig(1, false), "\n", ending)
		if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), AssociationConfigOptions{}); err != nil {
			t.Fatal(ending, err)
		}
	}
	// Indented continuation belongs to the preceding value; unlike a new key,
	// this folds a named reference's whitespace without dropping its provenance.
	source := strings.Replace(minimalConfig(1, false), "PresentationContexts = C", "PresentationContexts =\n C", 1)
	if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), AssociationConfigOptions{}); err != nil {
		t.Fatal("continuation", err)
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		source := strings.Replace(minimalConfig(1, false), "1.2.840.10008.1.2.1", "=LocalEndianExplicit", 1)
		p, err := parseConfigTest(t, source, AssociationConfigOptions{LocalByteOrder: order}).Profile("P", AssociationConfigModeRequestor)
		if err != nil {
			t.Fatal(err)
		}
		want := "1.2.840.10008.1.2.1"
		if order == binary.BigEndian {
			want = "1.2.840.10008.1.2.2"
		}
		if p.Contexts[0].TransferSyntaxUIDs[0] != want {
			t.Fatal("local byte order")
		}
	}
}

// A full value returned with a terminal error must not publish a parsed config.
type terminalAssociationConfigReader struct {
	data string
	err  error
}

func (r *terminalAssociationConfigReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if r.data == "" {
		return n, r.err
	}
	return n, nil
}

type cancelAssociationConfigReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r cancelAssociationConfigReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}
func TestAssociationConfigReadBoundaryAndCancellation(t *testing.T) {
	source := minimalConfig(1, false)
	c, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), AssociationConfigOptions{Limits: AssociationConfigLimits{MaxBytes: int64(len(source))}})
	if err != nil || c == nil {
		t.Fatal("exact input cap", err)
	}
	if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(source+" "), AssociationConfigOptions{Limits: AssociationConfigLimits{MaxBytes: int64(len(source))}}); !errors.Is(err, ErrAssociationConfigLimit) {
		t.Fatal("one-byte oversize", err)
	}
	cause := errors.New("private terminal error")
	if c, err := ParseAssociationConfig(context.Background(), &terminalAssociationConfigReader{source, cause}, AssociationConfigOptions{}); c != nil || !errors.Is(err, cause) || strings.Contains(err.Error(), "private") {
		t.Fatal("terminal read error", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if c, err := ParseAssociationConfig(ctx, cancelAssociationConfigReader{strings.NewReader(source), cancel}, AssociationConfigOptions{}); c != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation after read", err)
	}
	for _, limits := range []AssociationConfigLimits{{MaxBytes: math.MaxInt64}, {MaxLineBytes: math.MaxInt}, {MaxCompiledBytes: -1}} {
		if _, err := ParseAssociationConfig(context.Background(), strings.NewReader(source), AssociationConfigOptions{Limits: limits}); !errors.Is(err, ErrAssociationConfigLimit) {
			t.Fatal("overflow options", err)
		}
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		source := strings.Replace(source, "1.2.840.10008.1.2.1", "OppositeEndianExplicit", 1)
		profile, err := parseConfigTest(t, source, AssociationConfigOptions{LocalByteOrder: order}).Profile("P", AssociationConfigModeRequestor)
		want := "1.2.840.10008.1.2.2"
		if order == binary.BigEndian {
			want = "1.2.840.10008.1.2.1"
		}
		if err != nil || profile.Contexts[0].TransferSyntaxUIDs[0] != want {
			t.Fatal("opposite byte order", err)
		}
	}
}
func TestAssociationConfigConcurrentDetachedProfiles(t *testing.T) {
	c := parseConfigTest(t, minimalConfig(4, false), AssociationConfigOptions{})
	var workers sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 20; j++ {
				p, err := c.Profile("P", AssociationConfigModeAcceptor)
				if err != nil {
					failures <- err
					return
				}
				if p.Rules[0].TransferSyntaxes[0] != ExplicitVRLittleEndian {
					failures <- errors.New("profile slice mutation affected immutable config")
					return
				}
				p.Rules[0].TransferSyntaxes[0] = "caller mutation"
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
}

func exactCapacityAssociationConfigFixture() string {
	source := minimalConfig(3, false)
	source = strings.Replace(source, "TransferSyntax1 = 1.2.840.10008.1.2.1", "TransferSyntax1 = 1.2.840.10008.1.2.1\nTransferSyntax2 = 1.2.840.10008.1.2\nTransferSyntax3 = 1.2.840.10008.1.2.2", 1)
	var extra strings.Builder
	extra.WriteString("[[SCPSCURoleSelection]]\n[R]\n")
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(&extra, "Role%d = 1.2.3.%d\\SCU\n", i, i)
	}
	extra.WriteString("[[ExtendedNegotiation]]\n[E]\n")
	for i := 1; i <= 3; i++ {
		fmt.Fprintf(&extra, "ExtendedNegotiation%d = 1.2.3.%d\\%s\n", i, i, strings.Repeat("a7", 897))
	}
	source = strings.Replace(source, "[[Profiles]]", extra.String()+"[[Profiles]]", 1)
	return source + "SCPSCURoleSelection = R\nExtendedNegotiation = E\n"
}
func TestAssociationConfigExactCapacities(t *testing.T) {
	c := parseConfigTest(t, exactCapacityAssociationConfigFixture(), AssociationConfigOptions{})
	for _, mode := range []AssociationConfigMode{AssociationConfigModeRequestor, AssociationConfigModeAcceptor} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			p, err := c.Profile("P", mode)
			if err != nil {
				t.Fatal(err)
			}
			if cap(p.Contexts) != len(p.Contexts) || cap(p.Rules) != len(p.Rules) || cap(p.RoleSelections) != len(p.RoleSelections) || cap(p.ExtendedNegotiation) != len(p.ExtendedNegotiation) {
				t.Fatal("profile slice capacity exceeds admitted length")
			}
			for _, pc := range p.Contexts {
				if cap(pc.TransferSyntaxUIDs) != len(pc.TransferSyntaxUIDs) {
					t.Fatal("context syntax spare capacity")
				}
			}
			for _, rule := range p.Rules {
				if cap(rule.TransferSyntaxes) != len(rule.TransferSyntaxes) {
					t.Fatal("rule syntax spare capacity")
				}
			}
			for _, item := range p.ExtendedNegotiation {
				if len(item.Data) != 897 || cap(item.Data) != 897 {
					t.Fatalf("extended bytes len=%d cap=%d want897", len(item.Data), cap(item.Data))
				}
			}
			handler := p.ExtendedNegotiationHandler()
			r, err := handler(SopClassExtendedNegotiationItem{SopClassUID: "1.2.3.1"})
			if err != nil || len(r.Data) != 897 || cap(r.Data) != 897 {
				t.Fatal("handler response capacity", len(r.Data), cap(r.Data), err)
			}
		})
	}
}
func TestAssociationConfigCompiledAdmissionBoundary(t *testing.T) {
	source := exactCapacityAssociationConfigFixture()
	for _, mode := range []AssociationConfigMode{AssociationConfigModeRequestor, AssociationConfigModeAcceptor} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			// Publicly documented logical charge, independently calculated from this fixture.
			total := 0
			for i := 1; i <= 3; i++ {
				sop := fmt.Sprintf("1.2.3.%d", i)
				total += 128 + len(sop)
				for _, syntax := range []string{"1.2.840.10008.1.2.1", "1.2.840.10008.1.2", "1.2.840.10008.1.2.2"} {
					total += 16 + len(syntax)
				}
				total += 64 + len(sop)       // detached role
				total += 64 + len(sop) + 897 // extended structure and exact payload
			}
			if mode == AssociationConfigModeAcceptor {
				total += 64 + 64*3 + 64 + 64*3
			} // unique-SOP and role-index maps
			c := parseConfigTest(t, source, AssociationConfigOptions{Limits: AssociationConfigLimits{MaxCompiledBytes: total}})
			if _, err := c.Profile("P", mode); err != nil {
				t.Fatal("exact logical admission", total, err)
			}
			c = parseConfigTest(t, source, AssociationConfigOptions{Limits: AssociationConfigLimits{MaxCompiledBytes: total - 1}})
			if p, err := c.Profile("P", mode); !errors.Is(err, ErrAssociationConfigLimit) || len(p.Contexts) != 0 || len(p.Rules) != 0 || len(p.ExtendedNegotiation) != 0 {
				t.Fatal("limit-minus-one published profile", total-1, err)
			}
		})
	}
}
