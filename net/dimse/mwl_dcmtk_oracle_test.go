package dimse

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestDCMTKWorklistOracle is opt-in evidence for #996. DCMTK dump2dcm
// converts the example database, DCMTK's example queries and the additional
// ones (checked against the Go dump reader), DCMTK wlmscpfs answers every
// query through findscu, and the same
// findscu queries against the dicom-go SCP (in-memory handler) must return
// the same responses, which also equal the offline golden file; so must the
// directory handler serving wlmscpfs's own .wl files. With
// DICOMGO_UPDATE_GOLDEN=1 the golden file is rewritten from wlmscpfs.
// Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestDCMTKWorklistOracle(t *testing.T) {
	dump2dcm := dcmtkoracle.Tool(t, "dump2dcm")
	wlmscpfsTool := dcmtkoracle.Tool(t, "wlmscpfs")
	findscu := dcmtkoracle.Tool(t, "findscu")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	work := t.TempDir()
	dbDir := filepath.Join(work, "db", "OFFIS")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dbDir, "lockfile"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	convert := func(dump, out string) *object.Object {
		if output, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, dump2dcm, "+te", dump, out).CombinedOutput(); err != nil {
			t.Fatalf("dump2dcm %s: %v\n%s", dump, err, output)
		}
		file, err := object.OpenFile(out)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = file.Close() }()
		return file.Dataset
	}
	records := dcmtkWorklistRecords(t)
	for i, path := range dcmtkWorklistFiles(t, "wlistdb") {
		converted := convert(path, filepath.Join(dbDir, queryName(path)+".wl"))
		if got, want := worklistFingerprint(records[i]), worklistFingerprint(converted); got != want {
			t.Fatalf("%s: Go dump reader differs from dump2dcm\ngo:\n%s\ndump2dcm:\n%s", path, got, want)
		}
	}
	queries := map[string]string{}
	sources := map[string]string{}
	for _, path := range worklistQueryFiles(t) {
		sources[queryName(path)] = path
		out := filepath.Join(work, queryName(path)+".dcm")
		converted := convert(path, out)
		if got, want := worklistTagPaths(readDCMTKDumpFile(t, path)), worklistTagPaths(converted); got != want {
			t.Fatalf("%s: Go dump reader differs from dump2dcm\ngo:\n%s\ndump2dcm:\n%s", path, got, want)
		}
		queries[queryName(path)] = out
	}

	wlmPort := freePort(t)
	wlmscpfs := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, wlmscpfsTool, "-dfp", filepath.Join(work, "db"), strconv.Itoa(wlmPort))
	if err := wlmscpfs.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wlmscpfs.Process.Kill(); _ = wlmscpfs.Wait() })
	inMemory, err := NewInMemoryModalityWorklistHandlerWithOptions(records, InMemoryModalityWorklistOptions{UniversalQueryMatchesAll: true})
	if err != nil {
		t.Fatal(err)
	}
	goPort := serveWorklistForFindscu(t, ctx, inMemory)
	// The directory handler reads the very .wl files wlmscpfs serves.
	directory, err := NewDirectoryModalityWorklistHandler(filepath.Join(work, "db"), DirectoryModalityWorklistOptions{
		UniversalQueryMatchesAll: true,
		OnFileError:              func(path string, err error) { t.Errorf("directory handler skipped %s: %v", path, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	dirPort := serveWorklistForFindscu(t, ctx, directory)
	time.Sleep(300 * time.Millisecond)

	golden := map[string][]string{}
	names := make([]string, 0, len(queries))
	for name := range queries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fromDCMTK := findscuResponses(t, ctx, findscu, wlmPort, queries[name], filepath.Join(work, "wlm-"+name))
		fromGo := findscuResponses(t, ctx, findscu, goPort, queries[name], filepath.Join(work, "go-"+name))
		fromDirectory := findscuResponses(t, ctx, findscu, dirPort, queries[name], filepath.Join(work, "dir-"+name))
		inProcess := ourWorklistResponses(t, records, readDCMTKDumpFile(t, sources[name]))
		joined := func(values []string) string { return strings.Join(values, "\n--\n") }
		if joined(fromGo) != joined(fromDCMTK) || joined(inProcess) != joined(fromDCMTK) || joined(fromDirectory) != joined(fromDCMTK) {
			t.Errorf("%s: responses differ\nwlmscpfs (%d):\n%s\ndicom-go SCP (%d):\n%s\nin-process (%d):\n%s\ndirectory SCP (%d):\n%s",
				name, len(fromDCMTK), joined(fromDCMTK), len(fromGo), joined(fromGo), len(inProcess), joined(inProcess), len(fromDirectory), joined(fromDirectory))
		}
		t.Logf("%s: %d responses", name, len(fromDCMTK))
		golden[name] = fromDCMTK
	}
	if os.Getenv("DICOMGO_UPDATE_GOLDEN") == "1" {
		data, err := json.MarshalIndent(golden, "", " ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dcmtkWorklistGolden, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// worklistTagPaths lists every attribute path, including empty ones.
func worklistTagPaths(dataset *object.Object) string {
	var lines []string
	var walk func(prefix string, elements []core.Element)
	walk = func(prefix string, elements []core.Element) {
		for _, element := range elements {
			tag := element.Tag()
			if tag.Element == 0 {
				continue
			}
			path := prefix + tag.String()
			lines = append(lines, path)
			if sequence, ok := element.Value.(core.SequenceValue); ok {
				for i, item := range sequence.Items {
					walk(path+"["+strconv.Itoa(i)+"]", item.Elements)
				}
			}
		}
	}
	walk("", dataset.Elements())
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// findscuResponses runs findscu -W (a path from dcmtkoracle.Tool) with a
// query file and returns the sorted fingerprints of the responses it
// extracted.
func findscuResponses(t *testing.T, ctx context.Context, findscu string, port int, query, outDir string) []string {
	t.Helper()
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	output, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, findscu, "-v", "-W", "-aec", "OFFIS", "-X", "-od", outDir, "127.0.0.1", strconv.Itoa(port), query).CombinedOutput()
	if err != nil {
		t.Fatalf("findscu: %v\n%s", err, output)
	}
	paths, _ := filepath.Glob(filepath.Join(outDir, "rsp*.dcm"))
	responses := make([]string, 0, len(paths))
	for _, path := range paths {
		file, err := object.OpenFile(path)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, worklistFingerprint(file.Dataset))
		_ = file.Close()
	}
	sort.Strings(responses)
	return responses
}

// serveWorklistForFindscu serves Modality Worklist C-FIND associations to
// the called AE Title OFFIS with handler until the test ends.
func serveWorklistForFindscu(t *testing.T, ctx context.Context, handler ModalityWorklistHandler) int {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
				AETitle: "OFFIS", Context: ctx,
				SupportedAbstractSyntaxes: []string{ModalityWorklistFindSOPClassUID},
				SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID},
			})
			if err != nil {
				if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
					return
				}
				continue
			}
			pc, err := AcceptedContextForSOPClass(assoc, ModalityWorklistFindSOPClassUID)
			if err == nil {
				err = ServeModalityWorklistCFindWithOptions(ctx, assoc, pc.ID, handler, ModalityWorklistSCPOptions{Parse: dcmtkWorklistParseOptions, AllowUniversalQuery: true})
			}
			if err == nil {
				err = respondToStoreRelease(assoc)
			}
			if err != nil {
				t.Logf("worklist SCP: %v", err)
			}
			_ = assoc.Close()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}
