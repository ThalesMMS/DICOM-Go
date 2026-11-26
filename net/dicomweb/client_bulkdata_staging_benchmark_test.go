package dicomweb

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
)

// Run each size/mode in its own process for whole-process peak RSS. The server
// generates a deterministic stream in bounded chunks: no full-payload fixture.
// bytes/op and allocs/op include HTTP, test server and filesystem overhead.
func BenchmarkBulkDataResolutionStaging(b *testing.B) {
	for _, size := range []int64{1 << 20, 8 << 20, 32 << 20} {
		for _, staged := range []bool{false, true} {
			name := "buffered"
			if staged {
				name = "file"
			}
			b.Run(fmt.Sprintf("%s/%dMiB", name, size>>20), func(b *testing.B) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveBulkPattern(w, size) }))
				defer server.Close()
				client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
				var resolver interface {
					ResolveBulkDataURI(context.Context, string, int64) (object.BulkDataSource, error)
				} = client
				if staged {
					resolver = StagedBulkDataResolver{Client: client, Staging: FileBulkDataStaging{Directory: b.TempDir(), MaxBytes: size}, ChunkBytes: 32 << 10}
				}
				buffer := make([]byte, 32<<10)
				b.ReportAllocs()
				b.SetBytes(size)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					source, err := resolver.ResolveBulkDataURI(context.Background(), "/bulk", size)
					if err != nil {
						b.Fatal(err)
					}
					read, readErr := io.CopyBuffer(io.Discard, source.Reader, buffer)
					closeErr := source.Reader.Close()
					if readErr != nil || closeErr != nil || read != size || source.Size != size {
						b.Fatalf("read=%d/%d declared=%d errors=%v/%v", read, size, source.Size, readErr, closeErr)
					}
				}
			})
		}
	}
}
