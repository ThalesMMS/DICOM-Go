package dimse

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DCMTK dcmqrdb/tests/tqrlevel.cc uses an oversized level to exercise
// rejection and lowercase STUDY to exercise case normalization. Put the
// values directly on the wire without the validating query builder.
//
// A missing, blank or unknown level makes the Identifier unusable for the
// Information Model. PS3.4 C.4.1.1.4 reports that as A900 (Identifier does
// not match SOP Class); C000 (Unable to process) is kept for failures of the
// provider itself. The Error Comment names the offending key. DCMTK 3.7.0
// dcmqrscp answers A900 for a missing level but C000 for an unknown one;
// this SCP follows PS3.4 for both.
func TestDCMTKReferenceCFindQueryRetrieveLevel(t *testing.T) {
	const (
		missingComment     = "Missing Query/Retrieve Level (0008,0052)"
		unsupportedComment = "Unsupported Query/Retrieve Level (0008,0052)"
	)
	for _, tc := range []struct {
		name           string
		wire           string
		wantLevel      string
		wantDiagnostic string
		wantComment    string
	}{
		{"lowercase study", "study", "STUDY", "", ""},
		{"mixed case and padded study", " StUdY  ", "STUDY", "", ""},
		{"lowercase series", "series", "SERIES", "", ""},
		{"lowercase image", "image", "IMAGE", "", ""},
		{"blank level", "  ", "", "missing QueryRetrieveLevel", missingComment},
		{"missing level", "", "", "missing QueryRetrieveLevel", missingComment},
		{"oversized level from DCMTK", strings.Repeat("a", 60), "", "unsupported QueryRetrieveLevel", unsupportedComment},
		{"recognized prefix with excess value", "STUDY" + strings.Repeat("A", 59), "", "unsupported QueryRetrieveLevel", unsupportedComment},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			called := make(chan string, 2)
			handler := CFindHandlerFunc(func(_ context.Context, req CFindRequestContext) ([]*object.Object, error) {
				select {
				case called <- req.QueryRetrieveLevel:
					return nil, nil
				default:
					return nil, fmt.Errorf("unexpected additional C-FIND handler invocation")
				}
			})
			setWireLevel := func(elems []core.Element) []core.Element {
				if tc.wire == "" {
					return slices.DeleteFunc(elems, func(elem core.Element) bool { return elem.Header.Tag == tags.QueryRetrieveLevel })
				}
				// A peer can exceed the VR's declared character limit. Raw bytes
				// make the SCP check the full value it actually receives.
				elems[0] = core.NewRawElement(tags.QueryRetrieveLevel, core.VRCS, []byte(tc.wire))
				return elems
			}
			rsp, err := runStudyRootCFindResponseTest(t, ctx, 0, handler, setWireLevel)
			status := rsp.Status
			if tc.wantDiagnostic != "" {
				if status != CFindStatusIdentifierDoesNotMatchSOPClass || err == nil || !strings.Contains(err.Error(), tc.wantDiagnostic) {
					t.Fatalf("status = 0x%04X, server error = %v, want 0xA900 with %q", status, err, tc.wantDiagnostic)
				}
				if rsp.ErrorComment != tc.wantComment {
					t.Fatalf("Error Comment = %q, want %q", rsp.ErrorComment, tc.wantComment)
				}
				select {
				case got := <-called:
					t.Fatalf("invalid level reached the handler as %q", got)
				default:
				}
				return
			}
			if status != StatusSuccess || err != nil {
				t.Fatalf("status = 0x%04X, server error = %v, want success", status, err)
			}
			select {
			case got := <-called:
				if got != tc.wantLevel {
					t.Fatalf("handler level = %q, want %q", got, tc.wantLevel)
				}
			default:
				t.Fatal("valid level never reached the handler")
			}
			select {
			case got := <-called:
				t.Fatalf("C-FIND reached the handler more than once, extra level %q", got)
			default:
			}
		})
	}
}

// DCMTK dcmnet/tests/tscuscp.cc checks receive nesting depth and per-element
// limits. Add boundary cases and send a healthy request on the same association
// to exercise discarding every outstanding PDV after an early refusal.
func TestDCMTKReferenceStoreParserLimitsDrainOnWire(t *testing.T) {
	const depthLimit = 4 // Two sequences, each with one item.
	const elementLimit = 5000
	for _, mode := range []struct {
		name       string
		options    StorageSCPOptions
		measure    string
		boundaries []int
		fixture    func(int) core.Element
	}{
		{
			name:       "nested sequences",
			options:    StorageSCPOptions{MaxSequenceDepth: depthLimit},
			measure:    "nested sequence count",
			boundaries: []int{1, 2, 3},
			fixture:    dcmtkReferenceNestedElement,
		},
		{
			name:       "element length",
			options:    StorageSCPOptions{MaxElementBytes: elementLimit},
			measure:    "value bytes",
			boundaries: []int{elementLimit - 2, elementLimit, elementLimit + 2},
			fixture: func(size int) core.Element {
				return core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, bytes.Repeat([]byte{0x8a}, size))
			},
		},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for i, boundary := range mode.boundaries {
				t.Run(fmt.Sprintf("%d %s", boundary, mode.measure), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					accepted := make(chan string, 2)
					opts := mode.options
					opts.StoreHandler = CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
						select {
						case accepted <- req.Request.AffectedSOPInstanceUID:
							return StatusSuccess, nil
						default:
							return StatusCStoreOutOfResources, fmt.Errorf("unexpected additional C-STORE handler invocation")
						}
					})
					addr, done := startStreamSCP(t, ctx, opts)
					assoc := dialStoreSCU(t, ctx, addr)
					defer func() { _ = assoc.Close() }()

					fixture := readIntegrationFixture(t)
					fixture.Dataset.Put(mode.fixture(boundary))
					// Two elements after the tested one create more PDVs without
					// triggering the element limit themselves.
					for _, tag := range []core.Tag{core.NewTag(0x7FE1, 0x1010), core.NewTag(0x7FE1, 0x1011)} {
						fixture.Dataset.Put(core.NewRawElement(tag, core.VROB, make([]byte, 3200)))
					}
					var wire bytes.Buffer
					if err := object.WriteDataSet(&wire, fixture.Dataset, transfer.ExplicitVRLittleEndian); err != nil {
						t.Fatal(err)
					}
					first := dicomtest.TestSOPInstanceUID
					wantStatus := StatusSuccess
					wantHandlers := []string{first}
					if i == len(mode.boundaries)-1 {
						wantStatus = StatusCStoreOutOfResources
						wantHandlers = nil
					}
					if result := storeRaw(t, ctx, assoc, &wire, first); result.Response.Status != wantStatus {
						t.Fatalf("limited C-STORE status = %04X, want %04X", result.Response.Status, wantStatus)
					}

					healthy := readIntegrationFixture(t).Dataset
					second := first + ".1"
					if err := healthy.SetStrings(tags.SOPInstanceUID, core.VRUI, second); err != nil {
						t.Fatal(err)
					}
					response, err := NewStoreClient(assoc).Store(ctx, healthy)
					if err != nil || response.Status != StatusSuccess {
						t.Fatalf("following healthy C-STORE result = (%+v, %v)", response, err)
					}
					if err := assoc.Release(ctx); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					close(accepted)
					var gotHandlers []string
					for uid := range accepted {
						gotHandlers = append(gotHandlers, uid)
					}
					wantHandlers = append(wantHandlers, second)
					if !slices.Equal(gotHandlers, wantHandlers) {
						t.Fatalf("handler received %v, want %v", gotHandlers, wantHandlers)
					}
				})
			}
		})
	}
}

func dcmtkReferenceNestedElement(depth int) core.Element {
	elem := core.NewRawElement(core.NewTag(0x0008, 0x0100), core.VRSH, []byte("CODE"))
	for range depth {
		elem = core.Element{
			Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x2218), VR: core.VRSQ},
			Value:  core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{elem}}}},
		}
	}
	return elem
}

// DCMTK dcmqrdb/tests/tdispatch.cc exercises attempts to store through
// Verification, C-FIND, and C-GET contexts. A well-formed dataset with matching
// command UIDs still cannot enter the Storage handler through those contexts.
func TestDCMTKReferenceStoreRejectedOnNonStorageContexts(t *testing.T) {
	for _, tc := range []struct{ name, sopClass string }{
		{"verification", VerificationSOPClassUID},
		{"study root query", StudyRootFindSOPClassUID},
		{"study root retrieve", StudyRootGetSOPClassUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			const class = dicomtest.TestSOPClassUID
			const instance = dicomtest.TestSOPInstanceUID
			accepted := make(chan uint16, 2)
			assoc, done := startDCMTKReferenceSCP(t, ctx, []string{class, tc.sopClass}, AssociationSCPOptions{
				StorageSCPOptions: StorageSCPOptions{
					StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
						select {
						case accepted <- req.Request.MessageID:
							return StatusSuccess, nil
						default:
							return StatusCStoreOutOfResources, fmt.Errorf("unexpected additional C-STORE handler invocation")
						}
					}),
				},
			})
			dataset := readIntegrationFixture(t).Dataset
			for _, request := range []struct {
				sopClass  string
				messageID uint16
				status    uint16
			}{
				{tc.sopClass, 1, StatusCStoreDataSetDoesNotMatch},
				{class, 2, StatusSuccess},
			} {
				pc, err := AcceptedContextForSOPClass(assoc, request.sopClass)
				if err != nil {
					t.Fatal(err)
				}
				if err := SendCStoreRequestWithContext(ctx, assoc, pc.ID, CStoreRequest{
					MessageID: request.messageID, AffectedSOPClassUID: class, AffectedSOPInstanceUID: instance,
				}); err != nil {
					t.Fatal(err)
				}
				if err := SendDataSetWithContext(ctx, assoc, pc.ID, dataset, transfer.ExplicitVRLittleEndian); err != nil {
					t.Fatal(err)
				}
				rsp, err := ReceiveCStoreResponseWithContext(ctx, assoc, pc.ID)
				if err != nil {
					t.Fatal(err)
				}
				if rsp.Status != request.status || rsp.MessageIDBeingRespondedTo != request.messageID {
					t.Fatalf("context SOP %s got response %+v, want status 0x%04X for message %d", request.sopClass, rsp, request.status, request.messageID)
				}
			}
			if err := assoc.Release(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			close(accepted)
			var calls []uint16
			for id := range accepted {
				calls = append(calls, id)
			}
			if !slices.Equal(calls, []uint16{2}) {
				t.Fatalf("storage handler received messages %v, want only message 2", calls)
			}
		})
	}
}

func startDCMTKReferenceSCP(t *testing.T, ctx context.Context, sopClasses []string, options AssociationSCPOptions) (*ul.Association, <-chan error) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "ARCHIVE",
			Context:                   ctx,
			SupportedAbstractSyntaxes: sopClasses,
			SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID},
		})
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		done <- ServeAssociation(ctx, assoc, options)
	}()
	var contexts []ul.PresentationContext
	for _, sopClass := range sopClasses {
		contexts = append(contexts, ul.PresentationContext{
			AbstractSyntaxUID:  sopClass,
			TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID},
		})
	}
	client, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "ARCHIVE", CallingAETitle: "STORESCU", Contexts: contexts,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, done
}
