package dicomweb

import "testing"

func TestRetrieveRequestAcceptsOriginal(t *testing.T) {
	const explicit = "1.2.840.10008.1.2.1"
	tests := []struct {
		name   string
		req    RetrieveRequest
		syntax string
		want   bool
	}{
		{"direct instance", RetrieveRequest{Level: RetrieveLevelInstance, Accept: []MediaPreference{{MediaType: "application/dicom", Quality: 1}}}, explicit, true},
		{"study requires multipart", RetrieveRequest{Level: RetrieveLevelStudy, Accept: []MediaPreference{{MediaType: "application/dicom", Quality: 1}}}, explicit, false},
		{"study multipart", RetrieveRequest{Level: RetrieveLevelStudy, Accept: []MediaPreference{{MediaType: "application/dicom", Quality: 1, Multipart: true}}}, explicit, true},
		{"explicit syntax mismatch", RetrieveRequest{Level: RetrieveLevelInstance, Accept: []MediaPreference{{MediaType: "application/dicom", TransferSyntaxUID: "1.2.840.10008.1.2.4.80", Quality: 1}}}, explicit, false},
		{"retired implicit", RetrieveRequest{Level: RetrieveLevelInstance, Accept: []MediaPreference{{MediaType: "*/*", TransferSyntaxUID: "*", Quality: 1}}}, "1.2.840.10008.1.2", false},
		{"refused quality", RetrieveRequest{Level: RetrieveLevelInstance, Accept: []MediaPreference{{MediaType: "application/dicom", Quality: 0}}}, explicit, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.req.AcceptsOriginal(tc.syntax); got != tc.want {
				t.Fatalf("AcceptsOriginal=%v want %v", got, tc.want)
			}
		})
	}
}
