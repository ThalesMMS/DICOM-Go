package dicomtest

import (
	"encoding/binary"
	"strings"
)

// AssociationRQVector is an A-ASSOCIATE-RQ PDU (header included) and whether
// its encoding is malformed, so that parsing must fail.
type AssociationRQVector struct {
	Name      string
	PDU       []byte
	Malformed bool
}

// PS3.8 item and sub-item types used by the vectors. They are spelled out so
// this package does not import net/ul, whose tests use it.
const (
	itemApplicationContext   = 0x10
	itemPresentationContext  = 0x20
	itemAbstractSyntax       = 0x30
	itemTransferSyntax       = 0x40
	itemUserInformation      = 0x50
	subItemMaxLength         = 0x51
	subItemImplementationUID = 0x52
	subItemExtendedNeg       = 0x56
	subItemUserIdentity      = 0x58

	verificationUID     = "1.2.840.10008.1.1"
	implicitLittleUID   = "1.2.840.10008.1.2"
	applicationContext  = "1.2.840.10008.3.1.1.1"
	vectorImplementUID  = "1.2.826.0.1.3680043.10.543.994"
	vectorCalledAETitle = "DCMNETTESTCALLED"
	vectorCallerAETitle = "DCMNETTESTCALLER"
)

// AssociationRQVectors reproduces, from the PS3.8 PDU layout, the malformed
// A-ASSOCIATE-RQs that DCMTK 3.7.0 checks in dcmnet/tests/tparseassoc.cc and
// tassocleak.cc (commit 8d68392d, OFFIS BSD-3-Clause). Only the scenarios
// come from those tests; the bytes are generated here. The last vector is
// well formed: it must parse whole, although its username identity carries a
// secondary field that PS3.7 Table D.3-14 reserves for type 2.
func AssociationRQVectors() []AssociationRQVector {
	var vectors []AssociationRQVector

	// tparseassoc extNeg_truncated: ten minimal SOP Class Extended
	// Negotiation sub-items (UID length 0), then one a byte short.
	{
		var info []byte
		for i := 0; i < 10; i++ {
			info = append(info, vectorItem(subItemExtendedNeg, []byte{0, 0})...)
		}
		info = append(info, subItemExtendedNeg, 0, 0, 2, 0)
		vectors = append(vectors, AssociationRQVector{"extended negotiation truncated", associateRQ(vectorItem(itemUserInformation, info)), true})
	}
	// tparseassoc extNeg_malformed_itemLength: item length 1, below the
	// two-byte SOP Class UID length field.
	vectors = append(vectors, AssociationRQVector{"extended negotiation item length 1",
		associateRQ(vectorItem(itemUserInformation, []byte{subItemExtendedNeg, 0, 0, 1, 0, 0})), true})
	// tparseassoc duplicate_userIdentity: two well-formed User Identity
	// sub-items, which PS3.7 D.3.3.7 allows once.
	vectors = append(vectors, AssociationRQVector{"duplicate user identity", associateRQ(
		vectorItem(itemApplicationContext, []byte(applicationContext)),
		verificationPresentationContext(1, implicitLittleUID),
		vectorItem(itemUserInformation, vectorConcat(maxLength(16384), implementationUID(),
			userIdentity(1, nil, nil), userIdentity(1, nil, nil))),
	), true})
	// tparseassoc presCtx_malformed_transferSyntax: five valid Transfer
	// Syntax sub-items, then one whose length runs past the context.
	{
		context := verificationContextValue(1, implicitLittleUID, implicitLittleUID, implicitLittleUID, implicitLittleUID, implicitLittleUID)
		context = append(context, itemTransferSyntax, 0, 0, byte(len(implicitLittleUID)))
		vectors = append(vectors, AssociationRQVector{"transfer syntax beyond its context", associateRQ(vectorItem(itemPresentationContext, context)), true})
	}
	// tassocleak zeroTransferSyntax: an otherwise well-formed request whose
	// only presentation context has no Transfer Syntax sub-item.
	vectors = append(vectors, AssociationRQVector{"presentation context without transfer syntax", associateRQ(
		vectorItem(itemApplicationContext, []byte(applicationContext)),
		verificationPresentationContext(1),
		vectorItem(itemUserInformation, vectorConcat(maxLength(16384), implementationUID())),
	), true})
	// tparseassoc parseUserIdentity_secondaryField_bytesRead: a username
	// identity (type 1) with a non-empty secondary field is consumed whole,
	// so the Maximum Length sub-item after it still parses (32768).
	vectors = append(vectors, AssociationRQVector{"user identity secondary field", associateRQ(
		vectorItem(itemApplicationContext, []byte(applicationContext)),
		verificationPresentationContext(1, implicitLittleUID),
		vectorItem(itemUserInformation, vectorConcat(implementationUID(),
			userIdentity(1, []byte("ABC"), []byte("abcde")), maxLength(32768))),
	), false})
	return vectors
}

// AssociationRQVectorSeeds returns the vectors' PDUs as fuzz seeds.
func AssociationRQVectorSeeds() [][]byte {
	vectors := AssociationRQVectors()
	seeds := make([][]byte, len(vectors))
	for i, vector := range vectors {
		seeds[i] = vector.PDU
	}
	return seeds
}

// associateRQ builds an A-ASSOCIATE-RQ with the fixed 68-byte header and the
// given variable items.
func associateRQ(items ...[]byte) []byte {
	body := []byte{0, 1, 0, 0}
	body = append(body, padTitle(vectorCalledAETitle)...)
	body = append(body, padTitle(vectorCallerAETitle)...)
	body = append(body, make([]byte, 32)...)
	for _, item := range items {
		body = append(body, item...)
	}
	pdu := []byte{0x01, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(pdu[2:], uint32(len(body)))
	return append(pdu, body...)
}

func padTitle(title string) []byte {
	return []byte((title + strings.Repeat(" ", 16))[:16])
}

func vectorItem(itemType byte, value []byte) []byte {
	out := []byte{itemType, 0, 0, 0}
	binary.BigEndian.PutUint16(out[2:], uint16(len(value)))
	return append(out, value...)
}

func vectorConcat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func verificationContextValue(id byte, transferSyntaxes ...string) []byte {
	value := []byte{id, 0, 0, 0}
	value = append(value, vectorItem(itemAbstractSyntax, []byte(verificationUID))...)
	for _, ts := range transferSyntaxes {
		value = append(value, vectorItem(itemTransferSyntax, []byte(ts))...)
	}
	return value
}

func verificationPresentationContext(id byte, transferSyntaxes ...string) []byte {
	return vectorItem(itemPresentationContext, verificationContextValue(id, transferSyntaxes...))
}

func maxLength(length uint32) []byte {
	return vectorItem(subItemMaxLength, binary.BigEndian.AppendUint32(nil, length))
}

func implementationUID() []byte {
	return vectorItem(subItemImplementationUID, []byte(vectorImplementUID))
}

func userIdentity(identityType byte, primary, secondary []byte) []byte {
	value := []byte{identityType, 0}
	value = binary.BigEndian.AppendUint16(value, uint16(len(primary)))
	value = append(value, primary...)
	value = binary.BigEndian.AppendUint16(value, uint16(len(secondary)))
	value = append(value, secondary...)
	return vectorItem(subItemUserIdentity, value)
}
