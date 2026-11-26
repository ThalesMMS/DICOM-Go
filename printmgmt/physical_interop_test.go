package printmgmt_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

type physicalPeer struct {
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *strings.Builder
}

func startPhysicalPeer(t *testing.T, ctx context.Context, mode string) physicalPeer {
	t.Helper()
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in pinned software print peer")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	c := exec.CommandContext(ctx, python, "-u", "testdata/physical_print_peer.py", mode)
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
func (p physicalPeer) line(t *testing.T) string {
	t.Helper()
	if !p.out.Scan() {
		t.Fatalf("physical peer ended: %s", p.stderr.String())
	}
	return p.out.Text()
}
func TestPhysicalClientAgainstIndependentPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	peer := startPhysicalPeer(t, ctx, "scp")
	address := "127.0.0.1:" + peer.line(t)
	assoc, client := dialPrint(t, ctx, address, false)
	img := gray8(4, 10, 0)
	policies := []printmgmt.DecimateCropBehavior{printmgmt.Fail, printmgmt.Crop, printmgmt.Decimate}
	statuses := []int{0xC603, 0xB609, 0xB60A}
	for index, policy := range policies {
		result, err := client.PrintFilm(ctx, printmgmt.Film{Columns: 1, Rows: 1, Images: []*printmgmt.Image{img}, ImagePresentations: []printmgmt.ImageBoxPresentation{{RequestedWidthMM: 200, DecimateCropBehavior: policy}}, RequireImagePresentation: true})
		if policy == printmgmt.Fail {
			var e *printmgmt.PrintError
			if !errors.As(err, &e) || e.Status != 0xC603 || result.Printed {
				t.Fatal("peer refusal", err)
			}
		} else if err != nil || !result.Printed || len(result.Warnings) != 1 || int(result.Warnings[0].Status) != statuses[index] {
			t.Fatal("peer warning not preserved", result, err)
		}
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(peer.in, "DONE")
	var records []struct {
		Op         string  `json:"op"`
		Width      float64 `json:"width"`
		Behavior   string  `json:"behavior"`
		WidthVR    string  `json:"width_vr"`
		BehaviorVR string  `json:"behavior_vr"`
		SHA        string  `json:"sha256"`
		Status     int     `json:"status"`
	}
	if err := json.Unmarshal([]byte(peer.line(t)), &records); err != nil {
		t.Fatal(err)
	}
	sets, actions := 0, 0
	for _, r := range records {
		if r.Op == "N-ACTION" {
			actions++
		}
		if r.Op == "N-SET" {
			sets++
			if sets > len(policies) || r.Behavior != string(policies[sets-1]) || r.Status != statuses[sets-1] || r.Width != 200 || r.WidthVR != "DS" || r.BehaviorVR != "CS" || r.SHA != sum(img.PixelData) {
				t.Fatal("independent encoding mismatch", r)
			}
		}
	}
	if sets != 3 || actions != 2 {
		t.Fatal("failed request printed", sets, actions)
	}
}
func TestPhysicalIndependentSCUAgainstLibrarySCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	peer := startPhysicalPeer(t, ctx, "scu")
	backend := syntheticPhysicalBackend{printmgmt.NewMemoryBackend(8)}
	address := startPhysicalSCP(t, ctx, backend, physicalOptions())
	fmt.Fprintln(peer.in, address[strings.LastIndex(address, ":")+1:])
	var report struct {
		Statuses []int  `json:"statuses"`
		SHA      string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(peer.line(t)), &report); err != nil {
		t.Fatal(err)
	}
	jobs := backend.Jobs()
	if len(jobs) != 4 || len(report.Statuses) != 4 {
		t.Fatal("missing software jobs", len(jobs), report)
	}
	if !reflect.DeepEqual(report.Statuses, []int{0, 0xC603, 0xB609, 0xB60A}) {
		t.Fatal("independent SCU status mismatch", report.Statuses)
	}
	for i, j := range jobs {
		box := j.FilmBoxes[0].ImageBoxes[0]
		if !box.Plan.Resolved || sum(box.Image.PixelData) != report.SHA {
			t.Fatal("plan/pixels mismatch", i)
		}
		if i < 2 && box.Presentation.RequestedWidthMM != 100 {
			t.Fatal("refused request replaced previous image", box)
		}
		if i == 2 && box.Plan.Outcome != printmgmt.Crop {
			t.Fatal("crop not preserved")
		}
		if i == 3 && box.Plan.Outcome != printmgmt.Decimate {
			t.Fatal("decimation not preserved")
		}
	}
}
