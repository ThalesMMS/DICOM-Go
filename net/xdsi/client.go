package xdsi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

// Limits caps received documents, wire bytes and logical DICOM metadata.
type Limits struct {
	DocumentBytes, TotalBytes, MetadataBytes int64
}

func normalizeLimits(l Limits) (Limits, error) {
	if l.DocumentBytes == 0 {
		l.DocumentBytes = 64 << 20
	}
	if l.TotalBytes == 0 {
		l.TotalBytes = 128 << 20
	}
	if l.MetadataBytes == 0 {
		l.MetadataBytes = 4 << 20
	}
	if l.DocumentBytes < 1 || l.DocumentBytes > 1<<30 || l.TotalBytes < l.DocumentBytes ||
		l.TotalBytes > 1<<30 || l.MetadataBytes < 1 || l.MetadataBytes > l.DocumentBytes {
		return l, ErrLimit
	}
	return l, nil
}

type Operation string

const (
	Publish  Operation = "RAD-68"
	Retrieve Operation = "RAD-69"
)

// ClientConfig selects explicit repository and imaging-source peers. The
// HTTPClient supplies deployment-specific TLS and credentials. Redirects are
// refused. Mapping resolves a local patient into the affinity-domain identity.
// Authorize is called before I/O and before returning successful results.
// HoldInstances proves availability and holds source snapshots through
// publication. The caller owns their availability for the entire published
// lifetime and must submit a replacement when references are withdrawn.
type ClientConfig struct {
	HTTPClient                                            *http.Client
	RepositoryEndpoint, ImagingEndpoint, ImagingSourceUID string
	AllowHTTP                                             bool
	Limits                                                Limits
	Mapping                                               func(context.Context, PatientID) (PatientID, error)
	Authorize                                             func(context.Context, Operation, []InstanceRef) error
	HoldInstances                                         func(context.Context, []InstanceRef) (release func(), err error)
}

type Client struct {
	http                           *http.Client
	repository, imaging, sourceUID string
	limits                         Limits
	mapping                        func(context.Context, PatientID) (PatientID, error)
	authorize                      func(context.Context, Operation, []InstanceRef) error
	hold                           func(context.Context, []InstanceRef) (func(), error)
}

func NewClient(config ClientConfig) (*Client, error) {
	limits, err := normalizeLimits(config.Limits)
	if err != nil {
		return nil, err
	}
	if config.HTTPClient == nil || config.Authorize == nil || !core.IsValidUID(config.ImagingSourceUID) {
		return nil, ErrMapping
	}
	for _, endpoint := range []string{config.RepositoryEndpoint, config.ImagingEndpoint} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
			(u.Scheme != "https" && !(config.AllowHTTP && u.Scheme == "http")) {
			return nil, ErrMapping
		}
	}
	httpClient := *config.HTTPClient
	if httpClient.Timeout <= 0 {
		httpClient.Timeout = 30 * time.Second
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		http: &httpClient, repository: config.RepositoryEndpoint, imaging: config.ImagingEndpoint,
		sourceUID: config.ImagingSourceUID, limits: limits, mapping: config.Mapping,
		authorize: config.Authorize, hold: config.HoldInstances,
	}, nil
}

type RegistryError struct {
	Code, Severity, Location, Context string
}

type PublicationResult struct {
	Status, EntryUUID, SubmissionEntryUUID string
	Errors                                 []RegistryError
}

func (r PublicationResult) Successful() bool { return r.Status == statusSuccess }

// Publish sends one stable KOS and its metadata using SOAP 1.2 MTOM/XOP.
// A registry rejection is a result with Successful()==false. HTTP, envelope,
// correlation, resource and authorization failures return an error. A failure
// after request transmission can have an indeterminate remote publication
// outcome; callers must reconcile before retrying the same unique identifier.
func (c *Client) Publish(ctx context.Context, manifest *Manifest, metadata PublicationMetadata) (PublicationResult, error) {
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	if err := metadata.validate(manifest); err != nil {
		return PublicationResult{}, err
	}
	if manifest.sourceUID != c.sourceUID || c.mapping == nil {
		return PublicationResult{}, ErrMapping
	}
	mapped, err := c.mapping(ctx, manifest.patient)
	if err != nil {
		return PublicationResult{}, err
	}
	if !mapped.valid() || mapped != metadata.Patient {
		return PublicationResult{}, ErrMapping
	}
	if err := c.authorize(ctx, Publish, manifest.References()); err != nil {
		return PublicationResult{}, fmt.Errorf("%w: %w", ErrAuthorization, err)
	}
	if c.hold == nil {
		return PublicationResult{}, ErrAvailability
	}
	release, err := c.hold(ctx, manifest.References())
	if release != nil {
		defer release()
	}
	if err != nil {
		return PublicationResult{}, err
	}
	if release == nil {
		return PublicationResult{}, ErrAvailability
	}
	entryID, err := newMessageID()
	if err != nil {
		return PublicationResult{}, err
	}
	setID, err := newMessageID()
	if err != nil {
		return PublicationResult{}, err
	}
	msg, err := newMessageID()
	if err != nil {
		return PublicationResult{}, err
	}
	body := publicationBody(manifest, metadata, entryID, setID)
	root, attachments, err := c.exchange(ctx, c.repository, actionPublish, actionPublishResponse, msg, body,
		[]attachment{{id: "manifest@xdsi", mediaType: dicomMediaType, data: manifest.data}}, 1)
	if err != nil {
		return PublicationResult{}, err
	}
	if len(attachments) != 0 || root.Name != (xml.Name{Space: nsRS, Local: "RegistryResponse"}) {
		return PublicationResult{}, ErrProtocol
	}
	status, errors, err := registryResponse(root)
	if err != nil || status == statusPartial {
		return PublicationResult{}, ErrProtocol
	}
	if err := c.authorize(ctx, Publish, manifest.References()); err != nil {
		return PublicationResult{}, fmt.Errorf("%w: %w", ErrAuthorization, err)
	}
	if err := ctx.Err(); err != nil {
		return PublicationResult{}, err
	}
	return PublicationResult{Status: status, EntryUUID: entryID, SubmissionEntryUUID: setID, Errors: errors}, nil
}

func (c *Client) exchange(ctx context.Context, endpoint, action, responseAction, msg string, body *wireElement,
	attachments []attachment, maxParts int) (*wireElement, map[string]attachment, error) {
	wire, contentType, err := encodeMTOM(envelope(action, endpoint, msg, body), action, attachments)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(wire)) > c.limits.TotalBytes {
		return nil, nil, ErrLimit
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(wire))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "multipart/related; type=\"application/xop+xml\"")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("xdsi: peer HTTP status %d", response.StatusCode)
	}
	if response.ContentLength > c.limits.TotalBytes {
		return nil, nil, ErrLimit
	}
	limited := &io.LimitedReader{R: response.Body, N: c.limits.TotalBytes + 1}
	root, parts, err := readMTOM(response.Header.Get("Content-Type"), limited, c.limits, maxParts)
	if err != nil {
		return nil, nil, err
	}
	if _, err = io.Copy(io.Discard, limited); err != nil {
		return nil, nil, err
	}
	if limited.N == 0 {
		return nil, nil, ErrLimit
	}
	payload, err := responseBody(root, responseAction, msg)
	return payload, parts, err
}

func registryResponse(node *wireElement) (string, []RegistryError, error) {
	if node.Name != (xml.Name{Space: nsRS, Local: "RegistryResponse"}) {
		return "", nil, ErrProtocol
	}
	status := node.attr("status")
	if status != statusSuccess && status != statusPartial && status != statusFailure {
		return "", nil, ErrProtocol
	}
	var errors []RegistryError
	lists := node.children(nsRS, "RegistryErrorList")
	if len(lists) > 1 {
		return "", nil, ErrProtocol
	}
	if len(lists) == 1 {
		for _, child := range lists[0].Children {
			if child.Name != (xml.Name{Space: nsRS, Local: "RegistryError"}) {
				return "", nil, ErrProtocol
			}
			code, severity := child.attr("errorCode"), child.attr("severity")
			if code == "" || (severity != "urn:oasis:names:tc:ebxml-regrep:ErrorSeverityType:Error" &&
				severity != "urn:oasis:names:tc:ebxml-regrep:ErrorSeverityType:Warning") {
				return "", nil, ErrProtocol
			}
			if status == statusSuccess && strings.HasSuffix(severity, ":Error") {
				return "", nil, ErrProtocol
			}
			errors = append(errors, RegistryError{Code: code, Severity: severity, Location: child.attr("location"), Context: child.attr("codeContext")})
		}
	}
	if status != statusSuccess && len(errors) == 0 {
		return "", nil, ErrProtocol
	}
	if status != statusSuccess && !hasRegistryError(errors) {
		return "", nil, ErrProtocol
	}
	return status, errors, nil
}

type InstanceResult struct {
	Reference         InstanceRef
	Data              []byte
	SHA256            [32]byte
	TransferSyntaxUID string
	Errors            []RegistryError
	Err               error
}

type RetrievalResult struct {
	Status    string
	Instances []InstanceResult
	Errors    []RegistryError
}

// Retrieve requests every whole image in a validated manifest. Results retain
// manifest order. An unsuccessful instance has no Data. Part 10 metadata,
// local patient/domain, study, series, SOP class/instance, and advertised
// transfer syntax are verified before a successful instance is exposed.
func (c *Client) Retrieve(ctx context.Context, manifest *Manifest, syntaxes []string) (RetrievalResult, error) {
	if err := ctx.Err(); err != nil {
		return RetrievalResult{}, err
	}
	if manifest == nil || manifest.sourceUID != c.sourceUID {
		return RetrievalResult{}, ErrMapping
	}
	if len(syntaxes) == 0 || len(syntaxes) > 32 {
		return RetrievalResult{}, ErrManifest
	}
	for i, syntax := range syntaxes {
		if !core.IsValidUID(syntax) || slices.Contains(syntaxes[:i], syntax) {
			return RetrievalResult{}, ErrManifest
		}
	}
	if err := c.authorize(ctx, Retrieve, manifest.References()); err != nil {
		return RetrievalResult{}, fmt.Errorf("%w: %w", ErrAuthorization, err)
	}
	body := retrieveBody(manifest, syntaxes)
	msg, err := newMessageID()
	if err != nil {
		return RetrievalResult{}, err
	}
	root, parts, err := c.exchange(ctx, c.imaging, actionRetrieve, actionRetrieveResponse, msg, body, nil, len(manifest.refs)+1)
	if err != nil {
		return RetrievalResult{}, err
	}
	if root.Name != (xml.Name{Space: nsXDS, Local: "RetrieveDocumentSetResponse"}) {
		return RetrievalResult{}, ErrProtocol
	}
	registry, err := root.one(nsRS, "RegistryResponse")
	if err != nil {
		return RetrievalResult{}, err
	}
	status, registryErrors, err := registryResponse(registry)
	if err != nil {
		return RetrievalResult{}, err
	}
	result := RetrievalResult{Status: status, Errors: registryErrors, Instances: make([]InstanceResult, len(manifest.refs))}
	for _, child := range root.Children {
		if child.Name != (xml.Name{Space: nsRS, Local: "RegistryResponse"}) && child.Name != (xml.Name{Space: nsXDS, Local: "DocumentResponse"}) {
			return RetrievalResult{}, ErrProtocol
		}
	}
	index := make(map[string]int, len(manifest.refs))
	for i, ref := range manifest.refs {
		index[ref.SOPInstanceUID] = i
		result.Instances[i] = InstanceResult{Reference: ref}
	}
	for _, entry := range registryErrors {
		i, found := index[entry.Location]
		if entry.Location == "" && status == statusFailure {
			continue
		}
		if !found {
			return RetrievalResult{}, ErrProtocol
		}
		result.Instances[i].Errors = append(result.Instances[i].Errors, entry)
	}
	seen := make(map[string]bool, len(index))
	for _, node := range root.children(nsXDS, "DocumentResponse") {
		id, err := textChild(node, nsXDS, "DocumentUniqueId")
		if err != nil {
			return RetrievalResult{}, err
		}
		i, found := index[id]
		if !found || seen[id] {
			return RetrievalResult{}, ErrProtocol
		}
		seen[id] = true
		repository, err := textChild(node, nsXDS, "RepositoryUniqueId")
		if err != nil || repository != c.sourceUID || len(node.children(nsXDS, "HomeCommunityId")) != 0 {
			return RetrievalResult{}, ErrProtocol
		}
		media, err := textChild(node, nsXDS, "mimeType")
		if err != nil || media != dicomMediaType {
			return RetrievalResult{}, ErrProtocol
		}
		document, err := node.one(nsXDS, "Document")
		if err != nil {
			return RetrievalResult{}, err
		}
		include, err := document.one(nsXOP, "Include")
		if err != nil || len(document.Children) != 1 || strings.TrimSpace(document.Text) != "" || len(include.Children) != 0 || strings.TrimSpace(include.Text) != "" {
			return RetrievalResult{}, ErrProtocol
		}
		href := include.attr("href")
		if !strings.HasPrefix(href, "cid:") {
			return RetrievalResult{}, ErrProtocol
		}
		part, found := parts[strings.TrimPrefix(href, "cid:")]
		if !found || part.mediaType != dicomMediaType {
			return RetrievalResult{}, ErrProtocol
		}
		delete(parts, part.id)
		out := &result.Instances[i]
		if hasRegistryError(out.Errors) {
			return RetrievalResult{}, ErrProtocol
		}
		syntax, err := validateInstance(ctx, part.data, out.Reference, manifest.patient, syntaxes, c.limits)
		out.Err = err
		if err == nil {
			out.Data = part.data
			out.SHA256 = sha256.Sum256(part.data)
			out.TransferSyntaxUID = syntax
		}
	}
	if len(parts) != 0 {
		return RetrievalResult{}, ErrProtocol
	}
	received := len(seen)
	if status == statusSuccess && received != len(index) ||
		status == statusPartial && (received == 0 || received == len(index)) ||
		status == statusFailure && received != 0 {
		return RetrievalResult{}, ErrProtocol
	}
	for i := range result.Instances {
		out := &result.Instances[i]
		if !seen[out.Reference.SOPInstanceUID] {
			if !hasRegistryError(out.Errors) && status != statusFailure {
				return RetrievalResult{}, ErrProtocol
			}
			out.Err = ErrAvailability
		}
	}
	if err := c.authorize(ctx, Retrieve, manifest.References()); err != nil {
		return RetrievalResult{}, fmt.Errorf("%w: %w", ErrAuthorization, err)
	}
	if err := ctx.Err(); err != nil {
		return RetrievalResult{}, err
	}
	return result, nil
}

func textChild(node *wireElement, ns, local string) (string, error) {
	child, err := node.one(ns, local)
	if err != nil || len(child.Children) != 0 {
		return "", ErrProtocol
	}
	return strings.TrimSpace(child.Text), nil
}

func hasRegistryError(errors []RegistryError) bool {
	for _, err := range errors {
		if strings.HasSuffix(err.Severity, ":Error") {
			return true
		}
	}
	return false
}

func retrieveBody(manifest *Manifest, syntaxes []string) *wireElement {
	body := element(nsRAD, "RetrieveImagingDocumentSetRequest", "")
	study := element(nsRAD, "StudyRequest", "", "studyInstanceUID", manifest.studyUID)
	series := make(map[string]*wireElement)
	for _, ref := range manifest.refs {
		group := series[ref.SeriesUID]
		if group == nil {
			group = element(nsRAD, "SeriesRequest", "", "seriesInstanceUID", ref.SeriesUID)
			series[ref.SeriesUID] = group
			study.Children = append(study.Children, group)
		}
		request := element(nsXDS, "DocumentRequest", "")
		request.Children = []*wireElement{element(nsXDS, "RepositoryUniqueId", manifest.sourceUID), element(nsXDS, "DocumentUniqueId", ref.SOPInstanceUID)}
		group.Children = append(group.Children, request)
	}
	list := element(nsRAD, "TransferSyntaxUIDList", "")
	for _, syntax := range syntaxes {
		list.Children = append(list.Children, element(nsRAD, "TransferSyntaxUID", syntax))
	}
	body.Children = []*wireElement{study, list}
	return body
}

func validateInstance(ctx context.Context, data []byte, ref InstanceRef, patient PatientID, syntaxes []string, limits Limits) (string, error) {
	file, err := readPart10(ctx, data, limits)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrIdentity
	}
	defer file.Close()
	meta := file.Metadata()
	gotPatient, ok := patientIDFrom(file.Dataset)
	if !ok || gotPatient != patient || !singleString(file.Dataset, tags.StudyInstanceUID, ref.StudyUID) ||
		!singleString(file.Dataset, tags.SeriesInstanceUID, ref.SeriesUID) ||
		!singleString(file.Dataset, tags.SOPClassUID, ref.SOPClassUID) ||
		!singleString(file.Dataset, tags.SOPInstanceUID, ref.SOPInstanceUID) ||
		!singleString(file.Meta, tags.MediaStorageSOPClassUID, ref.SOPClassUID) ||
		!singleString(file.Meta, tags.MediaStorageSOPInstanceUID, ref.SOPInstanceUID) ||
		!slices.Contains(syntaxes, meta.TransferSyntaxUID) {
		return "", ErrIdentity
	}
	return meta.TransferSyntaxUID, nil
}
