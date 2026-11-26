package dicomweb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

const (
	maxStorageCommitmentReferences = 16_384
	maxStorageCommitmentBody       = 8 << 20
	storageCommitmentWait          = 2 * time.Minute
	storageCommitmentPollInterval  = time.Second

	commitmentMediaType             = string(MetadataMediaTypeDICOMJSON)
	commitmentReferencedSOPSequence = "00081199"
	commitmentFailedSOPSequence     = "00081198"
	commitmentSOPClassUID           = "00081150"
	commitmentSOPInstanceUID        = "00081155"
	commitmentFailureReason         = "00081197"
	commitmentTransactionUID        = "00081195"
)

// StorageCommitmentReference identifies an instance actually transmitted to
// the destination receiving the commitment request.
type StorageCommitmentReference struct {
	SOPClassUID    string
	SOPInstanceUID string
}

// StorageCommitmentFailure is a refusal reported for one requested instance.
type StorageCommitmentFailure struct {
	StorageCommitmentReference
	FailureReason uint16
}

// StorageCommitmentOptions configures one request and its bounded result wait.
type StorageCommitmentOptions struct {
	// TransactionUID is generated when empty. Specify it to correlate diagnostics
	// with a caller-owned transaction identifier.
	TransactionUID string
	// Timeout bounds the entire operation. Zero uses two minutes.
	Timeout time.Duration
	// PollInterval is the minimum delay after HTTP 202. Zero uses one second.
	// A longer server Retry-After is respected within the overall timeout.
	PollInterval time.Duration
	// BeforeRequest, when provided, revalidates caller authorization before
	// the initial request and each poll.
	BeforeRequest func(context.Context) error
}

// StorageCommitmentResult contains only fully validated final references.
// Pending records an accepted request whose complete result is unavailable.
type StorageCommitmentResult struct {
	TransactionUID string
	Pending        bool
	// RetryAfter is the peer's requested delay when Pending is true.
	RetryAfter time.Duration
	Committed  []StorageCommitmentReference
	Failed     []StorageCommitmentFailure
}

// CommitStorage performs the PS3.18 Commit transaction and, after HTTP 202,
// polls Check Commit Result on the same transaction resource. This profile
// sends a flat Referenced SOP Sequence and requests DICOM JSON.
func (c Client) CommitStorage(ctx context.Context, references []StorageCommitmentReference, opts StorageCommitmentOptions) (StorageCommitmentResult, error) {
	requested, err := validateStorageCommitmentReferences(references)
	if err != nil {
		return StorageCommitmentResult{}, err
	}
	if opts.Timeout < 0 || opts.PollInterval < 0 {
		return StorageCommitmentResult{}, fmt.Errorf("dicomweb: negative storage commitment timeout or poll interval")
	}
	uid := opts.TransactionUID
	if uid == "" {
		uid, err = core.NewUID()
		if err != nil {
			return StorageCommitmentResult{}, err
		}
	}
	u, err := c.storageCommitmentURL(uid)
	if err != nil {
		return StorageCommitmentResult{}, err
	}
	body, err := encodeStorageCommitmentRequest(references)
	if err != nil {
		return StorageCommitmentResult{}, err
	}

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = storageCommitmentWait
	}
	interval := opts.PollInterval
	if interval == 0 {
		interval = storageCommitmentPollInterval
	}
	ctx, cancel := context.WithTimeout(contextOrBackground(ctx), timeout)
	defer cancel()
	if opts.BeforeRequest != nil {
		if err := opts.BeforeRequest(ctx); err != nil {
			return StorageCommitmentResult{TransactionUID: uid}, err
		}
	}
	result, err := c.storageCommitmentRequest(ctx, u, uid, requested, body)
	for err == nil && result.Pending {
		if err := sleepContext(ctx, max(interval, result.RetryAfter)); err != nil {
			return result, requestError(u.String(), err)
		}
		if opts.BeforeRequest != nil {
			if err := opts.BeforeRequest(ctx); err != nil {
				return result, err
			}
		}
		next, checkErr := c.storageCommitmentRequest(ctx, u, uid, requested, nil)
		if checkErr != nil {
			return result, checkErr
		}
		result = next
	}
	return result, err
}

// CheckStorageCommitment checks a previously requested transaction once. Give
// it the same transaction UID and exact references as the original Commit.
func (c Client) CheckStorageCommitment(ctx context.Context, uid string, references []StorageCommitmentReference) (StorageCommitmentResult, error) {
	requested, err := validateStorageCommitmentReferences(references)
	if err != nil {
		return StorageCommitmentResult{}, err
	}
	u, err := c.storageCommitmentURL(uid)
	if err != nil {
		return StorageCommitmentResult{}, err
	}
	return c.storageCommitmentRequest(ctx, u, uid, requested, nil)
}

func (c Client) storageCommitmentURL(uid string) (*url.URL, error) {
	if !core.IsValidUID(uid) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("invalid storage commitment transaction UID")}
	}
	return c.Endpoint.serviceURL("", []string{"commitment-requests", uid}, nil)
}

func validateStorageCommitmentReferences(references []StorageCommitmentReference) (map[string]string, error) {
	if len(references) == 0 || len(references) > maxStorageCommitmentReferences {
		return nil, fmt.Errorf("dicomweb: storage commitment requires between 1 and %d references", maxStorageCommitmentReferences)
	}
	requested := make(map[string]string, len(references))
	for _, reference := range references {
		if !core.IsValidUID(reference.SOPInstanceUID) || !core.IsValidUID(reference.SOPClassUID) {
			return nil, fmt.Errorf("dicomweb: invalid SOP reference in storage commitment request")
		}
		if _, exists := requested[reference.SOPInstanceUID]; exists {
			return nil, fmt.Errorf("dicomweb: duplicate SOP instance in storage commitment request")
		}
		requested[reference.SOPInstanceUID] = reference.SOPClassUID
	}
	return requested, nil
}

func encodeStorageCommitmentRequest(references []StorageCommitmentReference) ([]byte, error) {
	items := make([]any, 0, len(references))
	for _, ref := range references {
		items = append(items, Dataset{
			commitmentSOPClassUID:    {VR: "UI", Value: []any{ref.SOPClassUID}},
			commitmentSOPInstanceUID: {VR: "UI", Value: []any{ref.SOPInstanceUID}},
		})
	}
	return json.Marshal(Dataset{commitmentReferencedSOPSequence: {VR: "SQ", Value: items}})
}

func (c Client) storageCommitmentRequest(ctx context.Context, u *url.URL, uid string, requested map[string]string, body []byte) (StorageCommitmentResult, error) {
	result := StorageCommitmentResult{TransactionUID: uid}
	// The request URI itself correlates the result. Never follow a redirect to
	// another resource, even on the same authenticated server.
	httpClient := c.Options.HTTPClient
	if httpClient == nil {
		httpClient = sourceport.HTTPClient()
	}
	clientCopy := *httpClient
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	c.Options.HTTPClient = &clientCopy
	if c.Options.MaxBodyBytes <= 0 || c.Options.MaxBodyBytes > maxStorageCommitmentBody {
		c.Options.MaxBodyBytes = maxStorageCommitmentBody
	}

	requestOpts := requestOptions{sameOriginRedirect: true}
	var response *http.Response
	var cancel context.CancelFunc
	var err error
	if body == nil {
		response, cancel, err = c.doHTTPAttempt(ctx, http.MethodGet, u, nil, "", commitmentMediaType, requestOpts)
	} else {
		response, cancel, err = c.doHTTPAttempt(ctx, http.MethodPost, u, bytes.NewReader(body), commitmentMediaType, commitmentMediaType, requestOpts)
	}
	if err != nil {
		return result, err
	}
	resp, err := c.bufferResponse(u, response, cancel)
	if err != nil {
		return result, err
	}
	if err := validateCommitmentLocation(u, resp.Header.Values("Location")); err != nil {
		return result, newDICOMwebError(ErrorKindInvalidEndpoint, resp.URL, resp.StatusCode, err)
	}

	switch resp.StatusCode {
	case http.StatusAccepted:
		if len(resp.Body) != 0 {
			return result, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, fmt.Errorf("storage commitment HTTP 202 contains a payload"))
		}
		result.Pending = true
		result.RetryAfter, _ = retryAfter(resp.Header, time.Now())
	case http.StatusOK:
		contentType, err := responseContentType(resp.Header.Values("Content-Type"))
		if err != nil {
			return result, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
		}
		mediaType, _, err := parseResponseMediaType(contentType)
		if err != nil || mediaType != commitmentMediaType {
			return result, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, fmt.Errorf("expected a DICOM JSON storage commitment result"))
		}
		result.Committed, result.Failed, err = parseStorageCommitmentResult(resp.Body, uid, requested)
		if err != nil {
			return result, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
		}
	default:
		return result, statusError(resp)
	}
	if err := contextOrBackground(ctx).Err(); err != nil {
		return StorageCommitmentResult{TransactionUID: uid, Pending: result.Pending}, err
	}
	return result, nil
}

func validateCommitmentLocation(u *url.URL, locations []string) error {
	if len(locations) == 0 {
		// PS3.18 identifies the poll by the original path's transaction UID.
		return nil
	}
	if len(locations) != 1 || strings.TrimSpace(locations[0]) == "" {
		return fmt.Errorf("invalid storage commitment Location")
	}
	reference, err := url.Parse(locations[0])
	if err != nil {
		return fmt.Errorf("invalid storage commitment Location")
	}
	location := u.ResolveReference(reference)
	if !sameOrigin(u, location) || location.Opaque != "" || location.EscapedPath() != u.EscapedPath() ||
		location.RawQuery != "" || location.ForceQuery || location.Fragment != "" || location.RawFragment != "" {
		return fmt.Errorf("storage commitment Location must identify the original transaction resource")
	}
	return nil
}

func parseStorageCommitmentResult(body []byte, uid string, requested map[string]string) ([]StorageCommitmentReference, []StorageCommitmentFailure, error) {
	var dataset Dataset
	switch trimmed := bytes.TrimSpace(body); {
	case len(trimmed) == 0:
		return nil, nil, fmt.Errorf("storage commitment response is empty")
	case trimmed[0] == '[':
		datasets, err := datasetsFromDICOMJSON(trimmed)
		if err != nil || len(datasets) != 1 {
			return nil, nil, fmt.Errorf("storage commitment response requires one DICOM dataset")
		}
		dataset = datasets[0]
	default:
		if err := decodeJSON(trimmed, &dataset); err != nil {
			return nil, nil, fmt.Errorf("storage commitment response requires one DICOM dataset")
		}
	}
	if _, present := dataset[commitmentTransactionUID]; present && commitmentUIDValue(dataset[commitmentTransactionUID]) != uid {
		return nil, nil, fmt.Errorf("storage commitment response has an unexpected transaction UID")
	}
	for _, tag := range []string{"00081110", "0008119B"} {
		if _, present := dataset[tag]; present {
			return nil, nil, fmt.Errorf("storage commitment response uses an unrequested grouped study layout")
		}
	}
	seen := make(map[string]bool, len(requested))
	var committed []StorageCommitmentReference
	var failed []StorageCommitmentFailure
	for _, tag := range []string{commitmentReferencedSOPSequence, commitmentFailedSOPSequence} {
		sequence, present := dataset[tag]
		if !present {
			continue
		}
		if sequence.VR != "SQ" || sequence.InlineBinary != "" || sequence.BulkDataURI != "" || len(sequence.Value) == 0 ||
			len(sequence.Value) > len(requested)-len(seen) {
			return nil, nil, fmt.Errorf("invalid storage commitment %s sequence", tag)
		}
		for _, item := range sequence.Value {
			result, err := sequenceItemDataset(item)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid storage commitment reference item")
			}
			ref := StorageCommitmentReference{
				SOPClassUID:    commitmentUIDValue(result[commitmentSOPClassUID]),
				SOPInstanceUID: commitmentUIDValue(result[commitmentSOPInstanceUID]),
			}
			if ref.SOPInstanceUID == "" || ref.SOPClassUID == "" || requested[ref.SOPInstanceUID] != ref.SOPClassUID || seen[ref.SOPInstanceUID] {
				return nil, nil, fmt.Errorf("storage commitment response has an unexpected or repeated SOP reference")
			}
			seen[ref.SOPInstanceUID] = true
			if tag == commitmentReferencedSOPSequence {
				if _, hasFailureReason := result[commitmentFailureReason]; hasFailureReason {
					return nil, nil, fmt.Errorf("committed SOP reference has a failure reason")
				}
				committed = append(committed, ref)
				continue
			}
			reason, err := commitmentFailureReasonValue(result[commitmentFailureReason])
			if err != nil {
				return nil, nil, err
			}
			failed = append(failed, StorageCommitmentFailure{StorageCommitmentReference: ref, FailureReason: reason})
		}
	}
	if len(seen) != len(requested) {
		return nil, nil, fmt.Errorf("storage commitment response omitted a requested SOP reference")
	}
	return committed, failed, nil
}

func commitmentUIDValue(element Element) string {
	if element.VR != "UI" || element.InlineBinary != "" || element.BulkDataURI != "" || len(element.Value) != 1 {
		return ""
	}
	value, ok := element.Value[0].(string)
	if !ok || !core.IsValidUID(value) {
		return ""
	}
	return value
}

func commitmentFailureReasonValue(element Element) (uint16, error) {
	if element.VR == "US" && element.InlineBinary == "" && element.BulkDataURI == "" && len(element.Value) == 1 {
		if value, ok := element.Value[0].(json.Number); ok {
			reason, err := strconv.ParseUint(string(value), 10, 16)
			if err == nil && reason != 0 {
				return uint16(reason), nil
			}
		}
	}
	return 0, fmt.Errorf("failed storage commitment reference has no valid failure reason")
}
