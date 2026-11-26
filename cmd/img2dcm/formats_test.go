package main

import (
	"bytes"
	"github.com/ThalesMMS/dicom-go/core"
	"os"
	"path/filepath"
	"testing"
)

func TestImg2DCMPortableRawAndMultiFrameSC(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		args []string
		want []byte
	}{
		{"pnm", []byte("P5 2 1 255\n\x09\xff"), nil, []byte{9, 255}},
		{"pgx", []byte("PG ML + 16 2 1\n\x12\x34\x00\x09"), nil, []byte{0x34, 0x12, 9, 0}},
		{"raw", []byte{7, 0, 9, 0, 255}, []string{"-format", "raw", "-rows", "1", "-columns", "2", "-bits-allocated", "16", "-bits-stored", "8", "-endian", "big", "-offset", "1"}, []byte{9, 0, 255, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := writeInput(t, "in."+tc.name, tc.data)
			output := filepath.Join(t.TempDir(), "out.dcm")
			var out, diag bytes.Buffer
			args := append(append([]string(nil), tc.args...), input, output)
			if code := run(args, &out, &diag); code != 0 {
				t.Fatalf("exit %d: %s", code, diag.String())
			}
			f := cliRead(t, output)
			raw, _ := f.Dataset.GetRaw(core.NewTag(0x7fe0, 0x0010))
			if !bytes.Equal(raw, tc.want) {
				t.Fatalf("raw %x want %x", raw, tc.want)
			}
		})
	}
	a := writeInput(t, "a.png", cliPNG(t))
	b := writeInput(t, "b.png", cliPNG(t))
	output := filepath.Join(t.TempDir(), "multi.dcm")
	var out, diag bytes.Buffer
	if code := run([]string{a, b, output}, &out, &diag); code != 0 {
		t.Fatalf("multi: %s", diag.String())
	}
	f := cliRead(t, output)
	frames, _ := f.Dataset.GetString(core.NewTag(0x0028, 0x0008))
	class, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0016))
	if frames != "2" || class != "1.2.840.10008.5.1.4.1.1.7.2" {
		t.Fatal("wrong Multi-frame SC")
	}
}
func TestImg2DCMInputCapBeforePublication(t *testing.T) {
	input := writeInput(t, "small.png", cliPNG(t))
	output := filepath.Join(t.TempDir(), "no-output.dcm")
	var out, diag bytes.Buffer
	if run([]string{"-max-input-bytes", "1", input, output}, &out, &diag) != 1 {
		t.Fatal("byte cap ignored")
	}
	if _, e := os.Stat(output); !os.IsNotExist(e) {
		t.Fatal("cap failure published")
	}
}
