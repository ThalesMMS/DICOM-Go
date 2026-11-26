package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestArchiveEncapsulatedIndependentPeers is the opt-in independent gate for
// #939. DCMTK storescu sends each compressed file with only its own syntax
// (-R); dcmdump parses every archived file with default strict error handling;
// DCMTK getscu and movescu->storescp retrieve them. dicom-go then verifies all
// fragments, trailing attributes and decodable samples of the DCMTK-written
// files, and pinned pydicom (when DICOMGO_PYTHON is set) checks syntax,
// sentinels and per-frame codestream hashes of every copy. Without the gate
// (DICOMGO_DCMTK_INTEGRATION=1) the test skips and qualifies nothing.
func TestArchiveEncapsulatedIndependentPeers(t *testing.T) {
	storescu := dcmtkoracle.Tool(t, "storescu")
	getscu := dcmtkoracle.Tool(t, "getscu")
	movescu := dcmtkoracle.Tool(t, "movescu")
	storescpTool := dcmtkoracle.Tool(t, "storescp")
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	moveDir := t.TempDir()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	movePort := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	storescp := dcmtkoracle.Command(ctx, t, dcmtkoracle.Protocol, storescpTool, "-aet", "MOVEDEST", "+xa", "-od", moveDir, strconv.Itoa(movePort))
	var storescpOutput bytes.Buffer
	storescp.Stdout, storescp.Stderr = &storescpOutput, &storescpOutput
	if err := storescp.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storescp.Process.Kill(); _ = storescp.Wait() })

	running := startEncapsulatedArchive(t, ctx, map[string]string{"MOVEDEST": net.JoinHostPort("127.0.0.1", strconv.Itoa(movePort))})
	host, port, err := net.SplitHostPort(running.address)
	if err != nil {
		t.Fatal(err)
	}
	output := func(cmd *exec.Cmd) string {
		t.Helper()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(cmd.Args, " "), err, out)
		}
		return string(out)
	}
	run := func(tolerance, tool string, args ...string) string {
		t.Helper()
		return output(dcmtkoracle.Command(ctx, t, tolerance, tool, args...))
	}
	const strictParse = "dcmdump parses with strict error handling and reaches the attribute after Pixel Data"

	cases := encapsulatedCases()
	instances := make([]encapsulatedInstance, len(cases))
	sourceDir := t.TempDir()
	for i, c := range cases {
		instances[i] = buildEncapsulatedInstance(t, c, 60+i)
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: instances[i].dataset, TransferSyntax: instances[i].syntax}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sourceDir, c.name+".dcm")
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		flag, ok := dcmtkSyntaxFlag[instances[i].syntax.UID]
		if !ok {
			t.Fatalf("no DCMTK proposal flag for %s", instances[i].syntax.UID)
		}
		run(dcmtkoracle.Protocol, storescu, "-R", "-"+flag, "-aet", "DCMTKSCU", "-aec", "REFERENCEARCHIVE", host, port, path)
		archived := running.storedPath(t, instances[i].uid)
		if err := verifyEncapsulatedFile(archived, instances[i], c.decode); err != nil {
			t.Fatalf("%s archived from DCMTK: %v", c.name, err)
		}
		if out := run(strictParse, dcmdump, "+P", "7fe1,1001", archived); !strings.Contains(out, instances[i].sentinel) {
			t.Fatalf("%s: dcmdump did not reach the attribute after Pixel Data:\n%s", c.name, out)
		}
	}

	// getscu negotiates one preferred storage syntax per run, so each
	// instance is retrieved at IMAGE level with its own syntax.
	getDir := t.TempDir()
	study := instances[0].study
	for _, instance := range instances {
		keys := []string{"-k", "0008,0052=IMAGE", "-k", "0020,000D=" + study, "-k", "0020,000E=" + study + ".1", "-k", "0008,0018=" + instance.uid}
		run(dcmtkoracle.Protocol, getscu, append(append([]string{"-S", "+" + dcmtkSyntaxFlag[instance.syntax.UID], "-aet", "DCMTKGET", "-aec", "REFERENCEARCHIVE", "-od", getDir}, keys...), host, port)...)
	}
	keys := []string{"-k", "0008,0052=STUDY", "-k", "0020,000D=" + study}
	run(dcmtkoracle.Protocol, movescu, append(append([]string{"-S", "-aet", "DCMTKMOVE", "-aec", "REFERENCEARCHIVE", "-aem", "MOVEDEST"}, keys...), host, port)...)

	byUID := func(dir string) map[string]string {
		files := map[string]string{}
		deadline := time.Now().Add(5 * time.Second)
		for {
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				path := filepath.Join(dir, entry.Name())
				file, err := object.OpenFile(path)
				if err != nil {
					continue
				}
				if uid, ok := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018)); ok {
					files[uid] = path
				}
				_ = file.Close()
			}
			if len(files) >= len(instances) || time.Now().After(deadline) {
				return files
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	type pydicomExpectation struct {
		Path     string   `json:"path"`
		Syntax   string   `json:"syntax"`
		Sentinel string   `json:"sentinel"`
		Frames   []string `json:"frames"`
	}
	var expectations []pydicomExpectation
	for _, dir := range []string{getDir, moveDir} {
		files := byUID(dir)
		for i, c := range cases {
			path, ok := files[instances[i].uid]
			if !ok {
				t.Fatalf("%s missing from %s", c.name, dir)
			}
			if err := verifyEncapsulatedFile(path, instances[i], c.decode); err != nil {
				t.Fatalf("%s written by DCMTK: %v", c.name, err)
			}
			run(dcmtkoracle.Accepted, dcmdump, "-q", path)
			expectations = append(expectations, pydicomExpectation{Path: path, Syntax: instances[i].syntax.UID, Sentinel: instances[i].sentinel, Frames: frameHashes(instances[i], c)})
		}
	}
	for i, c := range cases {
		expectations = append(expectations, pydicomExpectation{Path: running.storedPath(t, instances[i].uid), Syntax: instances[i].syntax.UID, Sentinel: instances[i].sentinel, Frames: frameHashes(instances[i], c)})
	}

	python := strings.TrimSpace(os.Getenv("DICOMGO_PYTHON"))
	if python == "" {
		t.Log("DICOMGO_PYTHON unset: pydicom portion not executed")
		return
	}
	manifest := filepath.Join(t.TempDir(), "expectations.json")
	data, err := json.Marshal(expectations)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, data, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "encapsulated_oracle.py")
	if err := os.WriteFile(script, []byte(encapsulatedPydicomScript), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Log(output(exec.CommandContext(ctx, python, script, manifest)))
}

// dcmtkSyntaxFlag is the DCMTK option suffix proposing (storescu -x…) or
// preferring (getscu +x…) each fixture syntax.
var dcmtkSyntaxFlag = map[string]string{
	transfer.RLELossless.UID:          "xr",
	transfer.JPEGLSLossless.UID:       "xt",
	transfer.JPEGLosslessSV1.UID:      "xs",
	transfer.JPEG2000LosslessOnly.UID: "xv",
}

// frameHashes are the per-frame codestreams: one Item per frame in the
// fixtures, two Items per frame in the refragmented case.
func frameHashes(instance encapsulatedInstance, c encapsulatedCase) []string {
	var frames [][]byte
	if c.refragment {
		for i := 0; i+1 < len(instance.pixel.Fragments); i += 2 {
			frames = append(frames, append(append([]byte(nil), instance.pixel.Fragments[i]...), instance.pixel.Fragments[i+1]...))
		}
	} else {
		frames = instance.pixel.Fragments
	}
	out := make([]string, len(frames))
	for i, frame := range frames {
		sum := sha256.Sum256(frame)
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}

const encapsulatedPydicomScript = `
import hashlib
import json
import sys

import pydicom
from pydicom.encaps import generate_frames

assert pydicom.__version__ == "3.0.2", pydicom.__version__
checked = 0
for item in json.load(open(sys.argv[1])):
    ds = pydicom.dcmread(item["path"])
    assert ds.file_meta.TransferSyntaxUID == item["syntax"], item["path"]
    assert ds[0x7FE11001].value == item["sentinel"], item["path"]
    elem = ds["PixelData"]
    assert elem.is_undefined_length, item["path"]
    frames = list(generate_frames(ds.PixelData, number_of_frames=int(ds.get("NumberOfFrames", 1))))
    got = [hashlib.sha256(f).hexdigest() for f in frames]
    assert got == item["frames"], (item["path"], len(got), len(item["frames"]))
    checked += 1
print("pydicom encapsulated oracle: %d files" % checked)
`
