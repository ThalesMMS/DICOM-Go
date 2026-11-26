package jpip

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestOpenJPEGInterop is opt-in (DICOMGO_JPIP_OPENJPEG=1 with opj_compress
// and opj_decompress on PATH). It encodes synthetic images with OpenJPEG in
// every progression order, with and without PLT, SOP and EPH, tiles and
// small precincts; checks that PLT and packet-header indexing agree; serves
// each through the Server; and has opj_decompress decode the rebuilt
// JPP-stream codestreams, full size and reduced, comparing them with the
// source decoded the same way.
func TestOpenJPEGInterop(t *testing.T) {
	if os.Getenv("DICOMGO_JPIP_OPENJPEG") != "1" {
		t.Skip("set DICOMGO_JPIP_OPENJPEG=1 to run the OpenJPEG interoperability check")
	}
	compress, err := exec.LookPath("opj_compress")
	if err != nil {
		t.Fatal("opj_compress not found")
	}
	decompress, err := exec.LookPath("opj_decompress")
	if err != nil {
		t.Fatal("opj_decompress not found")
	}
	dir := t.TempDir()
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(name), args, err, out)
		}
	}
	gray := filepath.Join(dir, "gray.pgm")
	color := filepath.Join(dir, "color.ppm")
	var pixels []byte
	for i := 0; i < 61*47; i++ {
		pixels = append(pixels, byte(i*7+i/61*13))
	}
	if err := os.WriteFile(gray, append([]byte("P5\n61 47\n255\n"), pixels...), 0o600); err != nil {
		t.Fatal(err)
	}
	pixels = pixels[:0]
	for i := 0; i < 40*36*3; i++ {
		pixels = append(pixels, byte(i*11+i/120*5))
	}
	if err := os.WriteFile(color, append([]byte("P6\n40 36\n255\n"), pixels...), 0o600); err != nil {
		t.Fatal(err)
	}
	decode := func(codestream []byte, reduce int, name string) []byte {
		t.Helper()
		in := filepath.Join(dir, name+".j2k")
		out := filepath.Join(dir, name+".raw")
		if err := os.WriteFile(in, codestream, 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Remove(out)
		run(decompress, "-i", in, "-o", out, "-r", fmt.Sprint(reduce))
		data, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, source := range []string{gray, color} {
		for _, order := range []string{"LRCP", "RLCP", "RPCL", "PCRL", "CPRL"} {
			for _, extra := range [][]string{nil, {"-SOP", "-EPH"}, {"-t", "32,24"}} {
				name := fmt.Sprintf("%s-%s-%d", filepath.Base(source), order, len(extra))
				args := append([]string{"-i", source, "-n", "4", "-r", "20,5,1", "-p", order,
					"-c", "[16,16],[16,16],[32,32],[32,32]", "-b", "8,8"}, extra...)
				plain := filepath.Join(dir, name+".j2k")
				tagged := filepath.Join(dir, name+"-plt.j2k")
				run(compress, append(args, "-o", plain)...)
				run(compress, append(args, "-o", tagged, "-PLT")...)
				plainData, _ := os.ReadFile(plain)
				taggedData, _ := os.ReadFile(tagged)
				a, err := IndexCodestream(plainData, IndexLimits{})
				if err != nil || !a.Precincts() {
					t.Fatalf("%s: %v %v", name, err, a.precinctErr)
				}
				b, err := IndexCodestream(taggedData, IndexLimits{})
				if err != nil || !b.Precincts() || !b.usedPLT {
					t.Fatalf("%s with PLT: %v %v (PLT %v)", name, err, b.precinctErr, b.usedPLT)
				}
				equalPackets(t, packetBytes(b), packetBytes(a))

				server, err := NewServer(ServerConfig{Provider: StaticTargets{"t": {Codestreams: [][]byte{taggedData}}}})
				if err != nil {
					t.Fatal(err)
				}
				ts := httptest.NewServer(server)
				for reduce := 0; reduce <= 2; reduce++ {
					fx, fy := b.ReducedSize(reduce)
					_, body := get(t, fmt.Sprintf("%s/?target=t&type=jpp-stream&fsiz=%d,%d", ts.URL, fx, fy))
					rec := rebuild(t, body)
					if got, want := decode(rec.Codestream, reduce, "rebuilt"), decode(taggedData, reduce, "source"); !bytes.Equal(got, want) {
						t.Fatalf("%s reduce %d: opj_decompress output differs", name, reduce)
					}
				}
				ts.Close()
				server.Close()
			}
		}
	}
}
