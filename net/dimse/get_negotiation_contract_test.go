package dimse

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestCGetWithPartiallyAcceptedStorageContexts(t *testing.T) {
	const mrStorage = "1.2.840.10008.5.1.4.1.1.4"
	const refusedInstance = "1.2.3.10"
	const supportedInstance = "1.2.3.11"
	for _, tc := range []struct {
		name           string
		acceptAbstract bool
		acceptSyntax   bool
		answerRole     bool
	}{
		{name: "abstract syntax refused", acceptSyntax: true},
		{name: "transfer syntax refused", acceptAbstract: true, answerRole: true},
		{name: "storage context accepted without reverse role", acceptAbstract: true, acceptSyntax: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var openedRefused bool
			var delivered []string
			final, err := runStudyRootCGetStatusTest(t, ctx,
				CGetHandlerFunc(func(context.Context, CGetRequestContext) ([]CGetSubOperation, error) {
					return []CGetSubOperation{
						{
							AffectedSOPClassUID:    mrStorage,
							AffectedSOPInstanceUID: refusedInstance,
							LoadDataSet: func(context.Context) (*object.Object, error) {
								openedRefused = true
								return nil, errors.New("ineligible instance should never be opened")
							},
						},
						cGetSubOperation(supportedInstance),
					}, nil
				}),
				func(cfg *cGetStatusTestConfig) {
					syntax := ul.ExplicitVRLittleEndian
					if tc.acceptSyntax {
						syntax = ul.ImplicitVRLittleEndian
					}
					cfg.clientContexts = append(cfg.clientContexts, ul.PresentationContext{
						AbstractSyntaxUID: mrStorage, TransferSyntaxUIDs: []string{syntax},
					})
					role := ul.RoleSelectionItem{SopClassUID: mrStorage, SCPRole: true}
					cfg.clientRoles = append(cfg.clientRoles, role)
					if tc.acceptAbstract {
						cfg.serverAbstract = append(cfg.serverAbstract, mrStorage)
					}
					if tc.answerRole {
						cfg.serverRoles = append(cfg.serverRoles, role)
					}
				},
				withCGetStoreHandler(CGetStoreHandlerFunc(func(_ context.Context, req CGetStoreRequestContext) (uint16, error) {
					delivered = append(delivered, req.Request.AffectedSOPInstanceUID)
					return StatusSuccess, nil
				})),
			)
			if err != nil {
				t.Fatal(err)
			}
			if openedRefused {
				t.Fatal("SCP opened a source without an eligible storage context and role")
			}
			if !slices.Equal(delivered, []string{supportedInstance}) {
				t.Fatalf("delivered UIDs = %v, want only %s", delivered, supportedInstance)
			}
			assertCGetCounts(t, "final", final, StatusCGetSubOperationsCompleteOneOrMoreFailures, 0, 1, 1, 0)
			if !slices.Equal(final.FailedSOPInstanceUIDListOrNil, []string{refusedInstance}) {
				t.Fatalf("failed UID list = %v, want only %s", final.FailedSOPInstanceUIDListOrNil, refusedInstance)
			}
		})
	}
}
