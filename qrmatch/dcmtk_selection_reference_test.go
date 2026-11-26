package qrmatch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
)

// dcmdata/tests/tmatch.cc exercises whole-value wildcards and an adversarial
// series of alternating '*' and '?' tokens. A prefix-table oracle checks the
// same semantics across combinations without a wall-clock assertion.
func TestDCMTKReferenceWildcardCombinations(t *testing.T) {
	values := shortWildcardWords("AB", 3)
	for _, pattern := range shortWildcardWords("AB*?", 4) {
		query := dataset(str(tags.PatientID, core.VRLO, pattern))
		for _, value := range values {
			candidate := dataset(str(tags.PatientID, core.VRLO, value))
			want := pattern == "" || wildcardPrefixTable(pattern, value)
			got, err := Match(query, candidate, Options{})
			if err != nil || got.Match != want {
				t.Errorf("pattern %q on value %q = %v, %v, want %v", pattern, value, got.Match, err, want)
			}
		}
	}
}

func shortWildcardWords(alphabet string, maxLength int) []string {
	words := []string{""}
	prev := words
	for range maxLength {
		var next []string
		for _, prefix := range prev {
			for _, c := range alphabet {
				next = append(next, prefix+string(c))
			}
		}
		words = append(words, next...)
		prev = next
	}
	return words
}

func wildcardPrefixTable(pattern, value string) bool {
	chars := []rune(value)
	previous := make([]bool, len(chars)+1)
	previous[0] = true
	for _, token := range pattern {
		current := make([]bool, len(chars)+1)
		current[0] = token == '*' && previous[0]
		for i, char := range chars {
			switch token {
			case '*':
				current[i+1] = previous[i+1] || current[i]
			case '?':
				current[i+1] = previous[i]
			default:
				current[i+1] = previous[i] && token == char
			}
		}
		previous = current
	}
	return previous[len(chars)]
}

func TestDCMTKReferenceWildcardAdversarialGrowth(t *testing.T) {
	candidate := dataset(str(tags.PatientID, core.VRLO, "CT CHEST ABDOMEN PELVIS WITH IV CONTRAST"))
	for pairs := 1; pairs <= 20; pairs++ {
		prefix := strings.Repeat("*?", pairs)
		for _, tc := range []struct {
			suffix string
			want   bool
		}{{"X", false}, {"*", true}} {
			pattern := prefix + tc.suffix
			got, err := Match(dataset(str(tags.PatientID, core.VRLO, pattern)), candidate, Options{})
			if err != nil || got.Match != tc.want {
				t.Errorf("%d pairs and suffix %q = %v, %v, want %v", pairs, tc.suffix, got.Match, err, tc.want)
			}
		}
	}

	query := dataset(str(tags.PatientID, core.VRLO, "*?*?*"))
	if got, err := Match(query, candidate, Options{Limits: Limits{MaxWildcardSteps: 1}}); !errors.Is(err, ErrResourceLimit) || got.Match {
		t.Fatalf("wildcard budget result = %+v, %v, want resource limit", got, err)
	}
	if got, err := Match(query, candidate, Options{}); err != nil || !got.Match {
		t.Fatalf("fresh wildcard budget result = %+v, %v, want match", got, err)
	}
}

// dcmdata/tests/tfilter.cc distinguishes exact SOP Classes and allowlists,
// including the distinction between CT Image Storage and Enhanced CT.
func TestDCMTKReferenceSOPClassSelection(t *testing.T) {
	const (
		cr         = "1.2.840.10008.5.1.4.1.1.1"
		ct         = "1.2.840.10008.5.1.4.1.1.2"
		enhancedCT = "1.2.840.10008.5.1.4.1.1.2.1"
		mr         = "1.2.840.10008.5.1.4.1.1.4"
	)
	for _, filter := range []struct {
		name   string
		values []string
		accept []string
	}{
		{"CT only", []string{ct}, []string{ct}},
		{"CR CT and MR", []string{cr, ct, mr}, []string{cr, ct, mr}},
		{"CT and MR", []string{ct, mr}, []string{ct, mr}},
		{"empty return key", nil, []string{cr, ct, enhancedCT, mr, ""}},
		{"literal wildcard in UID", []string{ct + "*"}, nil},
	} {
		t.Run(filter.name, func(t *testing.T) {
			query := dataset(str(tags.SOPClassUID, core.VRUI, filter.values...))
			for _, class := range []string{cr, ct, enhancedCT, mr, ""} {
				candidate := dataset()
				if class != "" {
					candidate = dataset(str(tags.SOPClassUID, core.VRUI, class))
				}
				want := slices.Contains(filter.accept, class)
				got, err := Match(query, candidate, Options{})
				if err != nil || got.Match != want {
					t.Errorf("SOP Class %q = %v, %v, want %v", class, got.Match, err, want)
				}
			}
		})
	}
}

// dcmwlm/tests/twlmatch.cc exercises veterinary scalar, coded and nested
// registry keys. This checks the reusable predicate over varied item layouts.
func TestDCMTKReferenceVeterinaryWorklistMatching(t *testing.T) {
	var (
		speciesDescription = core.NewTag(0x0010, 0x2201)
		speciesCode        = core.NewTag(0x0010, 0x2202)
		breedCode          = core.NewTag(0x0010, 0x2293)
		registration       = core.NewTag(0x0010, 0x2294)
		registrationNumber = core.NewTag(0x0010, 0x2295)
		registryCode       = core.NewTag(0x0010, 0x2296)
		organization       = core.NewTag(0x0010, 0x2299)
		codeValue          = core.NewTag(0x0008, 0x0100)
		codingScheme       = core.NewTag(0x0008, 0x0102)
	)
	code := func(value, scheme string) []core.Element {
		return []core.Element{str(codeValue, core.VRSH, value), str(codingScheme, core.VRSH, scheme)}
	}
	registeredBreed := func(number string, codes ...[]core.Element) []core.Element {
		return []core.Element{str(registrationNumber, core.VRLO, number), seq(registryCode, codes...)}
	}
	queryBreed := func(number, value, scheme string) core.Element {
		return seq(registration, registeredBreed(number, code(value, scheme)))
	}
	for _, tc := range []struct {
		name        string
		keys        []core.Element
		supported   []core.Tag
		want        bool
		unsupported []core.Tag
	}{
		{"absent scalar and empty sequences are return keys", []core.Element{str(organization, core.VRLO), seq(speciesCode), seq(breedCode, nil), seq(registration, []core.Element{seq(registryCode)})}, nil, true, nil},
		{"exact species", []core.Element{str(speciesDescription, core.VRLO, "Canine")}, nil, true, nil},
		{"different species", []core.Element{str(speciesDescription, core.VRLO, "Feline")}, nil, false, nil},
		{"wildcard species and breed code", []core.Element{str(speciesDescription, core.VRLO, "Can*"), seq(breedCode, code("132380002", "SCT"))}, nil, true, nil},
		{"breed value under wrong scheme", []core.Element{seq(breedCode, code("132380002", "DCM"))}, nil, false, nil},
		{"matching nested registry in later item", []core.Element{queryBreed("REG02", "109200", "DCM")}, nil, true, nil},
		{"registry match cannot borrow number from another item", []core.Element{queryBreed("REG01", "109200", "DCM")}, nil, false, nil},
		{"scheme and value cannot span nested items", []core.Element{queryBreed("REG01", "109200", "SCT")}, nil, false, nil},
		{"wrong parent sequence", []core.Element{seq(speciesCode, code("132380002", "SCT"))}, nil, false, nil},
		{"no match for absent populated organization", []core.Element{str(organization, core.VRLO, "ANIMAL CLINIC")}, nil, false, nil},
		{"unsupported nonempty key is reported", []core.Element{str(speciesDescription, core.VRLO, "Canine"), str(organization, core.VRLO, "ANIMAL CLINIC")}, []core.Tag{speciesDescription}, true, []core.Tag{organization}},
		{"unsupported empty key stays universal", []core.Element{str(organization, core.VRLO), seq(registration)}, []core.Tag{speciesDescription}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := dataset(
				str(speciesDescription, core.VRLO, "Canine"),
				seq(speciesCode, code("448771007", "SCT")),
				seq(breedCode, code("132380002", "SCT")),
				seq(registration,
					registeredBreed("REG01", code("109200", "99TEST"), code("109250", "SCT")),
					registeredBreed("REG02", code("109200", "DCM")),
				),
			)
			result, err := Match(dataset(tc.keys...), candidate, Options{SupportedMatching: tc.supported})
			if err != nil || result.Match != tc.want || !slices.Equal(result.UnsupportedOptional, tc.unsupported) {
				t.Fatalf("Match = %+v, %v, want %v and unsupported %v", result, err, tc.want, tc.unsupported)
			}
		})
	}
}
