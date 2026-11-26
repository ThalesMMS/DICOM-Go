package uid

import (
	"reflect"
	"strings"
	"testing"
)

func TestStaticRegistryCompleteCatalog(t *testing.T) {
	byUID := make(map[string]Entry)
	byKeyword := make(map[string]Entry)
	for _, entry := range entries {
		byUID[entry.UID] = entry
		key := strings.ToLower(entry.Keyword)
		if _, exists := byKeyword[key]; !exists {
			byKeyword[key] = entry
		}
	}
	for key, expected := range byUID {
		for _, query := range []string{key, key + " \x00"} {
			if actual, ok := Dictionary.ByUID(query); !ok || actual != expected {
				t.Fatalf("UID %q differs from former catalog", query)
			}
		}
	}
	for key, expected := range byKeyword {
		for _, query := range []string{key, expected.Keyword, strings.ToUpper(key)} {
			if actual, ok := Dictionary.ByKeyword(query); !ok || actual != expected {
				t.Fatalf("keyword %q differs from former catalog", query)
			}
		}
	}
	for _, query := range []string{"", "0.0", " 1.2.840.10008.1.1", "9.9.9"} {
		if value, ok := Dictionary.ByUID(query); ok || value != (Entry{}) {
			t.Fatalf("unknown UID %q found", query)
		}
	}
}

func TestStaticRegistryTypeSnapshots(t *testing.T) {
	for typ := Type(0); typ < 12; typ++ {
		expected := make([]Entry, 0)
		for _, entry := range entries {
			if entry.Type == typ {
				expected = append(expected, entry)
			}
		}
		actual := EntriesByType(typ)
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("type %d order/data differ", typ)
		}
		if len(actual) > 0 {
			actual[0].Name = "mutated"
		}
		if !reflect.DeepEqual(EntriesByType(typ), expected) {
			t.Fatalf("type %d snapshot aliases catalog", typ)
		}
	}
	if actual := EntriesByType(Type(255)); actual == nil || len(actual) != 0 {
		t.Fatal("unknown type must retain non-nil empty snapshot")
	}
}
