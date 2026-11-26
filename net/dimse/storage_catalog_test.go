package dimse

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/dictionary/uid"
)

func TestDefaultStorageProfilesUseTheCatalog(t *testing.T) {
	all := DefaultStorageSOPClassUIDs()
	if len(all) != len(uid.StorageSOPClasses()) {
		t.Fatalf("SCP profile has %d classes, the catalog %d", len(all), len(uid.StorageSOPClasses()))
	}
	get := DefaultCGetStorageSOPClassUIDs()
	if len(get) > uid.MaxSCUStorageContexts || len(get) < 100 {
		t.Fatalf("C-GET proposes %d classes", len(get))
	}
	// Every class this library reads is received and retrieved.
	for _, sopClassUID := range libraryStorageSOPClassUIDs {
		if !containsUID(all, sopClassUID) || !containsUID(get, sopClassUID) {
			t.Errorf("library class %s missing", sopClassUID)
		}
	}
	for _, sopClassUID := range []string{"1.2.840.10008.5.1.4.1.1.481.5", "1.2.840.10008.5.1.4.1.1.1.2", "1.2.840.10008.5.1.4.1.1.77.1.4",
		// The CT For Processing classes of Supplement 252 (#2027).
		"1.2.840.10008.5.1.4.1.1.2.3", "1.2.840.10008.5.1.4.1.1.2.4", "1.2.840.10008.5.1.4.1.1.2.5"} {
		if !containsUID(get, sopClassUID) {
			t.Errorf("C-GET subset lacks %s", sopClassUID)
		}
	}
}
