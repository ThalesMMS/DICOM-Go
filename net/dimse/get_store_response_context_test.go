package dimse

import (
	"context"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// The write of a performed C-GET sub-operation's response outlives the end
// of the operation's context and of the association's by
// cGetStoreResponseGrace, and no more: the association's context keeps
// bounding it when the operation's context does not end (#2004).
func TestCGetStoreResponseContextEndsAGraceAfterEitherContext(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endsWith string // which context ends: "operation" or "association"
	}{
		{"the operation's context ends", "operation"},
		{"the association's context ends", "association"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			operation, endOperation := context.WithCancel(context.Background())
			defer endOperation()
			associationCtx, endAssociation := context.WithCancel(context.Background())
			defer endAssociation()
			respond, release := cGetStoreResponseContext(operation, &ul.Association{Context: associationCtx})
			defer release()

			ended := time.Now()
			if tc.endsWith == "operation" {
				endOperation()
			} else {
				endAssociation()
			}
			select {
			case <-respond.Done():
				t.Fatal("the response context ended with the context, without the grace")
			case <-time.After(cGetStoreResponseGrace / 2):
			}
			select {
			case <-respond.Done():
			case <-time.After(5 * cGetStoreResponseGrace):
				t.Fatalf("the response context was still open %s after the %s context ended", 5*cGetStoreResponseGrace+cGetStoreResponseGrace/2, tc.endsWith)
			}
			if waited := time.Since(ended); waited < cGetStoreResponseGrace {
				t.Errorf("the response context ended %s after the %s context, before the grace of %s", waited.Round(time.Millisecond), tc.endsWith, cGetStoreResponseGrace)
			}
		})
	}
}
