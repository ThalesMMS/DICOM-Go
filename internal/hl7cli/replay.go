package hl7cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/hl7"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/internal/recordstore"
)

const (
	replayReceiptVersion = 1
	maxReplayRecords     = 100_000
)

var errCorruptReceipt = errors.New("hl7rcv: replay receipt is invalid")

// A single publication is both the inbox record and the replayable ACK.
// This path has no non-atomic application handler or second output file.
type replayReceipt struct {
	Version int    `json:"version"`
	ER7     []byte `json:"er7"`
	ACK     []byte `json:"ack"`
	ACKHash string `json:"ackSHA256"`
}

func openReplayStore(ctx context.Context, dir string, maxFrame int, diskQuota int64) (*recordstore.Store, error) {
	if _, err := os.Lstat(dir); err == nil {
		if err := privatefs.Check(dir); err != nil {
			return nil, recordstore.ErrUnavailable
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, recordstore.ErrUnavailable
	}
	// Both fields are base64 byte strings. This bound includes the version,
	// checksum and JSON keys before the total disk quota is applied.
	maxRecord := int64(2*base64.StdEncoding.EncodedLen(maxFrame) + 256)
	store, err := recordstore.Open(ctx, dir, recordstore.Options{
		MaxRecords:     maxReplayRecords,
		MaxTotalBytes:  diskQuota,
		MaxRecordBytes: min(maxRecord, diskQuota),
	})
	if err != nil {
		return nil, err
	}
	if err := checkReplayPrivacy(ctx, store, dir); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func checkReplayPrivacy(ctx context.Context, store *recordstore.Store, dir string) error {
	if err := privatefs.Check(dir); err != nil {
		return recordstore.ErrUnavailable
	}
	keys, err := store.Keys(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := privatefs.Check(filepath.Join(dir, key)); err != nil {
			return recordstore.ErrUnavailable
		}
	}
	return nil
}

func receiveWithReplay(ctx context.Context, store *recordstore.Store, message *hl7.Message, er7 []byte, maxFrame int) (ack []byte, committed bool, err error) {
	key := receiptDigest(er7) + ".json"
	record, err := store.Read(ctx, key)
	if errors.Is(err, recordstore.ErrNotFound) {
		ack, err = hl7.NewACK(message, hl7.AckAccept, hl7.AckOptions{}).MarshalWithLimit(maxFrame)
		if err != nil {
			return nil, false, err
		}
		record, err = json.Marshal(replayReceipt{
			Version: replayReceiptVersion,
			ER7:     er7,
			ACK:     ack,
			ACKHash: receiptDigest(ack),
		})
		if err != nil {
			return nil, false, err
		}
		created, err := store.Create(ctx, key, record)
		if err != nil {
			return nil, false, err
		}
		if created {
			return ack, true, nil
		}
		// Another connection accepted these exact bytes while this one was
		// preparing a receipt. Only the committed ACK can be sent.
		record, err = store.Read(ctx, key)
	}
	if err != nil {
		return nil, false, err
	}
	var receipt replayReceipt
	if err := json.Unmarshal(record, &receipt); err != nil ||
		receipt.Version != replayReceiptVersion ||
		!bytes.Equal(er7, receipt.ER7) ||
		len(receipt.ACK) == 0 || len(receipt.ACK) > maxFrame ||
		receipt.ACKHash != receiptDigest(receipt.ACK) {
		return nil, false, errCorruptReceipt
	}
	return receipt.ACK, false, nil
}

func receiptDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
