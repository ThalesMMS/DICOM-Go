package dicomweb

import (
	"errors"
	"testing"
)

func TestBulkDataURLFromMetadata(t *testing.T) {
	endpoint := Endpoint{BaseURL: "https://pacs.example.invalid:8443/dicomweb"}
	study := endpoint.BaseURL + "/studies/2.25.1"
	series := study + "/series/2.25.2"
	instance := series + "/instances/2.25.3"
	for _, test := range []struct {
		name  string
		base  string
		value string
		want  string
	}{
		{"study", study + "/metadata?request=1", "./bulkdata/00282000", study + "/bulkdata/00282000"},
		{"series", series + "/metadata", "bulkdata/00420011", series + "/bulkdata/00420011"},
		{"instance", instance + "/metadata", "./bulkdata/7FE00010?grant=test#fragment", instance + "/bulkdata/7FE00010?grant=test"},
		{"absolute path", study + "/metadata", "/other/bulk/value", "https://pacs.example.invalid:8443/other/bulk/value"},
		{"absolute URL", study + "/metadata", instance + "/pixeldata", instance + "/pixeldata"},
		{"absolute foreign value", study + "/metadata", "https://elsewhere.example.invalid/bulk", ""},
		{"port in value", study + "/metadata", "https://pacs.example.invalid/bulk", ""},
		{"relative foreign value", study + "/metadata", "//elsewhere.example.invalid/bulk", ""},
		{"userinfo in value", study + "/metadata", "https://user@pacs.example.invalid:8443/bulk", ""},
		{"foreign base", "https://elsewhere.example.invalid/metadata", instance + "/pixeldata", ""},
		{"different scheme in base", "http://pacs.example.invalid:8443/dicomweb/metadata", "bulkdata/7FE00010", ""},
		{"userinfo in base", "https://user@pacs.example.invalid:8443/dicomweb/metadata", "bulkdata/7FE00010", ""},
		{"fragment in base", study + "/metadata#fragment", "bulkdata/7FE00010", ""},
		{"relative base", "/dicomweb/studies/2.25.1/metadata", "bulkdata/7FE00010", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := endpoint.BulkDataURLFromMetadata(test.base, test.value)
			if test.want == "" {
				var webError *Error
				if got != nil || !errors.As(err, &webError) || webError.Kind != ErrorKindInvalidEndpoint {
					t.Fatalf("BulkDataURLFromMetadata = %v, %v; want invalid endpoint", got, err)
				}
				return
			}
			if err != nil || got.String() != test.want {
				t.Fatalf("BulkDataURLFromMetadata = %v, %v; want %s", got, err, test.want)
			}
		})
	}
}
