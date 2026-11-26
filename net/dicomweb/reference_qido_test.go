package dicomweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestReferenceQIDOURLValuesRoundTripToMatcher(t *testing.T) {
	const patient = "Renée^A=B&C,#[]+"
	const description = "Crânio, pescoço + 20% #1"
	const id = "A+007&offset=999"
	cases := []struct {
		uid, patient, description, id string
	}{
		{"2.25.7251", patient, description, id},
		{"2.25.7252", "Renee^A=B&C,#[]+", "Cranio, pescoco + 20% #1", "A 007&offset=999"},
		{"2.25.7253", "Mori^Hana=森^花", "胸部 CT %2F", "A+007"},
		{"2.25.7254", "Mori^Hana=森^葉", "胸部 CT /", "A#007"},
	}
	var candidates []*object.Object
	for _, fixture := range cases {
		candidate := searchCandidate(fixture.uid, fixture.patient, "20251023")
		candidate.Put(core.Element{Header: core.ElementHeader{Tag: tags.StudyDescription, VR: core.VRLO}, Value: core.StringValue{fixture.description}})
		candidate.Put(core.Element{Header: core.ElementHeader{Tag: tags.PatientID, VR: core.VRLO}, Value: core.StringValue{fixture.id}})
		candidates = append(candidates, candidate)
	}
	server, err := NewServer(ServerOptions{
		Backend: matcherSearchBackend(nil, candidates), AllowUnauthenticated: true,
		ServiceRoot: "/mounted/qido", SpoolDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(http.StripPrefix("/mounted/qido", server))
	t.Cleanup(host.Close)
	client := Client{Endpoint: Endpoint{BaseURL: host.URL + "/mounted", QIDOPath: "qido"}, Options: Options{HTTPClient: host.Client()}}
	for _, test := range []struct {
		name   string
		params url.Values
		want   []string
	}{
		{"encoded Unicode person name", url.Values{"PatientName": {patient}}, []string{"2.25.7251"}},
		{"encoded Unicode alternate name", url.Values{"PatientName": {"Mori^Hana=森^花"}}, []string{"2.25.7253"}},
		{"description with literal comma plus percent and fragment", url.Values{"StudyDescription": {description}}, []string{"2.25.7251"}},
		{"literal percent escape", url.Values{"StudyDescription": {"胸部 CT %2F"}}, []string{"2.25.7253"}},
		{"slash is distinct from escape", url.Values{"StudyDescription": {"胸部 CT /"}}, []string{"2.25.7254"}},
		{"ampersand and equals are value data", url.Values{"PatientID": {id}, "offset": {"0"}}, []string{"2.25.7251"}},
		{"space is distinct from plus", url.Values{"PatientID": {"A 007&offset=999"}}, []string{"2.25.7252"}},
		{"non-ASCII wildcard", url.Values{"StudyDescription": {"胸部 CT *"}}, []string{"2.25.7253", "2.25.7254"}},
		{"name and UID list intersect", url.Values{"PatientName": {patient}, "StudyInstanceUID": {"2.25.7251,2.25.7252"}}, []string{"2.25.7251"}},
		{"matching name outside UID list", url.Values{"PatientName": {patient}, "StudyInstanceUID": {"2.25.7252,2.25.7253"}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			datasets, err := client.SearchStudies(ctx, test.params)
			if err != nil {
				t.Fatal(err)
			}
			var uids []string
			for _, dataset := range datasets {
				uids = append(uids, dicomjson.ElementString(dataset, "0020000D"))
			}
			if !slices.Equal(uids, test.want) {
				t.Fatalf("matched %v, want %v", uids, test.want)
			}
		})
	}
}
