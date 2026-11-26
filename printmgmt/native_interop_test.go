package printmgmt_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func nativeDCMTKConfig(t *testing.T, port string, local, annotation bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "database")
	spool := filepath.Join(dir, "spool")
	logs := filepath.Join(dir, "logs")
	for _, path := range []string{db, spool, logs} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	typ := "PRINTER"
	if local {
		typ = "LOCALPRINTER"
	}
	text := fmt.Sprintf(`[[GENERAL]]
[DATABASE]
Directory = %s
[PRINT]
Directory = %s
MinPrintResolution = 4\4
MaxPrintResolution = 64\64
DefaultIllumination = 2000
DefaultReflection = 10
[APPLICATION]
LogDirectory = %s
[NETWORK]
Aetitle = NATIVEPEER
[[COMMUNICATION]]
[TEST]
Type = %s
Hostname = localhost
Port = %s
Aetitle = PRINTER
SupportsPresentationLUT = true
PresentationLUTinFilmSession = false
PresentationLUTMatchRequired = false
Supports12Bit = true
DisplayFormat = 1,1\2,1
FilmSizeID = 8INX10IN
MediumType = PAPER
FilmDestination = MAGAZINE
MagnificationType = REPLICATE\BILINEAR\NONE
MinDensity = 0\20
MaxDensity = 320
`, db, spool, logs, typ, port)
	if annotation {
		text += "Annotation = 2\\SYNTHETIC\n"
	}
	path := filepath.Join(dir, "printer.cfg")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path, db
}
func nativeDCMTKCommand(t *testing.T, ctx context.Context, tool string, args ...string) string {
	t.Helper()
	c := exec.CommandContext(ctx, tool, args...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s exit: %v\n%s", filepath.Base(tool), err, out)
	}
	t.Logf("%s: %s", filepath.Base(tool), out)
	return strings.TrimSpace(string(out))
}
func TestNativeDCMTKSpoolerAgainstLibrarySCP(t *testing.T) {
	spooler := dcmtkoracle.Tool(t, "dcmprscu")
	renderer := dcmtkoracle.Tool(t, "dcmpsprt")
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("pinned Python fixture writer required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(4)}
	address := startNativeSCP(t, ctx, backend, nativeSCPOptions(), nil)
	config, db := nativeDCMTKConfig(t, address[strings.LastIndex(address, ":")+1:], false, true)
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	imagePath := filepath.Join(t.TempDir(), "synthetic.dcm")
	nativeDCMTKCommand(t, ctx, python, "testdata/native_print_peer.py", "write-image", imagePath)
	nativeDCMTKCommand(t, ctx, renderer, "--verbose", "--config", config, "--printer", "TEST", "--identity", "--annotation", "SYNTHETIC1462", "--print-no-date", "--print-no-name", "--print-no-lighting", imagePath)
	stored := nativeDCMTKCommand(t, ctx, python, "testdata/native_print_peer.py", "find-stored-print", db)
	nativeDCMTKCommand(t, ctx, spooler, "--verbose", "--config", config, "--printer", "TEST", stored)
	jobs := backend.Jobs()
	if len(jobs) != 1 || len(jobs[0].PresentationLUTs) != 1 || jobs[0].PresentationLUTs[0].LUT.Shape != "IDENTITY" || len(jobs[0].FilmBoxes[0].AnnotationBoxes) != 2 || jobs[0].FilmBoxes[0].AnnotationBoxes[0].Text != "SYNTHETIC1462" || jobs[0].FilmBoxes[0].AnnotationBoxes[0].Position != 2 || jobs[0].FilmBoxes[0].ImageBoxes[0].Image.Bits != 12 {
		t.Fatalf("DCMTK native composition missing %+v", jobs)
	}
}
func TestNativeClientAgainstDCMTKPrintSCP(t *testing.T) {
	tool := dcmtkoracle.Tool(t, "dcmprscp")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	port := address[strings.LastIndex(address, ":")+1:]
	config, _ := nativeDCMTKConfig(t, port, true, false)
	logFile, err := os.Create(filepath.Join(t.TempDir(), "printscp.log"))
	if err != nil {
		t.Fatal(err)
	}
	c := exec.CommandContext(ctx, tool, "--verbose", "--config", config, "--printer", "TEST")
	c.Stdout, c.Stderr = logFile, logFile
	if err = c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait(); _ = logFile.Close() })
	var assoc *ul.Association
	for attempt := 0; attempt < 50; attempt++ {
		assoc, err = ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "NATIVEPEER", CalledAETitle: "PRINTER", Contexts: printmgmt.NativePresentationContexts(false, true, false)})
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err != nil {
		t.Fatal("print SCP did not become ready", err)
	}
	defer assoc.Close()
	client := printmgmt.NewClient(assoc, false)
	for _, p := range []printmgmt.PresentationLUT{{Shape: "IDENTITY"}, {Shape: "LIN OD"}, tableNative(8), tableNative(12)} {
		img := gray8(4, 10, 0)
		if p.InputBits == 12 {
			img = gray12(4, 10)
		}
		r, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Presentation: printmgmt.FilmBoxPresentation{Illumination: 2000, ReflectedAmbientLight: 10, MinDensity: 20, MaxDensity: 320}, Images: []*printmgmt.Image{img}, Native: printmgmt.NativeFilmOptions{PresentationLUT: &p}})
		if err != nil || !r.Printed || r.CleanupErr != nil {
			t.Fatalf("DCMTK accepted film %+v %v", r, err)
		}
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func startNativePeer(t *testing.T, ctx context.Context, mode string) physicalPeer {
	t.Helper()
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in pinned independent native print peer")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	c := exec.CommandContext(ctx, python, "-u", "testdata/native_print_peer.py", mode)
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &strings.Builder{}
	c.Stderr = stderr
	if err = c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return physicalPeer{in, bufio.NewScanner(out), stderr}
}

type nativeRecord struct {
	Op           string `json:"op"`
	Class        string `json:"class"`
	Context      string `json:"context"`
	Status       int    `json:"status"`
	Descriptor   []int  `json:"descriptor"`
	DescriptorVR string `json:"descriptor_vr"`
	DataVR       string `json:"data_vr"`
	LUTSHA       string `json:"lut_sha"`
	Charset      string `json:"charset"`
	Format       string `json:"format"`
	Position     int    `json:"position"`
	Text         string `json:"text"`
	PositionVR   string `json:"position_vr"`
	TextVR       string `json:"text_vr"`
	SHA          string `json:"sha"`
}

func nativeTableSHA(p printmgmt.PresentationLUT) string {
	raw := make([]byte, 2*len(p.Entries))
	for i, v := range p.Entries {
		binary.LittleEndian.PutUint16(raw[2*i:], v)
	}
	return sum(raw)
}
func TestNativeClientAgainstIndependentPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	peer := startNativePeer(t, ctx, "scp")
	a, c := dialNative(t, ctx, "127.0.0.1:"+peer.line(t), false, true, true)
	img := gray8(4, 10, 0)
	p := tableNative(8)
	r, err := c.PrintFilm(ctx, printmgmt.Film{Session: printmgmt.FilmSessionPresentation{SpecificCharacterSet: "ISO_IR 192"}, Columns: 1, Rows: 1, Images: []*printmgmt.Image{img}, Native: printmgmt.NativeFilmOptions{PresentationLUT: &p, AnnotationDisplayFormatID: "SYNTHETIC", Annotations: []printmgmt.Annotation{{Position: 2, Text: "SYNTHÉTIQUE"}}}})
	if err != nil || !r.Printed || r.CleanupErr != nil {
		t.Fatalf("native peer %+v %v", r, err)
	}
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(peer.in, "DONE")
	var records []nativeRecord
	if err := json.Unmarshal([]byte(peer.line(t)), &records); err != nil {
		t.Fatal(err)
	}
	want := []string{"N-CREATE " + printmgmt.PresentationLUTSOPClassUID, "N-CREATE " + printmgmt.FilmSessionSOPClassUID, "N-CREATE " + printmgmt.FilmBoxSOPClassUID, "N-SET " + printmgmt.AnnotationBoxSOPClassUID, "N-SET " + printmgmt.GrayscaleImageBoxUID, "N-ACTION " + printmgmt.FilmBoxSOPClassUID, "N-DELETE " + printmgmt.FilmSessionSOPClassUID, "N-DELETE " + printmgmt.PresentationLUTSOPClassUID}
	var got []string
	for _, r := range records {
		got = append(got, r.Op+" "+r.Class)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operation order %v", got)
	}
	if records[0].Context != printmgmt.PresentationLUTSOPClassUID || !reflect.DeepEqual(records[0].Descriptor, []int{256, 0, 12}) || records[0].DescriptorVR != "US" || records[0].DataVR != "OW" || records[0].LUTSHA != nativeTableSHA(p) || records[1].Charset != "ISO_IR 192" || records[2].Format != "SYNTHETIC" || records[3].Context != printmgmt.AnnotationBoxSOPClassUID || records[3].Position != 2 || records[3].Text != "SYNTHÉTIQUE" || records[3].PositionVR != "US" || records[3].TextVR != "LO" || records[4].SHA != sum(img.PixelData) {
		t.Fatal("independent values/context mismatch", records)
	}
}
func TestNativeRequiredIgnoredAndRejectedPeerStopsAction(t *testing.T) {
	for _, mode := range []string{"scp-ignore-lut", "scp-ignore-session", "scp-ignore-film", "scp-ignore-annotation", "scp-reject-context"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			peer := startNativePeer(t, ctx, mode)
			a, c := dialNative(t, ctx, "127.0.0.1:"+peer.line(t), false, true, true)
			r, err := c.PrintFilm(ctx, printmgmt.Film{Session: printmgmt.FilmSessionPresentation{SpecificCharacterSet: "ISO_IR 192"}, Columns: 1, Rows: 1, Images: []*printmgmt.Image{gray8(2, 2, 0)}, Native: printmgmt.NativeFilmOptions{PresentationLUT: &printmgmt.PresentationLUT{Shape: "IDENTITY"}, AnnotationDisplayFormatID: "SYNTHETIC", Annotations: []printmgmt.Annotation{{Position: 1, Text: "SYNTHETIC"}}}})
			var e *printmgmt.PrintError
			if err == nil || r.Printed {
				t.Fatalf("refused native request printed %+v %v", r, err)
			}
			if mode == "scp-reject-context" {
				if !errors.Is(err, printmgmt.ErrNativeUnsupported) {
					t.Fatal(err)
				}
			} else if !errors.As(err, &e) || e.Status != 0x0107 {
				t.Fatal("ignored exact status missing", err)
			}
			_ = a.Release(ctx)
			fmt.Fprintln(peer.in, "DONE")
			var records []nativeRecord
			if err := json.Unmarshal([]byte(peer.line(t)), &records); err != nil {
				t.Fatal(err)
			}
			for _, rec := range records {
				if rec.Op == "N-ACTION" {
					t.Fatal("required refusal reached action", records)
				}
			}
			if mode == "scp-reject-context" && len(records) != 0 {
				t.Fatal("rejected context still created hierarchy", records)
			}
		})
	}
}
func TestNativeIndependentSCUAgainstLibrarySCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	peer := startNativePeer(t, ctx, "scu")
	backend := syntheticNativeBackend{printmgmt.NewMemoryBackend(4)}
	address := startNativeSCP(t, ctx, backend, nativeSCPOptions(), nil)
	fmt.Fprintln(peer.in, address[strings.LastIndex(address, ":")+1:])
	var report struct {
		Statuses []int    `json:"statuses"`
		PixelSHA []string `json:"pixel_sha"`
		LUTSHA   []string `json:"lut_sha"`
	}
	if err := json.Unmarshal([]byte(peer.line(t)), &report); err != nil {
		t.Fatal(err)
	}
	want := []int{0, 0, 0x0111, 0x0106, 0, 0x0107, 0, 0, 0x0106, 0, 0, 0x0106, 0x0110, 0x0106, 0, 0, 0, 0, 0x0112}
	if !reflect.DeepEqual(report.Statuses, want) {
		t.Fatal("exact independent statuses", report.Statuses)
	}
	jobs := backend.Jobs()
	if len(jobs) != 1 || len(jobs[0].PresentationLUTs) != 2 {
		t.Fatal("wrong jobs", jobs)
	}
	f := jobs[0].FilmBoxes[0]
	if f.AnnotationBoxes[1].Text != "SYNTHÉTIQUE" || !f.Presentation.MinDensitySet || f.Presentation.MinDensity != 0 || !f.Presentation.ReflectedAmbientLightSet || f.ImageBoxes[0].Image.Bits != 8 || f.ImageBoxes[1].Image.Bits != 12 {
		t.Fatal("refusal/zero/native data lost", f)
	}
	for i, b := range f.ImageBoxes {
		if sum(b.Image.PixelData) != report.PixelSHA[i] {
			t.Fatal("peer pixels mutated")
		}
	}
	for _, p := range jobs[0].PresentationLUTs {
		index := 0
		if p.LUT.InputBits == 12 {
			index = 1
		}
		if nativeTableSHA(p.LUT) != report.LUTSHA[index] {
			t.Fatal("peer LUT words changed")
		}
	}
}
