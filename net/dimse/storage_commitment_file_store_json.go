package dimse

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

type commitmentFileRecord struct {
	SchemaVersion int                          `json:"schema_version"`
	Transaction   StorageCommitmentTransaction `json:"transaction"`
}

func commitmentFileValidUID(uid string) bool {
	return len(uid) <= 64 && validStoreUID(uid)
}

func commitmentFileValidTime(t time.Time) bool {
	return t.IsZero() || t.Year() >= 1 && t.Year() <= 9999
}

func commitmentFileValidState(state StorageCommitmentState) bool {
	switch state {
	case StorageCommitmentStateRequestPending, StorageCommitmentStateAcceptancePrepared, StorageCommitmentStateAccepted,
		StorageCommitmentStateProcessing, StorageCommitmentStateRequestRejected, StorageCommitmentStateResultReady,
		StorageCommitmentStateDelivering, StorageCommitmentStateDeliveryFailed, StorageCommitmentStateDeliveryExhausted,
		StorageCommitmentStateDelivered, StorageCommitmentStateResultReceived:
		return true
	default:
		return false
	}
}

func commitmentFileValidFailure(f StorageCommitmentFailureClass) bool {
	switch f {
	case StorageCommitmentFailureNone, StorageCommitmentFailureProcessing, StorageCommitmentFailureCallbackUnknown,
		StorageCommitmentFailureCallbackOffline, StorageCommitmentFailureTLS, StorageCommitmentFailureTimeout,
		StorageCommitmentFailureCanceled, StorageCommitmentFailureAssociationRejected, StorageCommitmentFailureDIMSEStatus,
		StorageCommitmentFailureProtocol:
		return true
	default:
		return false
	}
}

// Admission precedes clone/JSON allocations and digest traversal of authored
// records. Persistence does not duplicate the workflow's transition machine.
func (s *FileStorageCommitmentStore) validate(t StorageCommitmentTransaction) error {
	if !commitmentFileValidUID(t.TransactionUID) || t.Version == 0 ||
		(t.Direction != StorageCommitmentDirectionRequestor && t.Direction != StorageCommitmentDirectionCommitter) ||
		!commitmentFileValidState(t.State) || !validStorageCommitmentDeliveryMode(t.DeliveryMode) ||
		!commitmentFileValidFailure(t.LastFailure) || t.ProcessingTries < 0 || t.DeliveryAttempts < 0 ||
		ul.ValidateAETitle(t.RequestorAETitle) != nil || ul.ValidateAETitle(t.CommitterAETitle) != nil ||
		t.RequestorAETitle != ul.NormalizeAETitle(t.RequestorAETitle) || t.CommitterAETitle != ul.NormalizeAETitle(t.CommitterAETitle) ||
		t.CreatedAt.IsZero() || t.UpdatedAt.IsZero() {
		return ErrStorageCommitmentInvalidRequest
	}
	for _, timestamp := range []time.Time{t.CreatedAt, t.UpdatedAt, t.CompletedAt, t.NextAttemptAt, t.LeaseUntil} {
		if !commitmentFileValidTime(timestamp) {
			return ErrStorageCommitmentInvalidRequest
		}
	}
	if len(t.LeaseToken) > 32 {
		return ErrStorageCommitmentInvalidRequest
	}
	if t.LeaseToken != "" {
		if len(t.LeaseToken) != 32 || strings.ToLower(t.LeaseToken) != t.LeaseToken {
			return ErrStorageCommitmentInvalidRequest
		}
		if _, err := hex.DecodeString(t.LeaseToken); err != nil {
			return ErrStorageCommitmentInvalidRequest
		}
	}
	if err := s.validateReferences(t.ReferencedSOPs, false); err != nil {
		return err
	}
	if t.RequestDigest != storageCommitmentRequestDigest(t) {
		return ErrStorageCommitmentInvalidRequest
	}
	if t.Result == nil {
		if t.ResultDigest != ([32]byte{}) {
			return ErrStorageCommitmentInvalidResult
		}
		switch t.State {
		case StorageCommitmentStateResultReady, StorageCommitmentStateDelivering, StorageCommitmentStateDeliveryFailed,
			StorageCommitmentStateDeliveryExhausted, StorageCommitmentStateDelivered, StorageCommitmentStateResultReceived:
			return ErrStorageCommitmentInvalidResult
		}
		return nil
	}
	r := *t.Result
	if !commitmentFileValidTime(r.ProducedAt) || r.ProducedAt.IsZero() || r.TransactionUID != t.TransactionUID ||
		len(r.ReferencedSOPs) > s.options.MaxReferences || len(r.FailedSOPs) > s.options.MaxReferences ||
		len(r.ReferencedSOPs)+len(r.FailedSOPs) != len(t.ReferencedSOPs) {
		return ErrStorageCommitmentInvalidResult
	}
	if len(r.ReferencedSOPs) > 0 {
		if err := s.validateReferences(r.ReferencedSOPs, false); err != nil {
			return err
		}
	}
	if len(r.FailedSOPs) > 0 {
		if err := s.validateReferences(r.FailedSOPs, true); err != nil {
			return err
		}
	}
	// Reuse the exact existing partition validator after bounded admission. A
	// ProducedAt is mandatory above, so this call never needs the workflow clock.
	w := StorageCommitmentWorkflow{limits: StorageCommitmentLimits{MaxReferences: s.options.MaxReferences}}
	normalized, err := w.validateResult(t, r)
	if err != nil || t.ResultDigest != storageCommitmentResultDigest(normalized) {
		return ErrStorageCommitmentInvalidResult
	}
	return nil
}

func (s *FileStorageCommitmentStore) validateReferences(refs []StorageCommitmentSOPReference, failed bool) error {
	if len(refs) == 0 || len(refs) > s.options.MaxReferences {
		return ErrStorageCommitmentResourceLimit
	}
	for _, ref := range refs {
		if !commitmentFileValidUID(ref.SOPClassUID) || !commitmentFileValidUID(ref.SOPInstanceUID) {
			return ErrStorageCommitmentInvalidRequest
		}
	}
	normalized, err := normalizeStorageCommitmentReferences(refs, failed, s.options.MaxReferences)
	if err != nil {
		return err
	}
	for i := range refs {
		if refs[i] != normalized[i] {
			return ErrStorageCommitmentInvalidRequest
		}
	}
	return nil
}

func (s *FileStorageCommitmentStore) encode(t StorageCommitmentTransaction) ([]byte, error) {
	if err := s.validate(t); err != nil {
		return nil, err
	}
	expected, err := s.encodedLength(t)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(commitmentFileRecord{SchemaVersion: 1, Transaction: cloneStorageCommitmentTransaction(t)})
	if err != nil {
		return nil, ErrStorageCommitmentInvalidRequest
	}
	if int64(len(data)) != expected {
		return nil, ErrStorageCommitmentInvalidRequest
	}
	return data, nil
}

// encodedLength admits the exact standard JSON encoding before cloning the
// reference slices or constructing the whole-record buffer. validate has
// already bounded every scalar and reference. The small base envelope and one
// reference at a time are the only temporary encodings used during admission.
func (s *FileStorageCommitmentStore) encodedLength(t StorageCommitmentTransaction) (int64, error) {
	base := t
	base.ReferencedSOPs = nil
	if t.Result != nil {
		result := *t.Result
		result.ReferencedSOPs = nil
		result.FailedSOPs = nil
		base.Result = &result
	}
	data, err := json.Marshal(commitmentFileRecord{SchemaVersion: 1, Transaction: base})
	if err != nil {
		return 0, ErrStorageCommitmentInvalidRequest
	}
	n := int64(len(data))
	if n > s.options.MaxRecordBytes {
		return 0, ErrStorageCommitmentResourceLimit
	}
	lists := [][]StorageCommitmentSOPReference{t.ReferencedSOPs}
	if t.Result != nil {
		lists = append(lists, t.Result.ReferencedSOPs, t.Result.FailedSOPs)
	}
	for _, refs := range lists {
		for i, ref := range refs {
			part, err := json.Marshal(ref)
			if err != nil {
				return 0, ErrStorageCommitmentInvalidRequest
			}
			added := int64(len(part) + 1) // comma before every subsequent reference
			if i == 0 {
				added = int64(len(part) - 2) // replace null with [reference]
			}
			if added > s.options.MaxRecordBytes-n {
				return 0, ErrStorageCommitmentResourceLimit
			}
			n += added
		}
	}
	// Empty slices become nil in cloneStorageCommitmentTransaction, so the
	// base's null encodings already preserve that existing wire format.
	return n, nil
}

func (s *FileStorageCommitmentStore) decode(key string, data []byte) (StorageCommitmentTransaction, error) {
	if int64(len(data)) > s.options.MaxRecordBytes || !utf8.Valid(data) || !commitmentFileStrictJSON(data) {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentFileStoreInvalidRecord
	}
	var record commitmentFileRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.SchemaVersion != 1 || s.validate(record.Transaction) != nil ||
		key != commitmentFileKey(record.Transaction.TransactionUID) {
		return StorageCommitmentTransaction{}, ErrStorageCommitmentFileStoreInvalidRecord
	}
	return cloneStorageCommitmentTransaction(record.Transaction), nil
}

// Exact schemas prevent encoding/json's case-insensitive field aliases from
// silently replacing fields. The token pass also refuses duplicate keys,
// unknown objects, trailing documents and excessive structure before decoding.
var commitmentFileJSONFields = map[string]map[string]string{
	"envelope": {"schema_version": "scalar", "transaction": "transaction"},
	"transaction": {
		"TransactionUID": "scalar", "Direction": "scalar", "State": "scalar", "DeliveryMode": "scalar",
		"RequestorAETitle": "scalar", "CommitterAETitle": "scalar", "ReferencedSOPs": "references", "RequestMessageID": "scalar",
		"RequestDigest": "digest", "ResultDigest": "digest", "Result": "result", "Version": "scalar", "CreatedAt": "scalar",
		"UpdatedAt": "scalar", "CompletedAt": "scalar", "ProcessingTries": "scalar", "DeliveryAttempts": "scalar",
		"NextAttemptAt": "scalar", "LeaseToken": "scalar", "LeaseUntil": "scalar", "LastFailure": "scalar", "LastDIMSEStatus": "scalar",
	},
	"result":    {"TransactionUID": "scalar", "ReferencedSOPs": "references", "FailedSOPs": "references", "ProducedAt": "scalar"},
	"reference": {"SOPClassUID": "scalar", "SOPInstanceUID": "scalar", "FailureReason": "scalar"},
}

func commitmentFileStrictJSON(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	tokens := 0
	if !commitmentFileJSONValue(d, "envelope", 0, &tokens) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

func commitmentFileJSONValue(d *json.Decoder, kind string, depth int, tokens *int) bool {
	if depth > 12 || *tokens >= 400000 {
		return false
	}
	*tokens++
	token, err := d.Token()
	if err != nil {
		return false
	}
	delim, structured := token.(json.Delim)
	if !structured {
		return kind == "scalar" && token != nil || token == nil && (kind == "result" || kind == "references")
	}
	if delim == '[' && (kind == "references" || kind == "digest") {
		child := "reference"
		if kind == "digest" {
			child = "scalar"
		}
		count := 0
		for d.More() {
			count++
			if count > 10000 || kind == "digest" && count > 32 || !commitmentFileJSONValue(d, child, depth+1, tokens) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']') && (kind != "digest" || count == 32)
	}
	fields, ok := commitmentFileJSONFields[kind]
	if delim != '{' || !ok {
		return false
	}
	seen := make(map[string]bool, len(fields))
	for d.More() {
		keyToken, err := d.Token()
		key, ok := keyToken.(string)
		child, known := fields[key]
		if err != nil || !ok || !known || seen[key] {
			return false
		}
		seen[key] = true
		if !commitmentFileJSONValue(d, child, depth+1, tokens) {
			return false
		}
	}
	end, err := d.Token()
	return err == nil && end == json.Delim('}') && (kind != "envelope" || seen["schema_version"] && seen["transaction"])
}
