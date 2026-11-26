package deid

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrInvalidPseudonymTemplate reports a template that is empty, has no
	// identifying token, uses an unknown token, or renders characters or
	// lengths the target VR does not allow.
	ErrInvalidPseudonymTemplate = errors.New("deid: invalid pseudonym template")
	// ErrPseudonymSecretRequired reports a {hash} template without a secret of
	// at least MinPseudonymSecretBytes bytes.
	ErrPseudonymSecretRequired = errors.New("deid: keyed pseudonym template requires a secret")
	// ErrPseudonymCollision reports that two different original identities
	// rendered the same pseudonym. Distinct patients are never merged; use a
	// longer {hash:N} or a {counter} token.
	ErrPseudonymCollision = errors.New("deid: pseudonym collision between distinct original identities")
)

// MinPseudonymSecretBytes is the shortest accepted secret for {hash} tokens.
const MinPseudonymSecretBytes = 16

// maxPseudonymLength is the LO and PN component-group limit (PS3.5 6.2).
const maxPseudonymLength = 64

// PseudonymTemplate renders replacement Patient's Name (0010,0010) and
// Patient ID (0010,0020) values. A template is literal text with tokens:
//
//   - {counter} or {counter:N}: a sequence number assigned in first-seen
//     order by one Pseudonymizer, zero-padded to N digits (1..12). Stable
//     within one Pseudonymizer only.
//   - {hash} or {hash:N}: the first N (1..64, default 12) upper-case
//     hexadecimal digits of HMAC-SHA256(secret, original identity).
//     Deterministic for every Pseudonymizer holding the same secret.
//
// Literals are printable ASCII without backslash, braces or "=", and Patient
// ID literals also exclude "^". An empty field leaves that attribute as the
// Basic Profile made it (zero length). Every non-empty field needs a token so
// that distinct patients never share a pseudonym.
type PseudonymTemplate struct {
	PatientName string
	PatientID   string
}

// Pseudonym is one rendered replacement. Empty fields are not replaced.
type Pseudonym struct {
	PatientName string
	PatientID   string
}

type pseudonymPart struct {
	literal string
	token   string // "", "counter" or "hash"
	width   int
}

type compiledPseudonymField struct {
	parts []pseudonymPart
}

// Pseudonymizer assigns one pseudonym per original identity. It retains only
// keyed digests of original identities, never the identities themselves, and
// is safe for concurrent use.
type Pseudonymizer struct {
	mu       sync.Mutex
	name     compiledPseudonymField
	id       compiledPseudonymField
	secret   []byte
	indexKey []byte
	byKey    map[string]Pseudonym
	owners   map[string]string
	next     int
}

// NewPseudonymizer validates template and returns a Pseudonymizer. secret is
// held by the caller (for example an installation key) and is required only
// for {hash} tokens; it is copied and never reported.
func NewPseudonymizer(template PseudonymTemplate, secret []byte) (*Pseudonymizer, error) {
	name, err := compilePseudonymField(template.PatientName, true)
	if err != nil {
		return nil, err
	}
	id, err := compilePseudonymField(template.PatientID, false)
	if err != nil {
		return nil, err
	}
	if len(name.parts) == 0 && len(id.parts) == 0 {
		return nil, fmt.Errorf("%w: both fields are empty", ErrInvalidPseudonymTemplate)
	}
	if (name.uses("hash") || id.uses("hash")) && len(secret) < MinPseudonymSecretBytes {
		return nil, ErrPseudonymSecretRequired
	}
	indexKey := make([]byte, sha256.Size)
	if _, err := rand.Read(indexKey); err != nil {
		return nil, errors.New("deid: pseudonymizer setup failed")
	}
	return &Pseudonymizer{
		name: name, id: id,
		secret:   append([]byte(nil), secret...),
		indexKey: indexKey,
		byKey:    make(map[string]Pseudonym),
		owners:   make(map[string]string),
	}, nil
}

func compilePseudonymField(template string, personName bool) (compiledPseudonymField, error) {
	var field compiledPseudonymField
	if template == "" {
		return field, nil
	}
	hasToken := false
	for rest := template; rest != ""; {
		open := strings.IndexByte(rest, '{')
		literal := rest
		if open >= 0 {
			literal = rest[:open]
		}
		if err := validatePseudonymLiteral(literal, personName); err != nil {
			return compiledPseudonymField{}, err
		}
		if literal != "" {
			field.parts = append(field.parts, pseudonymPart{literal: literal})
		}
		if open < 0 {
			break
		}
		closing := strings.IndexByte(rest[open:], '}')
		if closing < 0 {
			return compiledPseudonymField{}, fmt.Errorf("%w: unterminated token", ErrInvalidPseudonymTemplate)
		}
		part, err := parsePseudonymToken(rest[open+1 : open+closing])
		if err != nil {
			return compiledPseudonymField{}, err
		}
		field.parts = append(field.parts, part)
		hasToken = true
		rest = rest[open+closing+1:]
	}
	if !hasToken {
		return compiledPseudonymField{}, fmt.Errorf("%w: a template needs a {counter} or {hash} token", ErrInvalidPseudonymTemplate)
	}
	if field.maxLength() > maxPseudonymLength {
		return compiledPseudonymField{}, fmt.Errorf("%w: rendered value may exceed %d characters", ErrInvalidPseudonymTemplate, maxPseudonymLength)
	}
	return field, nil
}

func parsePseudonymToken(token string) (pseudonymPart, error) {
	name, widthText, hasWidth := strings.Cut(token, ":")
	part := pseudonymPart{token: name}
	switch name {
	case "counter":
		part.width = 1
		if hasWidth {
			width, err := strconv.Atoi(widthText)
			if err != nil || width < 1 || width > 12 {
				return pseudonymPart{}, fmt.Errorf("%w: counter width must be 1..12", ErrInvalidPseudonymTemplate)
			}
			part.width = width
		}
	case "hash":
		part.width = 12
		if hasWidth {
			width, err := strconv.Atoi(widthText)
			if err != nil || width < 1 || width > 2*sha256.Size {
				return pseudonymPart{}, fmt.Errorf("%w: hash width must be 1..64", ErrInvalidPseudonymTemplate)
			}
			part.width = width
		}
	default:
		return pseudonymPart{}, fmt.Errorf("%w: unknown token", ErrInvalidPseudonymTemplate)
	}
	return part, nil
}

func validatePseudonymLiteral(literal string, personName bool) error {
	for _, r := range literal {
		switch {
		case r < 0x20 || r > 0x7e, r == '\\', r == '{', r == '}', r == '=':
			return fmt.Errorf("%w: literal characters must be printable ASCII without \\ { } =", ErrInvalidPseudonymTemplate)
		case r == '^' && !personName:
			return fmt.Errorf("%w: Patient ID literals cannot contain ^", ErrInvalidPseudonymTemplate)
		}
	}
	return nil
}

func (field compiledPseudonymField) uses(token string) bool {
	for _, part := range field.parts {
		if part.token == token {
			return true
		}
	}
	return false
}

// maxLength counts counters at their padded width; render rejects a counter
// that has grown past the VR limit.
func (field compiledPseudonymField) maxLength() int {
	total := 0
	for _, part := range field.parts {
		total += len(part.literal) + part.width
	}
	return total
}

func (field compiledPseudonymField) render(counter int, hash string) (string, error) {
	if len(field.parts) == 0 {
		return "", nil
	}
	var out strings.Builder
	for _, part := range field.parts {
		switch part.token {
		case "counter":
			fmt.Fprintf(&out, "%0*d", part.width, counter)
		case "hash":
			out.WriteString(hash[:part.width])
		default:
			out.WriteString(part.literal)
		}
	}
	if out.Len() > maxPseudonymLength {
		return "", fmt.Errorf("%w: rendered value exceeds %d characters", ErrInvalidPseudonymTemplate, maxPseudonymLength)
	}
	return out.String(), nil
}

// Pseudonym returns the pseudonym for one original identity, assigning a new
// one on first sight. identity is any stable caller key (for example Issuer
// of Patient ID and Patient ID); it is never stored or reported. Two distinct
// identities that render the same Patient ID or Patient's Name fail with
// ErrPseudonymCollision instead of being merged.
func (p *Pseudonymizer) Pseudonym(identity string) (Pseudonym, error) {
	if p == nil {
		return Pseudonym{}, ErrInvalidPseudonymTemplate
	}
	indexMAC := hmac.New(sha256.New, p.indexKey)
	indexMAC.Write([]byte(identity))
	key := string(indexMAC.Sum(nil))

	p.mu.Lock()
	defer p.mu.Unlock()
	if existing, ok := p.byKey[key]; ok {
		return existing, nil
	}
	hash := ""
	if len(p.secret) > 0 {
		mac := hmac.New(sha256.New, p.secret)
		mac.Write([]byte("dicom-go/deid pseudonym v1\x00"))
		mac.Write([]byte(identity))
		hash = strings.ToUpper(hex.EncodeToString(mac.Sum(nil)))
	}
	counter := p.next + 1
	name, err := p.name.render(counter, hash)
	if err != nil {
		return Pseudonym{}, err
	}
	id, err := p.id.render(counter, hash)
	if err != nil {
		return Pseudonym{}, err
	}
	for _, owned := range []string{"id\x00" + id, "name\x00" + name} {
		if strings.HasSuffix(owned, "\x00") {
			continue
		}
		if owner, taken := p.owners[owned]; taken && owner != key {
			return Pseudonym{}, ErrPseudonymCollision
		}
	}
	if id != "" {
		p.owners["id\x00"+id] = key
	}
	if name != "" {
		p.owners["name\x00"+name] = key
	}
	p.next = counter
	pseudonym := Pseudonym{PatientName: name, PatientID: id}
	p.byKey[key] = pseudonym
	return pseudonym, nil
}

// PatientPseudonymIdentity returns the stable identity key the Basic Profile
// uses for obj: Issuer of Patient ID and Patient ID, or, when Patient ID is
// empty, Patient's Name and Birth Date. Objects without any of these share
// one identity.
func PatientPseudonymIdentity(obj *object.Object) string {
	if obj == nil {
		return ""
	}
	value := func(tag core.Tag) string {
		text, _ := obj.GetString(tag)
		return strings.TrimSpace(text)
	}
	if id := value(tagPatientID); id != "" {
		return "id\x00" + value(tagIssuerOfPatientID) + "\x00" + id
	}
	if name := value(tagPatientName); name != "" {
		return "name\x00" + name + "\x00" + value(tagPatientBirthDate)
	}
	return ""
}

// applyPseudonym replaces the planned Patient's Name and Patient ID with the
// pseudonym of the source identity. PS3.15 Table E.1-1 assigns both action
// Z, which permits a dummy value consistent with the VR.
func (state *profilePlanner) applyPseudonym(source, planned *object.Object) error {
	pseudonymizer := state.options.Pseudonymizer
	if pseudonymizer == nil {
		return nil
	}
	pseudonym, err := pseudonymizer.Pseudonym(PatientPseudonymIdentity(source))
	if err != nil {
		return err
	}
	if pseudonym.PatientName != "" {
		planned.Put(core.NewRawElement(tagPatientName, core.VRPN, padText(pseudonym.PatientName)))
	}
	if pseudonym.PatientID != "" {
		planned.Put(core.NewRawElement(tagPatientID, core.VRLO, padText(pseudonym.PatientID)))
	}
	state.report.Summary.PatientPseudonyms = 1
	state.pseudonymized = true
	return nil
}
