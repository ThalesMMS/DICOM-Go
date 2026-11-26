package ul

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const registryCT = "1.2.840.10008.5.1.4.1.1.2"
const registryMR = "1.2.840.10008.5.1.4.1.1.4"

func registryFixture(t *testing.T) *CalledAERegistry {
	t.Helper()
	r, err := NewCalledAERegistry([]CalledAEProfile{
		{AETitle: "CT_SCP", Profile: AssociationProfile{Rules: []PresentationContextRule{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxes: []string{ExplicitVRLittleEndian}, Roles: &RequestorRoles{SCU: true}}, {AbstractSyntaxUID: registryCT, TransferSyntaxes: []string{ExplicitVRLittleEndian}, Roles: &RequestorRoles{SCU: true}}}}},
		{AETitle: "MR_SCP", Profile: AssociationProfile{Rules: []PresentationContextRule{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxes: []string{ImplicitVRLittleEndian}, Roles: &RequestorRoles{SCU: true}}, {AbstractSyntaxUID: registryMR, TransferSyntaxes: []string{ImplicitVRLittleEndian}, Roles: &RequestorRoles{SCU: true}}}}},
	}, CalledAERegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestCalledAERegistryPreflightAndDetached(t *testing.T) {
	r := registryFixture(t)
	input := r.Profiles()
	frozen, err := NewCalledAERegistry(input, CalledAERegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	input[0].AETitle = "CHANGED"
	input[0].Profile.Rules[0].TransferSyntaxes[0] = "1.2"
	input[0].Profile.Rules[0].Roles.SCU = false
	view := frozen.Profiles()
	view[0].Profile.Rules[0].Roles.SCU = false
	original, found := frozen.lookup(" CT_SCP ")
	if !found || original.Profile.Rules[0].TransferSyntaxes[0] != ExplicitVRLittleEndian || !original.Profile.Rules[0].Roles.SCU {
		t.Fatal("registry aliased input/view")
	}
	if _, found = frozen.lookup("ct_scp"); found {
		t.Fatal("AE lookup folded case")
	}
	for _, options := range []CalledAERegistryOptions{{MaxAEs: 1}, {MaxRules: 3}, {MaxTransferSyntaxes: 3}, {MaxCompiledBytes: 100}, {MaxRules: -1}} {
		if _, err = NewCalledAERegistry(r.Profiles(), options); !errors.Is(err, ErrCalledAERegistryLimit) {
			t.Fatalf("quota %+v: %v", options, err)
		}
	}
	duplicate := r.Profiles()
	duplicate[1].AETitle = " CT_SCP "
	if _, err = NewCalledAERegistry(duplicate, CalledAERegistryOptions{}); !errors.Is(err, ErrCalledAERegistry) {
		t.Fatalf("duplicate %v", err)
	}
}
func TestCalledAERegistryExclusiveOptions(t *testing.T) {
	r := registryFixture(t)
	for _, o := range []AcceptOptions{{SupportedAbstractSyntaxes: []string{registryCT}}, {SupportedTransferSyntaxes: []string{ExplicitVRLittleEndian}}, {AcceptAnyAbstractSyntax: true}, {AlwaysAcceptDefaultRole: true}, {RequireMatchingCalledAE: true}, {PresentationContextRules: []PresentationContextRule{{AbstractSyntaxUID: registryCT}}}, {RoleSelections: []RoleSelectionItem{{SopClassUID: registryCT, SCURole: true}}}, {CommonExtendedNegotiationHandler: AcceptRelatedGeneralSOPClasses}, {CalledAEPresentationContextRules: map[string][]PresentationContextRule{"CT_SCP": {{AbstractSyntaxUID: registryCT}}}}} {
		o.CalledAERegistry = r
		if err := validateCalledAEOptions(o); !errors.Is(err, ErrCalledAERegistry) {
			t.Fatalf("conflicting options accepted %+v", o)
		}
	}
}

func TestCalledAERegistryAcceptsEquivalentAcceptorRoleDeclarations(t *testing.T) {
	p := registryFixture(t).Profiles()
	p[0].Profile.RoleSelections = []RoleSelectionItem{{SopClassUID: registryCT, SCURole: true}}
	r, err := NewCalledAERegistry(p, CalledAERegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Profiles()[0].Profile.RoleSelections) != 0 || !r.Profiles()[0].Profile.Rules[1].Roles.SCU {
		t.Fatal("profile lost normalized role policy")
	}
	p[0].Profile.RoleSelections[0].SCPRole = true
	if _, err = NewCalledAERegistry(p, CalledAERegistryOptions{}); !errors.Is(err, ErrCalledAERegistry) {
		t.Fatalf("conflicting explicit roles %v", err)
	}
}

func TestCalledAERegistryLocalSCURequiresProposedSCPRole(t *testing.T) {
	r, err := NewCalledAERegistry([]CalledAEProfile{{AETitle: "ACCEPTOR", Profile: AssociationProfile{Rules: []PresentationContextRule{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxes: []string{ImplicitVRLittleEndian}, Roles: &RequestorRoles{SCP: true}}}}}}, CalledAERegistryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		role   *RoleSelectionItem
		reject bool
	}{{"omitted default SCU", nil, true}, {"requestor SCU", &RoleSelectionItem{SopClassUID: verificationSOPClassUID, SCURole: true}, true}, {"requestor SCP", &RoleSelectionItem{SopClassUID: verificationSOPClassUID, SCPRole: true}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := dialWithRole(t, AcceptOptions{CalledAERegistry: r, ARTIMTimeout: -1}, tc.role)
			if tc.reject {
				if !errors.Is(err, ErrAssociationRejected) {
					t.Fatalf("role error %v", err)
				}
				return
			}
			if err != nil || len(got) != 1 || got[0].SCURole || !got[0].SCPRole {
				t.Fatalf("role reply %+v/%v", got, err)
			}
		})
	}
}
func TestCalledAERegistryLoopbackIsolationAndAdmission(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	var admission, identity, handlers atomic.Int32
	server, err := NewAssociationServer(listener, AssociationServerOptions{Accept: AcceptOptions{CalledAERegistry: registryFixture(t), ARTIMTimeout: -1, Admission: func(context.Context, AdmissionRequest) *AssociationRJ { admission.Add(1); return nil }, UserIdentityHandler: func(UserIdentityRequest) (UserIdentityResult, error) {
		identity.Add(1)
		return UserIdentityResult{Action: UserIdentityActionAccept}, nil
	}}, Handler: func(ctx context.Context, a *Association) error {
		handlers.Add(1)
		defer a.Close()
		p, err := a.Receive(ctx)
		if err == nil {
			if _, ok := p.(*ReleaseRQ); ok {
				return a.Send(ctx, &ReleaseRP{})
			}
		}
		return err
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		shutdown, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		_ = server.Shutdown(shutdown)
		<-done
	})
	for _, title := range []string{"UNKNOWN", "ct_scp"} {
		_, err = DialContext(ctx, listener.Addr().String(), DialOptions{CalledAETitle: title, CallingAETitle: "SYNTHETIC", UserIdentity: &UserIdentityRequest{Type: 1, PrimaryField: []byte("synthetic")}, Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ExplicitVRLittleEndian}}}})
		var rejection *RejectionError
		if !errors.As(err, &rejection) || rejection.Result != AssociateRJResultPermanent || rejection.Source != AssociateRJSourceServiceUser || rejection.Reason != AssociateRJReasonCalledAETitleNotRecognized {
			t.Fatalf("unknown AE: %v", err)
		}
	}
	if admission.Load() != 0 || identity.Load() != 0 || handlers.Load() != 0 {
		t.Fatal("unknown AE reached callbacks")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			title, sop, other, ts, wrong := "CT_SCP", registryCT, registryMR, ExplicitVRLittleEndian, ImplicitVRLittleEndian
			if i%2 == 1 {
				title, sop, other, ts, wrong = "MR_SCP", registryMR, registryCT, ImplicitVRLittleEndian, ExplicitVRLittleEndian
			}
			a, e := DialContext(ctx, listener.Addr().String(), DialOptions{CalledAETitle: title, CallingAETitle: "SYNTHETIC", UserIdentity: &UserIdentityRequest{Type: 1, PrimaryField: []byte("synthetic")}, Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ts}}, {AbstractSyntaxUID: sop, TransferSyntaxUIDs: []string{ts}}, {AbstractSyntaxUID: other, TransferSyntaxUIDs: []string{ts}}, {AbstractSyntaxUID: sop, TransferSyntaxUIDs: []string{wrong}}, {AbstractSyntaxUID: sop, TransferSyntaxUIDs: []string{ts}}}, RoleSelections: []RoleSelectionItem{{SopClassUID: sop, SCURole: true}}})
			if e != nil {
				errs <- e
				return
			}
			outcomes := a.PresentationContextOutcomes()
			want := []byte{0, 0, 3, 4, 0}
			if len(outcomes) != len(want) {
				errs <- errors.New("outcome count")
			} else {
				for j, o := range outcomes {
					if o.Result != want[j] || o.Result == 0 && o.TransferSyntaxUID != ts {
						errs <- errors.New("catalog crossed AEs")
						break
					}
				}
			}
			if e = a.Release(ctx); e != nil {
				errs <- e
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if admission.Load() != 12 || identity.Load() != 12 || handlers.Load() != 12 {
		t.Fatalf("callbacks admission=%d identity=%d handler=%d", admission.Load(), identity.Load(), handlers.Load())
	}
	// An accepted Verification context keeps the role rejection observable.
	a, err := DialContext(ctx, listener.Addr().String(), DialOptions{CalledAETitle: "CT_SCP", CallingAETitle: "SYNTHETIC", Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ExplicitVRLittleEndian}}, {AbstractSyntaxUID: registryCT, TransferSyntaxUIDs: []string{ExplicitVRLittleEndian}}}, RoleSelections: []RoleSelectionItem{{SopClassUID: registryCT, SCPRole: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.PresentationContextOutcomes()[1].Result; got != PresentationContextUserRejection {
		t.Fatalf("wrong role result %d", got)
	}
	if err = a.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
