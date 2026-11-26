package ul

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
)

func officialAssociationConfigDirectory(t *testing.T) string {
	t.Helper()
	dcmtkoracle.Require(t)
	dir := os.Getenv("DICOMGO_DCMTK_CONFIG_DIR")
	if dir == "" {
		t.Skip("opt-in external official configs: DICOMGO_DCMTK_CONFIG_DIR")
	}
	return dir
}

func TestAssociationConfigDCMTKExamplesUnchanged(t *testing.T) {
	dir := officialAssociationConfigDirectory(t)
	oracle := os.Getenv("DICOMGO_DCMTK_ASSOCIATION_ORACLE")
	if oracle == "" {
		t.Skip("opt-in independently authored public-API dump harness: DICOMGO_DCMTK_ASSOCIATION_ORACLE")
	}
	for _, tc := range []struct {
		file, profile string
		mode          AssociationConfigMode
		count         int
	}{{"storescu.cfg", "Default", AssociationConfigModeRequestor, 128}, {"storescp.cfg", "Default", AssociationConfigModeAcceptor, 215}, {"storescp.cfg", "AllDICOM", AssociationConfigModeAcceptor, 183}} {
		t.Run(tc.file+"/"+tc.profile, func(t *testing.T) {
			path := filepath.Join(dir, tc.file)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			c, err := ReadAssociationConfigFile(context.Background(), path, AssociationConfigOptions{})
			if err != nil {
				t.Fatal(err)
			}
			p, err := c.Profile(tc.profile, tc.mode)
			if err != nil {
				t.Fatal(err)
			}
			contexts := p.Contexts
			if tc.mode == AssociationConfigModeAcceptor {
				for _, rule := range p.Rules {
					contexts = append(contexts, PresentationContext{AbstractSyntaxUID: rule.AbstractSyntaxUID, TransferSyntaxUIDs: rule.TransferSyntaxes})
				}
			}
			if len(contexts) != tc.count {
				t.Fatal("official context count", len(contexts))
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, oracle, path, tc.profile).Output()
			if err != nil {
				t.Fatal("DCMTK public-API dump", err)
			}
			if len(output) > 4<<20 {
				t.Fatal("oracle output bound")
			}
			expected := parseDCMTKProfileDump(t, output)
			if tc.mode == AssociationConfigModeAcceptor {
				for i := range expected {
					expected[i].ID = 0
				}
			}
			if !reflect.DeepEqual(contexts, expected) {
				t.Fatalf("official cfg differs from DCMTK profile: Go=%d contexts DCMTK=%d; expected exact SOPs, ordered syntaxes and canonical requestor IDs", len(contexts), len(expected))
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("external official cfg changed")
			}
		})
	}
}

func parseDCMTKProfileDump(t *testing.T, data []byte) []PresentationContext {
	t.Helper()
	pc := regexp.MustCompile(`^Presentation Context #([0-9]+), abstract syntax: ([0-9.]+)$`)
	ts := regexp.MustCompile(`^  Transfer Syntax #[0-9]+: ([0-9.]+)$`)
	var out []PresentationContext
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		line := s.Text()
		if m := pc.FindStringSubmatch(line); m != nil {
			out = append(out, PresentationContext{ID: byte(2*len(out) + 1), AbstractSyntaxUID: m[2]})
		} else if m := ts.FindStringSubmatch(line); m != nil {
			if len(out) == 0 {
				t.Fatal("oracle syntax before context")
			}
			out[len(out)-1].TransferSyntaxUIDs = append(out[len(out)-1].TransferSyntaxUIDs, m[1])
		}
	}
	if s.Err() != nil || len(out) == 0 {
		t.Fatal("oracle dump parse", s.Err())
	}
	return out
}

func startDCMTKAssociationProfileSCP(t *testing.T, ctx context.Context, config, profile string) string {
	t.Helper()
	tool := dcmtkoracle.Tool(t, "storescp")
	reserve, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserve.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	reserve.Close()
	cmd := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, tool, "-aet", "PROFILE", "-xf", config, profile, "-od", t.TempDir(), port)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-done
		if t.Failed() {
			t.Log("DCMTK synthetic negotiation peer:", output.String())
		}
		if ctx.Err() == nil && waitErr == nil {
			t.Log("oracle exited normally")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return address
		}
		select {
		case <-done:
			t.Fatalf("DCMTK profile SCP exited before ready: %v %s", waitErr, output.String())
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("DCMTK profile SCP readiness timeout")
	return ""
}

func probeAssociationProfile(t *testing.T, ctx context.Context, address string, contexts []PresentationContext, roles []RoleSelectionItem, extended []SopClassExtendedNegotiationItem) ([]PresentationContextOutcome, []RoleSelectionItem, []SopClassExtendedNegotiationItem) {
	t.Helper()
	a, err := DialContext(ctx, address, DialOptions{CalledAETitle: "PROFILE", CallingAETitle: "PROFILETEST", Contexts: contexts, RoleSelections: roles, ExtendedNegotiation: extended})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	out := a.PresentationContextOutcomes()
	r := append([]RoleSelectionItem(nil), a.AcceptedRoleSelections...)
	e := append([]SopClassExtendedNegotiationItem(nil), a.AcceptedExtendedNegotiation...)
	if err := a.Release(ctx); err != nil {
		t.Fatal("release", err)
	}
	return out, r, e
}

func probeLocalAssociationProfile(t *testing.T, ctx context.Context, p AssociationProfile, contexts []PresentationContext, roles []RoleSelectionItem, extended []SopClassExtendedNegotiationItem) ([]PresentationContextOutcome, []RoleSelectionItem, []SopClassExtendedNegotiationItem) {
	t.Helper()
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		a, err := listener.AcceptAssociation(AcceptOptions{AETitle: "PROFILE", Context: ctx, PresentationContextRules: p.Rules, ExtendedNegotiationHandler: p.ExtendedNegotiationHandler()})
		if err == nil {
			defer a.Close()
			if !reflect.DeepEqual(a.Contexts, contexts) {
				done <- fmt.Errorf("received wire template differs in IDs, SOPs or ordered syntaxes")
				return
			}
			var pdu PDU
			pdu, err = a.ReadPDU()
			if err == nil {
				if _, ok := pdu.(*ReleaseRQ); !ok {
					err = fmt.Errorf("unexpected PDU")
				} else {
					err = a.WritePDU(&ReleaseRP{})
				}
			}
		}
		done <- err
	}()
	o, r, e := probeAssociationProfile(t, ctx, listener.Addr().String(), contexts, roles, extended)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return o, r, e
}

func TestAssociationConfigDCMTKOfficialNegotiationRealSockets(t *testing.T) {
	dir := officialAssociationConfigDirectory(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(dir, "storescp.cfg")
	c, err := ReadAssociationConfigFile(ctx, path, AssociationConfigOptions{})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := c.Profile("Default", AssociationConfigModeAcceptor)
	if err != nil {
		t.Fatal(err)
	}
	address := startDCMTKAssociationProfileSCP(t, ctx, path, "Default")
	for start := 0; start < len(profile.Rules); start += 80 {
		end := min(start+80, len(profile.Rules))
		var proposals []PresentationContext
		for _, rule := range profile.Rules[start:end] {
			syntaxes := []string{rule.TransferSyntaxes[len(rule.TransferSyntaxes)-1]}
			if rule.TransferSyntaxes[0] != syntaxes[0] {
				syntaxes = append(syntaxes, rule.TransferSyntaxes[0])
			}
			proposals = append(proposals, PresentationContext{ID: byte(2*len(proposals) + 1), AbstractSyntaxUID: rule.AbstractSyntaxUID, TransferSyntaxUIDs: syntaxes})
		}
		proposals = append(proposals, PresentationContext{ID: byte(2*len(proposals) + 1), AbstractSyntaxUID: "1.2.3.99", TransferSyntaxUIDs: []string{ExplicitVRLittleEndian}}, PresentationContext{ID: byte(2*len(proposals) + 3), AbstractSyntaxUID: profile.Rules[start].AbstractSyntaxUID, TransferSyntaxUIDs: []string{"1.2.3.98"}})
		want, wr, we := probeAssociationProfile(t, ctx, address, proposals, nil, nil)
		got, gr, ge := probeLocalAssociationProfile(t, ctx, profile, proposals, nil, nil)
		if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gr, wr) || !reflect.DeepEqual(ge, we) {
			t.Fatalf("official profile negotiation chunk %d differs from DCMTK; compare IDs, results, SOPs, syntax preference, roles and ext exactly", start)
		}
		if got[len(got)-2].Result != PresentationContextAbstractSyntaxNotSupported || got[len(got)-1].Result != PresentationContextTransferSyntaxesNotSupported {
			t.Fatal("refusal controls")
		}
	}
	requestorConfig, err := ReadAssociationConfigFile(ctx, filepath.Join(dir, "storescu.cfg"), AssociationConfigOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requestor, err := requestorConfig.Profile("Default", AssociationConfigModeRequestor)
	if err != nil {
		t.Fatal(err)
	}
	want, wr, we := probeAssociationProfile(t, ctx, address, requestor.Contexts, requestor.RoleSelections, requestor.ExtendedNegotiation)
	got, gr, ge := probeLocalAssociationProfile(t, ctx, profile, requestor.Contexts, requestor.RoleSelections, requestor.ExtendedNegotiation)
	if len(got) != 128 || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gr, wr) || !reflect.DeepEqual(ge, we) {
		t.Fatal("complete unchanged official SCU 128-context wire template differs from DCMTK")
	}
}

func TestAssociationConfigDCMTKRolesAndExtendedRealSockets(t *testing.T) {
	dcmtkoracle.Require(t)
	for _, role := range []string{"SCU", "SCP", "BOTH"} {
		t.Run(role, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			source := strings.Replace(minimalConfig(1, false), "PresentationContext1 = 1.2.3.1\\N", "PresentationContext1 = 1.2.3.1\\N\nPresentationContext2 = 1.2.840.10008.1.1\\N", 1)
			source = strings.Replace(source, "[[Profiles]]", "[[SCPSCURoleSelection]]\n[R]\nRole1 = 1.2.3.1\\"+role+"\n[[ExtendedNegotiation]]\n[E]\nExtendedNegotiation1 = 1.2.3.1\\010002\n[[Profiles]]", 1) + "SCPSCURoleSelection = R\nExtendedNegotiation = E\n"
			path := filepath.Join(t.TempDir(), "synthetic-role-ext.cfg")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			profile, err := parseConfigTest(t, source, AssociationConfigOptions{}).Profile("P", AssociationConfigModeAcceptor)
			if err != nil {
				t.Fatal(err)
			}
			address := startDCMTKAssociationProfileSCP(t, ctx, path, "P")
			contexts := []PresentationContext{{ID: 1, AbstractSyntaxUID: "1.2.3.1", TransferSyntaxUIDs: []string{ExplicitVRLittleEndian}}, {ID: 3, AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ExplicitVRLittleEndian}}}
			for _, requested := range []RoleSelectionItem{{SopClassUID: "1.2.3.1", SCURole: true}, {SopClassUID: "1.2.3.1", SCPRole: true}, {SopClassUID: "1.2.3.1", SCURole: true, SCPRole: true}} {
				extended := []SopClassExtendedNegotiationItem{{SopClassUID: "1.2.3.1", Data: []byte{9, 8}}}
				want, wr, we := probeAssociationProfile(t, ctx, address, contexts, []RoleSelectionItem{requested}, extended)
				got, gr, ge := probeLocalAssociationProfile(t, ctx, profile, contexts, []RoleSelectionItem{requested}, extended)
				policy := profile.Rules[0].Roles
				intersection := requested.SCURole && policy.SCU || requested.SCPRole && policy.SCP
				if !intersection {
					// Existing Go UL rejects the unusable context. DCMTK 3.7.0+DEV
					// accepts it with both role bits zero. Keep this difference explicit.
					if len(got) != 2 || len(want) != 2 || got[0].Result != PresentationContextUserRejection || want[0].Result != PresentationContextAcceptance || !reflect.DeepEqual(got[1], want[1]) || len(gr) != 0 || len(wr) != 1 || wr[0].SCURole || wr[0].SCPRole || len(ge) != 0 || len(we) != 1 || !bytes.Equal(we[0].Data, []byte{1, 0, 2}) {
						t.Fatal("documented no-role-intersection difference changed", got, want, gr, wr, ge, we)
					}
					t.Log("qualified difference: Go rejects unusable PC and omits role/ext; DCMTK accepts PC with role NONE and configured ext")
					continue
				}
				if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gr, wr) || !reflect.DeepEqual(ge, we) {
					t.Fatal("requestor-relative role/opaque ext intersection negotiation differs", got, want, gr, wr, ge, we)
				}
			}
			requested := RoleSelectionItem{SopClassUID: "1.2.3.1", SCURole: role != "SCP", SCPRole: role == "SCP"}
			a, err := DialContext(ctx, address, DialOptions{CalledAETitle: "PROFILE", CallingAETitle: "PROFILETEST", Contexts: contexts, RoleSelections: []RoleSelectionItem{requested}})
			if a != nil {
				_ = a.Close()
			}
			if !errors.Is(err, ErrInvalidUserItem) || !strings.Contains(err.Error(), "accepted extended negotiation for unrequested SOP class") {
				t.Fatal("DCMTK unsolicited reply must be rejected by existing Go UL validation", err)
			}
			_, _, ge := probeLocalAssociationProfile(t, ctx, profile, contexts, []RoleSelectionItem{requested}, nil)
			if len(ge) != 0 {
				t.Fatal("Go must omit unsolicited configured extended replies", ge)
			}
			t.Log("qualified difference: Go omits unsolicited configured extended replies; Go Dial rejects DCMTK's unsolicited reply")
		})
	}
}
