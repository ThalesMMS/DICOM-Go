package deid

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/des"
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"io"
	"math/big"
)

// This file is a minimal CMS EnvelopedData (RFC 5652 section 6) writer and
// reader for the Encrypted Content (0400,0520) of PS3.3 C.12.1.1.4.1, written
// from the RFCs. It supports key transport recipients only (RSAES-OAEP,
// RFC 3560 / RFC 8017, and RSAES-PKCS1-v1_5) with AES-CBC content encryption
// (RFC 3565); the reader also accepts Triple-DES CBC (RFC 3370) for content
// produced by older de-identifiers. The reader accepts BER (indefinite
// lengths, constructed strings), as OpenSSL streams it.

var (
	oidCMSData          = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}
	oidCMSEnvelopedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 3}
	oidRSAEncryption    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidRSAESOAEP        = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 7}
	oidMGF1             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 8}
	oidPSpecified       = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 9}
	oidSHA1             = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidSHA256           = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidSHA384           = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidSHA512           = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
	oidAES128CBC        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC        = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
	oidDESEDE3CBC       = asn1.ObjectIdentifier{1, 2, 840, 113549, 3, 7}
)

// maxCMSRecipients bounds the recipients one EnvelopedData may carry, both
// when writing and when reading untrusted input.
const maxCMSRecipients = 64

// maxBERDepth bounds the nesting of the BER structures the reader accepts.
const maxBERDepth = 32

type cmsContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,tag:0"`
}

type cmsEnvelopedData struct {
	Version              int
	OriginatorInfo       asn1.RawValue   `asn1:"optional,tag:0"`
	RecipientInfos       []asn1.RawValue `asn1:"set"`
	EncryptedContentInfo cmsEncryptedContentInfo
	UnprotectedAttrs     asn1.RawValue `asn1:"optional,tag:1"`
}

type cmsEncryptedContentInfo struct {
	ContentType                asn1.ObjectIdentifier
	ContentEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedContent           asn1.RawValue `asn1:"optional,tag:0"`
}

type cmsKeyTransRecipientInfo struct {
	Version                int
	RID                    asn1.RawValue
	KeyEncryptionAlgorithm pkix.AlgorithmIdentifier
	EncryptedKey           []byte
}

type cmsIssuerAndSerialNumber struct {
	Issuer       asn1.RawValue
	SerialNumber *big.Int
}

type rsaesOAEPParams struct {
	HashFunc    pkix.AlgorithmIdentifier `asn1:"optional,explicit,tag:0"`
	MaskGenFunc pkix.AlgorithmIdentifier `asn1:"optional,explicit,tag:1"`
	PSourceFunc pkix.AlgorithmIdentifier `asn1:"optional,explicit,tag:2"`
}

// cmsRecipient is one key transport recipient to encrypt for.
type cmsRecipient struct {
	certificate *x509.Certificate
	publicKey   *rsa.PublicKey
}

// encryptEnvelopedData encrypts plaintext (as id-data) for every recipient
// and returns the DER ContentInfo.
func encryptEnvelopedData(plaintext []byte, recipients []cmsRecipient, transport KeyTransport, content ContentEncryption, useSKI bool, random io.Reader) ([]byte, error) {
	if len(recipients) == 0 || len(recipients) > maxCMSRecipients {
		return nil, fmt.Errorf("%w: between 1 and %d recipients are required", ErrInvalidEncryptedAttributesOptions, maxCMSRecipients)
	}
	contentOID, keySize, err := contentEncryptionParameters(content)
	if err != nil {
		return nil, err
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(random, key); err != nil {
		return nil, fmt.Errorf("deid: generate content-encryption key: %w", err)
	}
	defer clear(key)
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(random, iv); err != nil {
		return nil, fmt.Errorf("deid: generate content-encryption IV: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plaintext, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	clear(padded)
	ivParameter, err := asn1.Marshal(iv)
	if err != nil {
		return nil, err
	}

	version := 0
	infos := make([]asn1.RawValue, 0, len(recipients))
	for _, recipient := range recipients {
		info, err := keyTransRecipientInfo(recipient, key, transport, useSKI, random)
		if err != nil {
			return nil, err
		}
		if info.Version == 2 {
			version = 2
		}
		encoded, err := asn1.Marshal(info)
		if err != nil {
			return nil, err
		}
		infos = append(infos, asn1.RawValue{FullBytes: encoded})
	}
	enveloped := cmsEnvelopedData{
		Version:        version,
		RecipientInfos: infos,
		EncryptedContentInfo: cmsEncryptedContentInfo{
			ContentType:                oidCMSData,
			ContentEncryptionAlgorithm: pkix.AlgorithmIdentifier{Algorithm: contentOID, Parameters: asn1.RawValue{FullBytes: ivParameter}},
			EncryptedContent:           asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, Bytes: ciphertext},
		},
	}
	encoded, err := asn1.Marshal(enveloped)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(cmsContentInfo{
		ContentType: oidCMSEnvelopedData,
		Content:     asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: encoded},
	})
}

func contentEncryptionParameters(content ContentEncryption) (asn1.ObjectIdentifier, int, error) {
	switch content {
	case ContentEncryptionAES128CBC:
		return oidAES128CBC, 16, nil
	case ContentEncryptionAES192CBC:
		return oidAES192CBC, 24, nil
	case ContentEncryptionAES256CBC:
		return oidAES256CBC, 32, nil
	default:
		return nil, 0, fmt.Errorf("%w: unsupported content encryption %q", ErrInvalidEncryptedAttributesOptions, content)
	}
}

func keyTransRecipientInfo(recipient cmsRecipient, key []byte, transport KeyTransport, useSKI bool, random io.Reader) (cmsKeyTransRecipientInfo, error) {
	var info cmsKeyTransRecipientInfo
	if useSKI {
		if len(recipient.certificate.SubjectKeyId) == 0 {
			return info, fmt.Errorf("%w: certificate has no subject key identifier", ErrEncryptedAttributesRecipient)
		}
		info.Version = 2
		info.RID = asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, Bytes: recipient.certificate.SubjectKeyId}
	} else {
		issuerAndSerial, err := asn1.Marshal(cmsIssuerAndSerialNumber{
			Issuer:       asn1.RawValue{FullBytes: recipient.certificate.RawIssuer},
			SerialNumber: recipient.certificate.SerialNumber,
		})
		if err != nil {
			return info, fmt.Errorf("%w: %v", ErrEncryptedAttributesRecipient, err)
		}
		info.RID = asn1.RawValue{FullBytes: issuerAndSerial}
	}
	var err error
	switch transport {
	case KeyTransportRSAOAEPSHA256:
		info.KeyEncryptionAlgorithm, err = oaepAlgorithmIdentifier(oidSHA256)
		if err != nil {
			return info, err
		}
		info.EncryptedKey, err = rsa.EncryptOAEP(crypto.SHA256.New(), random, recipient.publicKey, key, nil)
	case KeyTransportRSAPKCS1v15:
		info.KeyEncryptionAlgorithm = pkix.AlgorithmIdentifier{Algorithm: oidRSAEncryption, Parameters: asn1.NullRawValue}
		info.EncryptedKey, err = rsa.EncryptPKCS1v15(random, recipient.publicKey, key)
	default:
		return info, fmt.Errorf("%w: unsupported key transport %q", ErrInvalidEncryptedAttributesOptions, transport)
	}
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrEncryptedAttributesRecipient, err)
	}
	return info, nil
}

// oaepAlgorithmIdentifier encodes RSAES-OAEP with hash and MGF1 over hash.
// SHA-2 identifiers carry absent parameters (RFC 5754 section 2).
func oaepAlgorithmIdentifier(hash asn1.ObjectIdentifier) (pkix.AlgorithmIdentifier, error) {
	hashAlgorithm := pkix.AlgorithmIdentifier{Algorithm: hash}
	mgfParameter, err := asn1.Marshal(hashAlgorithm)
	if err != nil {
		return pkix.AlgorithmIdentifier{}, err
	}
	parameters, err := asn1.Marshal(rsaesOAEPParams{
		HashFunc:    hashAlgorithm,
		MaskGenFunc: pkix.AlgorithmIdentifier{Algorithm: oidMGF1, Parameters: asn1.RawValue{FullBytes: mgfParameter}},
	})
	if err != nil {
		return pkix.AlgorithmIdentifier{}, err
	}
	return pkix.AlgorithmIdentifier{Algorithm: oidRSAESOAEP, Parameters: asn1.RawValue{FullBytes: parameters}}, nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	out := make([]byte, len(data)+padding)
	copy(out, data)
	for index := len(data); index < len(out); index++ {
		out[index] = byte(padding)
	}
	return out
}

// envelopedAlgorithms names the algorithms of a parsed EnvelopedData for a
// value-free report.
type envelopedAlgorithms struct {
	keyTransport      KeyTransport
	contentEncryption ContentEncryption
}

// decryptEnvelopedData decrypts a ContentInfo addressed to certificate with
// key. It returns ErrRecipientNotFound when no key transport recipient names
// the certificate, and ErrEncryptedAttributesDecrypt (without detail, so the
// failure is no padding oracle) when the key or content does not decrypt.
func decryptEnvelopedData(data []byte, certificate *x509.Certificate, key crypto.Decrypter) ([]byte, envelopedAlgorithms, error) {
	var algorithms envelopedAlgorithms
	der, err := berToDER(data)
	if err != nil {
		return nil, algorithms, err
	}
	var info cmsContentInfo
	if rest, err := asn1.Unmarshal(der, &info); err != nil || len(rest) != 0 {
		return nil, algorithms, fmt.Errorf("%w: content info", ErrMalformedEncryptedAttributes)
	}
	if !info.ContentType.Equal(oidCMSEnvelopedData) {
		return nil, algorithms, fmt.Errorf("%w: content type is not enveloped-data", ErrUnsupportedEncryptedAttributes)
	}
	var enveloped cmsEnvelopedData
	if rest, err := asn1.Unmarshal(info.Content.Bytes, &enveloped); err != nil || len(rest) != 0 {
		return nil, algorithms, fmt.Errorf("%w: enveloped-data", ErrMalformedEncryptedAttributes)
	}
	if len(enveloped.RecipientInfos) == 0 || len(enveloped.RecipientInfos) > maxCMSRecipients {
		return nil, algorithms, fmt.Errorf("%w: recipient count", ErrMalformedEncryptedAttributes)
	}
	content := enveloped.EncryptedContentInfo
	if !content.ContentType.Equal(oidCMSData) {
		return nil, algorithms, fmt.Errorf("%w: encrypted content type is not data", ErrUnsupportedEncryptedAttributes)
	}
	newCipher, keySize, blockSize, contentName, err := contentDecryptionAlgorithm(content.ContentEncryptionAlgorithm.Algorithm)
	if err != nil {
		return nil, algorithms, err
	}
	algorithms.contentEncryption = contentName
	var iv []byte
	if rest, err := asn1.Unmarshal(content.ContentEncryptionAlgorithm.Parameters.FullBytes, &iv); err != nil || len(rest) != 0 || len(iv) != blockSize {
		return nil, algorithms, fmt.Errorf("%w: content-encryption IV", ErrMalformedEncryptedAttributes)
	}
	ciphertext, err := encryptedContentBytes(content.EncryptedContent)
	if err != nil {
		return nil, algorithms, err
	}
	if len(ciphertext) == 0 || len(ciphertext)%blockSize != 0 {
		return nil, algorithms, fmt.Errorf("%w: encrypted content length", ErrMalformedEncryptedAttributes)
	}

	recipient, found, err := findKeyTransRecipient(enveloped.RecipientInfos, certificate)
	if err != nil {
		return nil, algorithms, err
	}
	if !found {
		return nil, algorithms, ErrRecipientNotFound
	}
	contentKey, transport, err := decryptContentKey(recipient, key, keySize)
	if err != nil {
		return nil, algorithms, err
	}
	defer clear(contentKey)
	algorithms.keyTransport = transport
	block, err := newCipher(contentKey)
	if err != nil {
		return nil, algorithms, ErrEncryptedAttributesDecrypt
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	unpadded, ok := pkcs7Unpad(plaintext, blockSize)
	if !ok {
		clear(plaintext)
		return nil, algorithms, ErrEncryptedAttributesDecrypt
	}
	return unpadded, algorithms, nil
}

func contentDecryptionAlgorithm(oid asn1.ObjectIdentifier) (func([]byte) (cipher.Block, error), int, int, ContentEncryption, error) {
	switch {
	case oid.Equal(oidAES128CBC):
		return aes.NewCipher, 16, aes.BlockSize, ContentEncryptionAES128CBC, nil
	case oid.Equal(oidAES192CBC):
		return aes.NewCipher, 24, aes.BlockSize, ContentEncryptionAES192CBC, nil
	case oid.Equal(oidAES256CBC):
		return aes.NewCipher, 32, aes.BlockSize, ContentEncryptionAES256CBC, nil
	case oid.Equal(oidDESEDE3CBC):
		return des.NewTripleDESCipher, 24, des.BlockSize, contentEncryptionDESEDE3CBC, nil
	default:
		return nil, 0, 0, "", fmt.Errorf("%w: content-encryption algorithm %s", ErrUnsupportedEncryptedAttributes, oid)
	}
}

// encryptedContentBytes returns the [0] IMPLICIT OCTET STRING content,
// joining the segments of its BER constructed form.
func encryptedContentBytes(value asn1.RawValue) ([]byte, error) {
	if value.Class != asn1.ClassContextSpecific || value.Tag != 0 {
		return nil, fmt.Errorf("%w: encrypted content is absent", ErrMalformedEncryptedAttributes)
	}
	if !value.IsCompound {
		return value.Bytes, nil
	}
	var out []byte
	rest := value.Bytes
	for len(rest) > 0 {
		var segment []byte
		var err error
		rest, err = asn1.Unmarshal(rest, &segment)
		if err != nil {
			return nil, fmt.Errorf("%w: encrypted content segment", ErrMalformedEncryptedAttributes)
		}
		out = append(out, segment...)
	}
	return out, nil
}

func findKeyTransRecipient(infos []asn1.RawValue, certificate *x509.Certificate) (cmsKeyTransRecipientInfo, bool, error) {
	for _, raw := range infos {
		if raw.Class != asn1.ClassUniversal || raw.Tag != asn1.TagSequence {
			// Key agreement, KEK, password and other recipients are not
			// supported; another recipient may still match.
			continue
		}
		var info cmsKeyTransRecipientInfo
		if rest, err := asn1.Unmarshal(raw.FullBytes, &info); err != nil || len(rest) != 0 {
			return info, false, fmt.Errorf("%w: key transport recipient", ErrMalformedEncryptedAttributes)
		}
		if recipientIdentifies(info, certificate) {
			return info, true, nil
		}
	}
	return cmsKeyTransRecipientInfo{}, false, nil
}

func recipientIdentifies(info cmsKeyTransRecipientInfo, certificate *x509.Certificate) bool {
	rid := info.RID
	if rid.Class == asn1.ClassContextSpecific && rid.Tag == 0 && !rid.IsCompound {
		return len(certificate.SubjectKeyId) > 0 && bytes.Equal(rid.Bytes, certificate.SubjectKeyId)
	}
	if rid.Class != asn1.ClassUniversal || rid.Tag != asn1.TagSequence {
		return false
	}
	var issuerAndSerial cmsIssuerAndSerialNumber
	if rest, err := asn1.Unmarshal(rid.FullBytes, &issuerAndSerial); err != nil || len(rest) != 0 || issuerAndSerial.SerialNumber == nil {
		return false
	}
	return bytes.Equal(issuerAndSerial.Issuer.FullBytes, certificate.RawIssuer) &&
		issuerAndSerial.SerialNumber.Cmp(certificate.SerialNumber) == 0
}

func decryptContentKey(info cmsKeyTransRecipientInfo, key crypto.Decrypter, keySize int) ([]byte, KeyTransport, error) {
	algorithm := info.KeyEncryptionAlgorithm
	switch {
	case algorithm.Algorithm.Equal(oidRSAEncryption):
		// A wrong key yields a random session key, not an error, so a
		// failure is indistinguishable from a content padding failure.
		contentKey, err := key.Decrypt(rand.Reader, info.EncryptedKey, &rsa.PKCS1v15DecryptOptions{SessionKeyLen: keySize})
		if err != nil || len(contentKey) != keySize {
			return nil, KeyTransportRSAPKCS1v15, ErrEncryptedAttributesDecrypt
		}
		return contentKey, KeyTransportRSAPKCS1v15, nil
	case algorithm.Algorithm.Equal(oidRSAESOAEP):
		options, transport, err := parseOAEPParameters(algorithm.Parameters)
		if err != nil {
			return nil, "", err
		}
		contentKey, err := key.Decrypt(rand.Reader, info.EncryptedKey, options)
		if err != nil || len(contentKey) != keySize {
			return nil, transport, ErrEncryptedAttributesDecrypt
		}
		return contentKey, transport, nil
	default:
		return nil, "", fmt.Errorf("%w: key-encryption algorithm %s", ErrUnsupportedEncryptedAttributes, algorithm.Algorithm)
	}
}

var oaepHashes = []struct {
	oid       asn1.ObjectIdentifier
	hash      crypto.Hash
	transport KeyTransport
}{
	{oidSHA1, crypto.SHA1, keyTransportRSAOAEPSHA1},
	{oidSHA256, crypto.SHA256, KeyTransportRSAOAEPSHA256},
	{oidSHA384, crypto.SHA384, keyTransportRSAOAEPSHA384},
	{oidSHA512, crypto.SHA512, keyTransportRSAOAEPSHA512},
}

func oaepHash(oid asn1.ObjectIdentifier) (crypto.Hash, KeyTransport, bool) {
	for _, candidate := range oaepHashes {
		if candidate.oid.Equal(oid) {
			return candidate.hash, candidate.transport, true
		}
	}
	return 0, "", false
}

// parseOAEPParameters applies the RFC 8017 defaults (SHA-1, MGF1 with
// SHA-1, empty label) to absent fields.
func parseOAEPParameters(raw asn1.RawValue) (*rsa.OAEPOptions, KeyTransport, error) {
	options := &rsa.OAEPOptions{Hash: crypto.SHA1, MGFHash: crypto.SHA1}
	transport := keyTransportRSAOAEPSHA1
	if len(raw.FullBytes) == 0 || bytes.Equal(raw.FullBytes, asn1.NullBytes) {
		return options, transport, nil
	}
	var params rsaesOAEPParams
	if rest, err := asn1.Unmarshal(raw.FullBytes, &params); err != nil || len(rest) != 0 {
		return nil, "", fmt.Errorf("%w: RSAES-OAEP parameters", ErrMalformedEncryptedAttributes)
	}
	if params.HashFunc.Algorithm != nil {
		hash, name, ok := oaepHash(params.HashFunc.Algorithm)
		if !ok {
			return nil, "", fmt.Errorf("%w: RSAES-OAEP hash %s", ErrUnsupportedEncryptedAttributes, params.HashFunc.Algorithm)
		}
		options.Hash, options.MGFHash, transport = hash, hash, name
	}
	if params.MaskGenFunc.Algorithm != nil {
		if !params.MaskGenFunc.Algorithm.Equal(oidMGF1) {
			return nil, "", fmt.Errorf("%w: RSAES-OAEP mask generation %s", ErrUnsupportedEncryptedAttributes, params.MaskGenFunc.Algorithm)
		}
		var mgfHash pkix.AlgorithmIdentifier
		if rest, err := asn1.Unmarshal(params.MaskGenFunc.Parameters.FullBytes, &mgfHash); err != nil || len(rest) != 0 {
			return nil, "", fmt.Errorf("%w: MGF1 parameters", ErrMalformedEncryptedAttributes)
		}
		hash, _, ok := oaepHash(mgfHash.Algorithm)
		if !ok {
			return nil, "", fmt.Errorf("%w: MGF1 hash %s", ErrUnsupportedEncryptedAttributes, mgfHash.Algorithm)
		}
		options.MGFHash = hash
	}
	if params.PSourceFunc.Algorithm != nil {
		if !params.PSourceFunc.Algorithm.Equal(oidPSpecified) {
			return nil, "", fmt.Errorf("%w: RSAES-OAEP label source %s", ErrUnsupportedEncryptedAttributes, params.PSourceFunc.Algorithm)
		}
		var label []byte
		if rest, err := asn1.Unmarshal(params.PSourceFunc.Parameters.FullBytes, &label); err != nil || len(rest) != 0 {
			return nil, "", fmt.Errorf("%w: RSAES-OAEP label", ErrMalformedEncryptedAttributes)
		}
		options.Label = label
	}
	return options, transport, nil
}

// pkcs7Unpad removes and checks the padding without branching on the
// padding bytes.
func pkcs7Unpad(data []byte, blockSize int) ([]byte, bool) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, false
	}
	padding := int(data[len(data)-1])
	good := subtle.ConstantTimeLessOrEq(1, padding) & subtle.ConstantTimeLessOrEq(padding, blockSize)
	for index := 1; index <= blockSize; index++ {
		inPadding := subtle.ConstantTimeLessOrEq(index, padding)
		matches := subtle.ConstantTimeByteEq(data[len(data)-index], byte(padding))
		good &= subtle.ConstantTimeSelect(inPadding, matches, 1)
	}
	if good != 1 {
		return nil, false
	}
	return data[:len(data)-padding], true
}

var errBER = fmt.Errorf("%w: BER encoding", ErrMalformedEncryptedAttributes)

// berToDER re-encodes BER as the DER subset encoding/asn1 reads: definite
// minimal lengths, and universal OCTET STRINGs in primitive form. Other
// constructed values keep their constructed form with definite lengths.
func berToDER(data []byte) ([]byte, error) {
	out, rest, err := berElement(data, 0)
	if err != nil {
		return nil, err
	}
	// Encrypted Content (OB) pads an odd-length encoding with one zero.
	if len(rest) > 1 || (len(rest) == 1 && rest[0] != 0) {
		return nil, errBER
	}
	return out, nil
}

// berElement re-encodes the first element of data and returns what follows.
func berElement(data []byte, depth int) ([]byte, []byte, error) {
	if depth > maxBERDepth {
		return nil, nil, errBER
	}
	identifierEnd, err := berIdentifierLength(data)
	if err != nil {
		return nil, nil, err
	}
	identifier := data[:identifierEnd]
	constructed := identifier[0]&0x20 != 0
	length, lengthEnd, indefinite, err := berLength(data[identifierEnd:])
	if err != nil {
		return nil, nil, err
	}
	body := data[identifierEnd+lengthEnd:]
	if indefinite && !constructed {
		return nil, nil, errBER
	}
	if !constructed {
		if length > len(body) {
			return nil, nil, errBER
		}
		return derTLV(identifier, body[:length]), body[length:], nil
	}
	var children [][]byte
	var rest []byte
	if indefinite {
		rest = body
		for {
			if len(rest) >= 2 && rest[0] == 0 && rest[1] == 0 {
				rest = rest[2:]
				break
			}
			if len(rest) == 0 {
				return nil, nil, errBER
			}
			var child []byte
			child, rest, err = berElement(rest, depth+1)
			if err != nil {
				return nil, nil, err
			}
			children = append(children, child)
		}
	} else {
		if length > len(body) {
			return nil, nil, errBER
		}
		inner := body[:length]
		rest = body[length:]
		for len(inner) > 0 {
			var child []byte
			child, inner, err = berElement(inner, depth+1)
			if err != nil {
				return nil, nil, err
			}
			children = append(children, child)
		}
	}
	// A universal constructed OCTET STRING becomes one primitive string.
	if identifier[0] == 0x24 && len(identifier) == 1 {
		var joined []byte
		for _, child := range children {
			var segment []byte
			if remaining, err := asn1.Unmarshal(child, &segment); err != nil || len(remaining) != 0 {
				return nil, nil, errBER
			}
			joined = append(joined, segment...)
		}
		return derTLV([]byte{0x04}, joined), rest, nil
	}
	return derTLV(identifier, bytes.Join(children, nil)), rest, nil
}

func berIdentifierLength(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, errBER
	}
	if data[0]&0x1f != 0x1f {
		return 1, nil
	}
	for index := 1; index < len(data) && index <= 5; index++ {
		if data[index]&0x80 == 0 {
			return index + 1, nil
		}
	}
	return 0, errBER
}

func berLength(data []byte) (length, consumed int, indefinite bool, err error) {
	if len(data) == 0 {
		return 0, 0, false, errBER
	}
	first := data[0]
	switch {
	case first < 0x80:
		return int(first), 1, false, nil
	case first == 0x80:
		return 0, 1, true, nil
	case first == 0xff:
		return 0, 0, false, errBER
	}
	count := int(first & 0x7f)
	if count > 4 || len(data) < 1+count {
		return 0, 0, false, errBER
	}
	for _, octet := range data[1 : 1+count] {
		length = length<<8 | int(octet)
	}
	if length < 0 {
		return 0, 0, false, errBER
	}
	return length, 1 + count, false, nil
}

func derTLV(identifier, content []byte) []byte {
	out := make([]byte, 0, len(identifier)+6+len(content))
	out = append(out, identifier...)
	switch length := len(content); {
	case length < 0x80:
		out = append(out, byte(length))
	default:
		var octets []byte
		for remaining := length; remaining > 0; remaining >>= 8 {
			octets = append([]byte{byte(remaining)}, octets...)
		}
		out = append(out, 0x80|byte(len(octets)))
		out = append(out, octets...)
	}
	return append(out, content...)
}
