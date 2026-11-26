package qrmatch

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	dicomencoding "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestMatchScopedCharacterSetAndTextDelimiter(t *testing.T) {
	seqTag := core.NewTag(0x0008, 0x0096)
	description := core.NewTag(0x4000, 0x4000)
	for _, tc := range []struct {
		name, queryCharset, candidateCharset string
		query, candidate                     core.Element
		override                             string
		match                                bool
	}{
		{
			name:         "Latin-1 query inherits parent, UTF-8 candidate inherits independently",
			queryCharset: "ISO_IR 100", candidateCharset: "ISO_IR 192",
			query: str(tags.PatientName, core.VRPN, "érable^C?lia"), candidate: str(tags.PatientName, core.VRPN, "Érable^Célia"),
			match: true,
		},
		{
			name:         "candidate item overrides parent",
			queryCharset: "ISO_IR 192", candidateCharset: "ISO_IR 192", override: "ISO_IR 100",
			query: str(tags.PatientName, core.VRPN, "Érable^Célia"), candidate: str(tags.PatientName, core.VRPN, "Érable^Célia"),
			match: true,
		},
		{
			name:         "JIS Roman Yen is text for LT",
			queryCharset: "ISO_IR 192", candidateCharset: "ISO_IR 13",
			query: str(description, core.VRLT, "A¥B"), candidate: str(description, core.VRLT, "A¥B"),
			match: true,
		},
		{
			name:         "literal backslash in LT does not create two values",
			queryCharset: "ISO_IR 192", candidateCharset: "ISO_IR 100",
			query: str(description, core.VRLT, "Clé"), candidate: str(description, core.VRLT, `Clé\Doc`),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := parsedMatchingDataset(t, str(specificCharacterSetTag, core.VRCS, tc.queryCharset),
				seq(seqTag, []core.Element{tc.query}))
			candidateItem := []core.Element{tc.candidate}
			if tc.override != "" {
				candidateItem = append(candidateItem, str(specificCharacterSetTag, core.VRCS, tc.override))
			}
			candidate := parsedMatchingDataset(t, str(specificCharacterSetTag, core.VRCS, tc.candidateCharset),
				seq(seqTag, candidateItem))
			result, err := Match(query, candidate, Options{})
			if err != nil || result.Match != tc.match {
				t.Fatalf("dataset match = %v, error = %v, want match %v", result.Match, err, tc.match)
			}

			// A caller can also match item facades taken from parent datasets.
			queryItems, _ := query.GetSequence(seqTag)
			candidateItems, _ := candidate.GetSequence(seqTag)
			result, err = Match(queryItems[0], candidateItems[0], Options{})
			if err != nil || result.Match != tc.match {
				t.Fatalf("item match = %v, error = %v, want match %v", result.Match, err, tc.match)
			}
		})
	}
}

func TestMatchUndecodableTextAndEncodedLimits(t *testing.T) {
	name := core.NewRawElement(tags.PatientName, core.VRPN, []byte("FIXTURE\xff"))
	malformed := dataset(str(specificCharacterSetTag, core.VRCS, "ISO_IR 192"), name)
	valid := dataset(str(tags.PatientName, core.VRPN, "FIXTURE*"))
	t.Run("redacted invalid query even without the matching key on candidate", func(t *testing.T) {
		got, err := Match(malformed, dataset(), Options{})
		if got.Match || !errors.Is(err, ErrInvalidQuery) || strings.Contains(err.Error(), "FIXTURE") || strings.Contains(err.Error(), "0xFF") {
			t.Fatalf("match = %v, invalid query error = %v", got.Match, err)
		}
	})
	t.Run("invalid candidate fails the comparison", func(t *testing.T) {
		got, err := Match(valid, malformed, Options{})
		if got.Match || err != nil {
			t.Fatalf("match = %v, error = %v", got.Match, err)
		}
	})
	t.Run("unsupported optional and empty keys require no text decoding", func(t *testing.T) {
		got, err := Match(malformed, dataset(), Options{SupportedMatching: []core.Tag{tags.StudyInstanceUID}})
		if !got.Match || err != nil || len(got.UnsupportedOptional) != 1 || got.UnsupportedOptional[0] != tags.PatientName {
			t.Fatalf("match = %+v, error = %v", got, err)
		}
		got, err = Match(dataset(str(specificCharacterSetTag, core.VRCS, "INVALID"), core.NewRawElement(tags.PatientName, core.VRPN, nil)),
			malformed, Options{})
		if !got.Match || err != nil {
			t.Fatalf("return key match = %+v, error = %v", got, err)
		}
	})
	t.Run("explicit fallback belongs to the dataset", func(t *testing.T) {
		withFallback := object.FromElementsWithTextOptions([]core.Element{name}, nil, object.TextOptions{
			FallbackCharacterSet: dicomencoding.ISOIR100, CharsetFallback: object.FallbackUndeclared,
		})
		got, err := Match(dataset(str(tags.PatientName, core.VRPN, "FIXTUREÿ")), withFallback, Options{})
		if !got.Match || err != nil {
			t.Fatalf("fallback match = %v, error = %v", got.Match, err)
		}
	})
	t.Run("encoded UTF-8 bytes are bounded before decoding", func(t *testing.T) {
		key := parsedMatchingDataset(t, str(specificCharacterSetTag, core.VRCS, "ISO_IR 192"),
			str(tags.PatientName, core.VRPN, strings.Repeat("É", 8)))
		if _, err := Match(key, key, Options{Limits: Limits{MaxValueBytes: 15}}); !errors.Is(err, ErrResourceLimit) {
			t.Fatalf("15 byte budget error = %v, want ErrResourceLimit", err)
		}
		got, err := Match(key, key, Options{Limits: Limits{MaxValueBytes: 16}})
		if !got.Match || err != nil {
			t.Fatalf("16 byte budget match = %v, error = %v", got.Match, err)
		}
	})
}

func TestMatchConcurrentBorrowedCharacterSetScopes(t *testing.T) {
	seqTag := core.NewTag(0x0008, 0x0096)
	query := parsedMatchingDataset(t, str(specificCharacterSetTag, core.VRCS, "ISO_IR 100"),
		seq(seqTag, []core.Element{str(tags.PatientName, core.VRPN, "Érable*")}))
	candidate := parsedMatchingDataset(t, str(specificCharacterSetTag, core.VRCS, "ISO_IR 192"),
		seq(seqTag, []core.Element{str(tags.PatientName, core.VRPN, "Érable^Célia")}))
	for range 8 {
		t.Run("shared datasets", func(t *testing.T) {
			t.Parallel()
			result, err := Match(query, candidate, Options{})
			if err != nil || !result.Match {
				t.Fatalf("match = %v, error = %v", result.Match, err)
			}
		})
	}
}

func parsedMatchingDataset(t *testing.T, elements ...core.Element) *object.Object {
	t.Helper()
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, dataset(elements...), transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadDataSet(&encoded, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
