package std

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

// This independent reference is the former map-building contract, including
// repeating-keyword canonicalization. It checks the complete generated catalog.
func TestStaticDictionaryCompleteCatalog(t *testing.T) {
	byTag := make(map[core.Tag]dictionary.Entry)
	byKeyword := make(map[string]dictionary.Entry)
	for _, entry := range entries {
		byTag[entry.Tag] = entry
		key := strings.ToLower(entry.Keyword)
		if _, exists := byKeyword[key]; !exists {
			byKeyword[key] = entry
		}
	}
	for tag, expected := range byTag {
		if actual, ok := Dictionary.ByTag(tag); !ok || actual != expected {
			t.Fatalf("tag %v differs from former catalog", tag)
		}
	}
	for key, expected := range byKeyword {
		for _, query := range []string{key, expected.Keyword, strings.ToUpper(key)} {
			if actual, ok := Dictionary.ByKeyword(query); !ok || actual != expected {
				t.Fatalf("keyword %q differs from former canonical entry", query)
			}
		}
	}
	for _, tag := range []core.Tag{core.NewTag(0, 0xFFFF), core.NewTag(0xFFFF, 0xFFFF), core.NewTag(0x9999, 1)} {
		if _, known := byTag[tag]; known {
			t.Fatal("unknown control collides with catalog")
		}
		if value, ok := Dictionary.ByTag(tag); ok || value != (dictionary.Entry{}) {
			t.Fatalf("unknown tag %v found", tag)
		}
		if _, ok := Dictionary.(table).VRSpecByTag(tag); ok {
			t.Fatalf("unknown VR tag %v found", tag)
		}
	}
	for _, key := range []string{"", " nonexistent ", "PATIENTNAME ", "Δunknown"} {
		if _, ok := Dictionary.ByKeyword(key); ok {
			t.Fatalf("unknown keyword %q found", key)
		}
	}
}

func TestStaticDictionaryContextAndConcurrentReaders(t *testing.T) {
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tag, expected := range contextualVRSpecs {
				actual, ok := dictionary.LookupVRSpec(Dictionary, tag)
				if !ok || actual.Context() != expected.Context() || !reflect.DeepEqual(actual.Values(), expected.Values()) {
					t.Errorf("contextual VR %v changed", tag)
				}
				values := actual.Values()
				if len(values) > 0 {
					values[0] = core.VRUN
				}
				again, _ := dictionary.LookupVRSpec(Dictionary, tag)
				if !reflect.DeepEqual(again.Values(), expected.Values()) {
					t.Errorf("contextual VR %v aliases public slice", tag)
				}
			}
		}()
	}
	wg.Wait()
}
