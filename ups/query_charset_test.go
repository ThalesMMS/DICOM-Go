package ups_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/ups"
)

func TestUPSQueryMatchesAcrossCharacterSets(t *testing.T) {
	for _, tc := range []struct {
		queryCharset, candidateCharset string
		rawName                        []byte
	}{
		{"ISO_IR 192", "ISO_IR 100", []byte("GARC\xcdA^PATIENT ")},
		{"ISO_IR 100", "ISO_IR 192", []byte("GARCÍA^PATIENT ")},
	} {
		t.Run(tc.queryCharset, func(t *testing.T) {
			ctx := context.Background()
			store, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			service, err := ups.NewService(store, ups.ServiceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			attributes := scheduledAttributes(t)
			attributes.Put(ups.StringElement(ups.TagSpecificCharacterSet, core.VRCS, tc.candidateCharset))
			attributes.Put(core.NewRawElement(ups.TagPatientName, core.VRPN, tc.rawName))
			if _, err := service.Create(ctx, ups.CreateRequest{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.304", Attributes: attributes}); err != nil {
				t.Fatal(err)
			}
			routes, err := service.QueryRoutes(ups.QuerySCPOptions{MaxScannedSteps: 1})
			if err != nil {
				t.Fatal(err)
			}
			identifier, err := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{
				ups.TagPatientName:          ups.Match("GARCÍA^?AT*"),
				ups.TagSpecificCharacterSet: ups.Match(tc.queryCharset),
			}})
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			syntax := transfer.ExplicitVRLittleEndian
			if err := object.WriteDataSet(&wire, identifier, syntax); err != nil {
				t.Fatal(err)
			}
			request, err := object.ReadDataSetWithOptions(&wire, syntax, object.ReadFileOptions{})
			if err != nil {
				t.Fatal(err)
			}
			matches := 0
			err = routes[0].Handler.Find(ctx, dimse.StreamingCFindRequest{Identifier: request}, func(_ uint16, match *object.Object) error {
				name, ok := match.GetString(ups.TagPatientName)
				if !ok || name != "GARCÍA^PATIENT" {
					t.Fatalf("decoded response name = %q (%t)", name, ok)
				}
				if charset, _ := match.GetString(ups.TagSpecificCharacterSet); charset != tc.candidateCharset {
					t.Fatalf("projected charset = %q, want %q", charset, tc.candidateCharset)
				}
				matches++
				return nil
			})
			if err != nil || matches != 1 {
				t.Fatalf("matching results = %d, error %v", matches, err)
			}
		})
	}
}

func TestUPSQueryRejectsUndecodableMatchingKey(t *testing.T) {
	request := ups.NewDataSet(
		ups.StringElement(ups.TagSpecificCharacterSet, core.VRCS, "ISO_IR 192"),
		core.NewRawElement(ups.TagPatientName, core.VRPN, []byte("GARC\xcdA* ")),
	)
	if _, err := ups.ParseQueryIdentifier(request); !errors.Is(err, ups.ErrQueryIdentifier) {
		t.Fatalf("parse error = %v, want ErrQueryIdentifier", err)
	}
}
