package jpeg2000

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/jpip"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
)

func jpipDataset(metadata pixeldata.Metadata) *object.Object {
	u16 := func(group, element uint16, value uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUS}, Value: core.Uint16Value{value}}
	}
	elements := []core.Element{
		u16(0x0028, 0x0002, metadata.SamplesPerPixel),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{metadata.PhotometricInterpretation}},
		u16(0x0028, 0x0010, metadata.Rows),
		u16(0x0028, 0x0011, metadata.Columns),
		u16(0x0028, 0x0100, metadata.BitsAllocated),
		u16(0x0028, 0x0101, metadata.BitsStored),
		u16(0x0028, 0x0102, metadata.HighBit),
		u16(0x0028, 0x0103, metadata.PixelRepresentation),
	}
	if metadata.SamplesPerPixel > 1 {
		elements = append(elements, u16(0x0028, 0x0006, metadata.PlanarConfiguration))
	}
	return object.FromElements(elements, nil)
}

// TestJPIPServerFramesDecodeLikeTheSource serves every JPEG 2000 Part 1
// corpus codestream through jpip.Server and decodes what jpip.Client
// retrieves with the core backend: the default complete codestream, a
// JPP-stream (precinct data-bins rebuilt in RPCL order) and a JPT-stream
// (tile data-bins) must all decode to the samples of the source codestream.
// Reduced view windows must decode, at that reduction, like the source.
func TestJPIPServerFramesDecodeLikeTheSource(t *testing.T) {
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	tested := 0
	for _, fixture := range fixtures {
		if fixture.Family != "jpeg-2000-part-1" || fixture.Comparison == "expected-rejection" ||
			!strings.HasSuffix(fixture.Path, ".j2k") {
			continue
		}
		t.Run(fixture.ID, func(t *testing.T) {
			path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.Path)
			if err != nil {
				t.Fatal(err)
			}
			codestream, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := decodeCorpusFixture(t, fixture, CoreOptions{})
			if err != nil {
				t.Fatal(err)
			}
			index, err := jpip.IndexCodestream(codestream, jpip.IndexLimits{})
			if err != nil {
				t.Fatal(err)
			}
			server, err := jpip.NewServer(jpip.ServerConfig{Provider: jpip.StaticTargets{"frame": {Codestreams: [][]byte{codestream}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			ts := httptest.NewServer(server)
			defer ts.Close()
			registry := pixeldata.NewMemoryRegistry()
			if err := RegisterCore(registry, CoreOptions{}); err != nil {
				t.Fatal(err)
			}
			rule, err := jpip.RuleFromURL(ts.URL)
			if err != nil {
				t.Fatal(err)
			}
			client, err := jpip.NewClient(jpip.Config{HTTPClient: ts.Client(), Policy: jpip.Policy{Rules: []jpip.Rule{rule}}, Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			reference := pixeldata.JPIPReference{PixelDataProviderURL: ts.URL + "/?target=frame"}
			kinds := []jpip.StreamType{"", jpip.StreamTypeJPT}
			if index.Precincts() {
				kinds = append(kinds, jpip.StreamTypeJPP)
			}
			for _, kind := range kinds {
				decoded, err := client.DecodeFrame(context.Background(), jpip.Request{Reference: reference, Type: kind}, jpipDataset(corpusMetadata(fixture.Input)))
				if err != nil {
					t.Fatalf("type %q: %v", kind, err)
				}
				if decoded.Partial || !bytes.Equal(decoded.Data, want[0]) {
					t.Fatalf("type %q: partial=%v, samples differ=%v", kind, decoded.Partial, !bytes.Equal(decoded.Data, want[0]))
				}
			}
			if !index.Precincts() {
				return
			}
			width, height := index.Size()
			for reduce := 1; ; reduce++ {
				expected, err := DecodeCoreReduced(context.Background(), codestream, reduce, CoreOptions{})
				if err != nil {
					break // past the decomposition depth
				}
				// fsiz is measured on the reference grid (ISO/IEC 15444-9
				// C.4.2), so a sub-sampled image's decoded size differs.
				fx, fy := index.ReducedSize(reduce)
				rebuilt := reducedWindow(t, ts.URL, fx, fy)
				got, err := DecodeCoreReduced(context.Background(), rebuilt, reduce, CoreOptions{})
				if err != nil {
					t.Fatalf("reduce %d: %v", reduce, err)
				}
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("reduce %d of %dx%d: rebuilt window decodes differently", reduce, width, height)
				}
			}
		})
		tested++
	}
	if tested < 15 {
		t.Fatalf("only %d corpus codestreams tested", tested)
	}
}

// reducedWindow requests a JPP-stream for a reduced frame size and rebuilds
// the codestream, which then holds only the precincts that size needs.
func reducedWindow(t *testing.T, base string, width, height int) []byte {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/?target=frame&type=jpp-stream&fsiz=%d,%d", base, width, height))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get("JPIP-fsiz"); got != fmt.Sprintf("%d,%d", width, height) {
		t.Fatalf("JPIP-fsiz = %q, want %d,%d", got, width, height)
	}
	stream, err := jpip.ParseStream(body)
	if err != nil {
		t.Fatal(err)
	}
	set := jpip.NewDataBinSet(0)
	if err := set.AddStream(stream); err != nil {
		t.Fatal(err)
	}
	rec, err := set.Codestream(1)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Complete {
		t.Fatal("reduced window rebuilt as the complete image")
	}
	return rec.Codestream
}
