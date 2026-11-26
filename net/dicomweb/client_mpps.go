package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	mppsMediaType           = string(MetadataMediaTypeDICOMJSON)
	mppsMaxBodyBytes        = 8 << 20
	mppsSOPClassUID         = "1.2.840.10008.3.1.2.3.3"
	mppsRetrieveSOPClassUID = "1.2.840.10008.3.1.2.3.4"
)

// MPPSURL builds the PS3.18 §15 resource for an individual Modality
// Performed Procedure Step under the configured base URL.
func (e Endpoint) MPPSURL(sopInstanceUID string) (*url.URL, error) {
	if !core.IsValidUID(sopInstanceUID) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: errors.New("a valid MPPS UID is required")}
	}
	return e.serviceURL("", []string{"modality-performed-procedure-steps", sopInstanceUID}, nil)
}

// CreateMPPS reports a new IN PROGRESS step with the caller's SOP Instance UID.
// Provide the N-CREATE attribute list from mpps.BuildCreate. The server
// confirms creation with HTTP 201 and an empty response payload.
func (c Client) CreateMPPS(ctx context.Context, sopInstanceUID string, attributes *object.Object) error {
	return c.writeMPPS(ctx, sopInstanceUID, attributes, http.StatusCreated)
}

// UpdateMPPS updates an existing step with one DICOM JSON modification list.
// Provide the N-SET list from mpps.BuildSet, Complete or Discontinue. The server
// confirms the change with HTTP 200.
func (c Client) UpdateMPPS(ctx context.Context, sopInstanceUID string, modifications *object.Object) error {
	return c.writeMPPS(ctx, sopInstanceUID, modifications, http.StatusOK)
}

func (c Client) writeMPPS(ctx context.Context, sopInstanceUID string, attributes *object.Object, expected int) error {
	resource, err := c.Endpoint.MPPSURL(sopInstanceUID)
	if err != nil {
		return err
	}
	if attributes == nil || attributes.Len() == 0 {
		return newDICOMwebError(ErrorKindRequestFailure, resource.String(), 0, errors.New("MPPS requires a DICOM attribute list"))
	}
	if err := verifyMPPSObjectIdentity(attributes, sopInstanceUID, false); err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, resource.String(), 0, err)
	}
	target := *resource
	if expected == http.StatusOK {
		target.RawQuery = "update"
	}
	data, err := dicomjson.MarshalContext(contextOrBackground(ctx), attributes, dicomjson.Options{
		Limits: dicomjson.Limits{MaxJSONBytes: mppsMaxBodyBytes},
	})
	if err != nil {
		return newDICOMwebError(ErrorKindRequestFailure, resource.String(), 0, err)
	}
	resp, err := c.mppsClient().doRequest(ctx, http.MethodPost, &target, bytes.NewReader(data), mppsMediaType, mppsMediaType)
	if err != nil {
		return err
	}
	if resp.StatusCode != expected {
		return statusError(resp)
	}
	if err := verifyMPPSLocation(resource, resp); err != nil {
		return err
	}
	if len(bytes.TrimSpace(resp.Body)) != 0 {
		return newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, errors.New("unexpected MPPS success response payload"))
	}
	return contextOrBackground(ctx).Err()
}

// RetrieveMPPS reads a step's DICOM JSON representation on servers that
// implement the optional Retrieve transaction. The request is uncached.
func (c Client) RetrieveMPPS(ctx context.Context, sopInstanceUID string) (*object.Object, error) {
	resource, err := c.Endpoint.MPPSURL(sopInstanceUID)
	if err != nil {
		return nil, err
	}
	resp, err := c.mppsClient().doGETUncached(ctx, resource, mppsMediaType)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	if err := verifyMPPSLocation(resource, resp); err != nil {
		return nil, err
	}
	dataset, err := decodeMPPSDataset(contextOrBackground(ctx), resp, sopInstanceUID)
	if err != nil {
		return nil, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	return dataset, nil
}

func (c Client) mppsClient() Client {
	underlying := c.Options.HTTPClient
	if underlying == nil {
		underlying = sourceport.HTTPClient()
	}
	isolated := *underlying
	// Even a same-origin redirect could change the operation or the step.
	isolated.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	c.Options.HTTPClient = &isolated
	c.Options.Retry = nil
	if c.Options.MaxBodyBytes <= 0 || c.Options.MaxBodyBytes > mppsMaxBodyBytes {
		c.Options.MaxBodyBytes = mppsMaxBodyBytes
	}
	return c
}

func verifyMPPSLocation(resource *url.URL, response Response) error {
	for _, name := range []string{"Location", "Content-Location"} {
		locations := response.Header.Values(name)
		if len(locations) == 0 {
			continue
		}
		if len(locations) == 1 && mppsLocationMatches(resource, locations[0]) {
			continue
		}
		return newDICOMwebError(ErrorKindDecodeResponse, response.URL, response.StatusCode,
			fmt.Errorf("MPPS %s does not identify the requested step", name))
	}
	return nil
}

func mppsLocationMatches(resource *url.URL, value string) bool {
	location, err := url.Parse(strings.TrimSpace(value))
	if err != nil || value == "" || location.User != nil || location.Opaque != "" ||
		location.RawQuery != "" || location.ForceQuery || strings.Contains(value, "#") {
		return false
	}
	target := resource.ResolveReference(location)
	return strings.EqualFold(target.Scheme, resource.Scheme) &&
		strings.EqualFold(target.Host, resource.Host) &&
		target.EscapedPath() == resource.EscapedPath()
}

func verifyMPPSObjectIdentity(dataset *object.Object, sopInstanceUID string, retrieved bool) error {
	for _, identity := range [...]struct {
		tag   core.Tag
		value string
	}{{core.NewTag(0x0008, 0x0018), sopInstanceUID}, {core.NewTag(0x0008, 0x0016), mppsSOPClassUID}} {
		if element, present := dataset.Get(identity.tag); present {
			values, err := dataset.LookupStrings(identity.tag)
			if err != nil || element.VR() != core.VRUI || len(values) != 1 {
				return errors.New("MPPS attribute list contains a malformed SOP UID")
			}
			if values[0] != identity.value && !(retrieved && identity.value == mppsSOPClassUID && values[0] == mppsRetrieveSOPClassUID) {
				return errors.New("MPPS attribute list identifies a different resource")
			}
		}
	}
	return nil
}

func decodeMPPSDataset(ctx context.Context, response Response, sopInstanceUID string) (*object.Object, error) {
	contentType, err := responseContentType(response.Header.Values("Content-Type"))
	if err != nil {
		return nil, err
	}
	media, params, err := parseResponseMediaType(contentType)
	if err != nil || media != mppsMediaType || !metadataParametersSupported(params, "charset") ||
		!metadataCharsetSupported(params) {
		return nil, &ResponseDecodeError{Kind: ResponseDecodeUnexpectedMedia}
	}
	body := bytes.TrimSpace(response.Body)
	decoder := dicomjson.NewDecoder(bytes.NewReader(body), std.Dictionary, dicomjson.DefaultUnmarshalOptions())
	var dataset *object.Object
	if len(body) != 0 && body[0] == '[' {
		if err := decoder.DecodeArrayContext(ctx, func(candidate *object.Object) error {
			if dataset != nil {
				return errors.New("MPPS response contains more than one dataset")
			}
			dataset = candidate
			return nil
		}); err != nil {
			return nil, err
		}
	} else {
		dataset, err = decoder.DecodeContext(ctx)
		if err != nil {
			return nil, err
		}
	}
	if dataset == nil {
		return nil, errors.New("MPPS response contains no dataset")
	}
	// PS3.4's Retrieve attribute list can omit SOP Instance/Class UID.
	// Identifiers supplied in a response must agree with the addressed step.
	if err := verifyMPPSObjectIdentity(dataset, sopInstanceUID, true); err != nil {
		return nil, err
	}
	stateTag := core.NewTag(0x0040, 0x0252)
	state, _ := dataset.Get(stateTag)
	values, err := dataset.LookupStrings(stateTag)
	if state.VR() != core.VRCS || err != nil || len(values) != 1 {
		return nil, errors.New("MPPS response must provide one Performed Procedure Step Status")
	}
	switch values[0] {
	case "IN PROGRESS", "COMPLETED", "DISCONTINUED":
		return dataset, nil
	default:
		return nil, errors.New("invalid Performed Procedure Step Status")
	}
}
