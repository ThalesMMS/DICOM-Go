package interopcheck_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/interopcheck"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func expectation() interopcheck.Expectation {
	return interopcheck.Expectation{TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID, Dataset: interopcheck.Fixture()}
}

func fixtureFile(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := object.WriteFile(&out, &object.File{Dataset: interopcheck.Fixture(), TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// assertValueFree checks that a mismatch names paths only: reports built from
// it must not echo dataset content.
func assertValueFree(t *testing.T, err error) {
	t.Helper()
	for _, value := range []string{"SYNTHETIC", "Synthetic", "SYNTH01", "20260925"} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("mismatch %q exposes a value", err)
		}
	}
}

func TestVerifyAcceptsTheExpectedFileOnly(t *testing.T) {
	data := fixtureFile(t)
	if err := interopcheck.VerifyBytes(data, expectation()); err != nil {
		t.Fatalf("exact file: %v", err)
	}
	want := expectation()
	want.TransferSyntaxUID = transfer.ImplicitVRLittleEndian.UID
	if reason := interopcheck.ReasonOf(interopcheck.VerifyBytes(data, want)); reason != interopcheck.ReasonTransferSyntax {
		t.Fatalf("unexpected syntax gave %q", reason)
	}
	if reason := interopcheck.ReasonOf(interopcheck.VerifyBytes(data[:len(data)/2], expectation())); reason == "" {
		t.Fatal("truncated file accepted")
	}
	other := interopcheck.Fixture()
	other.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.945.9"}})
	if err := interopcheck.VerifyBytes(data, interopcheck.Expectation{TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID, Dataset: other}); interopcheck.ReasonOf(err) != interopcheck.ReasonIdentity {
		t.Fatalf("identity mismatch gave %v", err)
	}
}

// Every negative control parses as a well-formed file; only the comparison
// with the expectation rejects it, with its own reason.
func TestNegativeControlsAreRejected(t *testing.T) {
	for _, mutation := range interopcheck.Mutations() {
		t.Run(string(mutation), func(t *testing.T) {
			data, err := mutation.Apply(interopcheck.Fixture())
			if err != nil {
				t.Fatal(err)
			}
			err = interopcheck.VerifyBytes(data, expectation())
			if got := interopcheck.ReasonOf(err); got != mutation.Want() {
				t.Fatalf("reason %q (%v), want %q", got, err, mutation.Want())
			}
			assertValueFree(t, err)
			if mutation == interopcheck.MutationRelabelSyntax {
				return
			}
			// The mutated file is accepted by the parser: parsing alone is no gate.
			file, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("control is not a readable file: %v", err)
			}
			if err := interopcheck.VerifyDataSet(file.Dataset, interopcheck.Fixture()); interopcheck.ReasonOf(err) != mutation.Want() {
				t.Fatalf("dataset reason %v, want %q", err, mutation.Want())
			}
		})
	}
}

// A receiver that answers success but persists a mutated object fails the
// gate: the DIMSE status and a completed local roundtrip prove nothing about
// the content.
func TestNegativeControlsFailDespiteSuccessfulStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	go func() {
		defer close(served)
		for {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "TAMPER", Context: ctx,
				SupportedAbstractSyntaxes: []string{interopcheck.FixtureSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}})
			if err != nil {
				return
			}
			mode := interopcheck.Mutation(strings.ToLower(assoc.CallingAETitle))
			err = dimse.ServeStorageAssociation(ctx, assoc, dimse.StorageSCPOptions{StoreHandler: dimse.CStoreHandlerFunc(func(_ context.Context, req dimse.CStoreRequestContext) (uint16, error) {
				data, err := mode.Apply(req.DataSet)
				if err != nil {
					return 0xA700, err
				}
				if err := os.WriteFile(filepath.Join(dir, string(mode)+".dcm"), data, 0o600); err != nil {
					return 0xA700, err
				}
				return dimse.StatusSuccess, nil
			})})
			_ = assoc.Close()
			if err != nil {
				served <- err
				return
			}
		}
	}()
	for _, mutation := range interopcheck.Mutations() {
		assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CallingAETitle: strings.ToUpper(string(mutation)), CalledAETitle: "TAMPER",
			Contexts: []ul.PresentationContext{{AbstractSyntaxUID: interopcheck.FixtureSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
		if err != nil {
			t.Fatal(err)
		}
		response, err := dimse.NewStoreClient(assoc).Store(ctx, interopcheck.Fixture())
		if err != nil || response.Status != dimse.StatusSuccess {
			t.Fatalf("%s: store %v", mutation, err)
		}
		if err := assoc.Release(ctx); err != nil {
			t.Fatal(err)
		}
		err = interopcheck.VerifyFile(filepath.Join(dir, string(mutation)+".dcm"), expectation())
		if interopcheck.ReasonOf(err) != mutation.Want() {
			t.Fatalf("%s: success status with tampered content gave %v", mutation, err)
		}
	}
	_ = listener.Close()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
