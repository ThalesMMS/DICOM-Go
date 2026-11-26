//go:build ignore

// Command generate creates the JPEG 2000 Part 1 qualification corpus for the
// optional core backend (#934). Sources are deterministic and synthetic (no
// patient data). OpenJPEG encodes them; OpenJPEG's decoder output is recorded
// as an independent reconstruction, and lossless cases also keep the source
// samples, which are the exact oracle. Raw files are little-endian,
// interleaved frame/row/column/component samples in 8- or 16-bit containers.
//
//	DICOM_GO_OPENJPEG_COMPRESS=opj_compress DICOM_GO_OPENJPEG_DECOMPRESS=opj_decompress go run generate.go
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type spec struct {
	ID         string   `json:"id"`
	Rows       int      `json:"rows"`
	Columns    int      `json:"columns"`
	Components int      `json:"components"`
	Bits       int      `json:"bits"`
	Signed     bool     `json:"signed"`
	Args       []string `json:"args"`
	Lossy      bool     `json:"lossy"`
	// Refusal names the feature the core backend must refuse; such fixtures
	// have no reconstruction.
	Refusal     string `json:"refusal,omitempty"`
	Photometric string `json:"photometric"`
	Pattern     string `json:"pattern"`
}

var specs = []spec{
	{ID: "jpeg2000-core-u8-odd-layers", Rows: 23, Columns: 37, Components: 1, Bits: 8, Args: []string{"-r", "20,10,1", "-n", "3"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-u12-tiles", Rows: 47, Columns: 61, Components: 1, Bits: 12, Args: []string{"-t", "16,16", "-n", "3"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-u16-extremes-rlcp", Rows: 33, Columns: 64, Components: 1, Bits: 16, Args: []string{"-p", "RLCP"}, Photometric: "MONOCHROME2", Pattern: "extremes"},
	{ID: "jpeg2000-core-u16-high-values", Rows: 48, Columns: 48, Components: 1, Bits: 16, Photometric: "MONOCHROME2", Pattern: "high"},
	{ID: "jpeg2000-core-s16-extremes", Rows: 40, Columns: 40, Components: 1, Bits: 16, Signed: true, Photometric: "MONOCHROME2", Pattern: "extremes"},
	{ID: "jpeg2000-core-s12", Rows: 31, Columns: 33, Components: 1, Bits: 12, Signed: true, Args: []string{"-n", "4"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-rgb8-rct", Rows: 31, Columns: 29, Components: 3, Bits: 8, Args: []string{"-mct", "1", "-n", "4"}, Photometric: "YBR_RCT", Pattern: "ramp"},
	{ID: "jpeg2000-core-rgb8-no-mct", Rows: 31, Columns: 29, Components: 3, Bits: 8, Args: []string{"-mct", "0", "-n", "4"}, Photometric: "RGB", Pattern: "ramp"},
	{ID: "jpeg2000-core-u8-irreversible", Rows: 64, Columns: 64, Components: 1, Bits: 8, Args: []string{"-I", "-r", "8"}, Lossy: true, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-u16-irreversible", Rows: 48, Columns: 40, Components: 1, Bits: 16, Args: []string{"-I", "-r", "4"}, Lossy: true, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-rgb8-ict", Rows: 40, Columns: 48, Components: 3, Bits: 8, Args: []string{"-I", "-mct", "1", "-r", "6"}, Lossy: true, Photometric: "YBR_ICT", Pattern: "ramp"},
	// #1031: progressions, precincts, code-block styles and subsampling.
	{ID: "jpeg2000-core-rgb8-rpcl-precincts", Rows: 47, Columns: 61, Components: 3, Bits: 8, Args: []string{"-p", "RPCL", "-c", "[32,32],[16,16]", "-n", "4", "-r", "30,10,1", "-t", "32,24"}, Photometric: "YBR_RCT", Pattern: "ramp"},
	{ID: "jpeg2000-core-u12-pcrl-offsets", Rows: 53, Columns: 71, Components: 1, Bits: 12, Args: []string{"-p", "PCRL", "-c", "[16,32],[16,16],[8,8]", "-n", "3", "-d", "5,3", "-T", "2,1", "-t", "40,40"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-rgb8-cprl", Rows: 50, Columns: 43, Components: 3, Bits: 8, Args: []string{"-p", "CPRL", "-c", "[32,32],[16,16]", "-n", "4", "-r", "20,5,1"}, Photometric: "YBR_RCT", Pattern: "ramp"},
	{ID: "jpeg2000-core-u16-block-styles", Rows: 45, Columns: 52, Components: 1, Bits: 16, Args: []string{"-M", "63", "-r", "20,4,1", "-c", "[32,32],[16,16]"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-s12-bypass-termall", Rows: 40, Columns: 37, Components: 1, Bits: 12, Signed: true, Args: []string{"-M", "5", "-r", "30,10,1", "-n", "4"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-u8-vsc-reset-irreversible", Rows: 48, Columns: 48, Components: 1, Bits: 8, Args: []string{"-I", "-M", "10", "-r", "6"}, Lossy: true, Photometric: "MONOCHROME2", Pattern: "ramp"},
	{ID: "jpeg2000-core-u8-subsampled", Rows: 23, Columns: 37, Components: 1, Bits: 8, Args: []string{"-s", "2,2"}, Photometric: "MONOCHROME2", Pattern: "ramp"},
}

type generation struct {
	OpenJPEG      string `json:"openjpeg"`
	CompressSHA   string `json:"opj_compress_sha256"`
	DecodeSHA     string `json:"opj_decompress_sha256"`
	Fixtures      []spec `json:"fixtures"`
	Deterministic bool   `json:"deterministic"`
}

func main() {
	compress := required("DICOM_GO_OPENJPEG_COMPRESS")
	decompress := required("DICOM_GO_OPENJPEG_DECOMPRESS")
	out, err := os.Getwd()
	check(err)
	temp, err := os.MkdirTemp("", "jpeg2000-core-corpus-*")
	check(err)
	defer os.RemoveAll(temp)
	for _, s := range specs {
		samples := source(s)
		input := filepath.Join(temp, s.ID+ext(s))
		check(os.WriteFile(input, encodeInput(s, samples), 0o600))
		codestream := filepath.Join(out, s.ID+".j2k")
		args := []string{"-i", input, "-o", codestream}
		if s.Components == 1 {
			sign := "u"
			if s.Signed {
				sign = "s"
			}
			args = append(args, "-F", fmt.Sprintf("%d,%d,1,%d,%s", s.Columns, s.Rows, s.Bits, sign))
		}
		run(compress, append(args, s.Args...)...)
		if s.Refusal != "" {
			continue
		}
		decoded := filepath.Join(temp, s.ID+".decoded"+ext(s))
		run(decompress, "-i", codestream, "-o", decoded)
		reconstructed := decodeOutput(s, read(decoded))
		check(os.WriteFile(filepath.Join(out, s.ID+".openjpeg.raw"), raw(s, reconstructed), 0o600))
		if !s.Lossy {
			check(os.WriteFile(filepath.Join(out, s.ID+".source.raw"), raw(s, samples), 0o600))
			if !equal(samples, reconstructed) {
				fmt.Fprintf(os.Stderr, "%s: OpenJPEG lossless reconstruction differs from the source\n", s.ID)
				os.Exit(1)
			}
		}
	}
	g := generation{OpenJPEG: "2.5.4", CompressSHA: sum(compress), DecodeSHA: sum(decompress), Fixtures: specs, Deterministic: true}
	data, err := json.MarshalIndent(g, "", "  ")
	check(err)
	check(os.WriteFile(filepath.Join(out, "generation.json"), append(data, '\n'), 0o600))
}

func ext(s spec) string {
	if s.Components == 3 {
		return ".ppm"
	}
	return ".rawl"
}

// source returns interleaved samples as int32 values in the declared range.
func source(s spec) []int32 {
	n := s.Rows * s.Columns * s.Components
	out := make([]int32, n)
	lo, hi := int64(0), int64(1)<<s.Bits-1
	if s.Signed {
		lo, hi = -(int64(1) << (s.Bits - 1)), int64(1)<<(s.Bits-1)-1
	}
	span := hi - lo + 1
	for y := 0; y < s.Rows; y++ {
		for x := 0; x < s.Columns; x++ {
			for c := 0; c < s.Components; c++ {
				i := (y*s.Columns+x)*s.Components + c
				var v int64
				switch s.Pattern {
				case "extremes":
					switch (x + 3*y + c) % 7 {
					case 0:
						v = lo
					case 1:
						v = hi
					case 2:
						v = lo + 1
					case 3:
						v = hi - 1
					case 4:
						v = (lo + hi) / 2
					default:
						v = lo + (int64(x*7919+y*104729+c*31) % span)
					}
				case "high":
					v = hi - int64((x*37+y*11)%512)
				default:
					v = lo + (int64(x*97+y*211+c*53+x*y*3) % span)
				}
				out[i] = int32(v)
			}
		}
	}
	return out
}

func encodeInput(s spec, samples []int32) []byte {
	var b bytes.Buffer
	if s.Components == 3 {
		fmt.Fprintf(&b, "P6\n%d %d\n%d\n", s.Columns, s.Rows, 1<<s.Bits-1)
		for _, v := range samples {
			b.WriteByte(byte(v))
		}
		return b.Bytes()
	}
	// Raw little-endian samples with -F; OpenJPEG's PGX reader treats a
	// signed 16-bit header as 15 bits.
	for _, v := range samples {
		if s.Bits <= 8 {
			b.WriteByte(byte(v))
		} else {
			_ = binary.Write(&b, binary.LittleEndian, uint16(v))
		}
	}
	return b.Bytes()
}

func decodeOutput(s spec, data []byte) []int32 {
	n := s.Rows * s.Columns * s.Components
	out := make([]int32, n)
	body := data
	if s.Components == 3 {
		// P6, size and maxval lines; OpenJPEG also writes a # comment line.
		header, seen := 0, 0
		for seen < 3 {
			end := bytes.IndexByte(data[header:], '\n')
			if data[header] != '#' {
				seen++
			}
			header += end + 1
		}
		body = data[header:]
	}
	for i := range out {
		var v int32
		if s.Components == 3 || s.Bits <= 8 {
			v = int32(body[i])
		} else {
			v = int32(binary.LittleEndian.Uint16(body[2*i:]))
		}
		// OpenJPEG writes the stored bit pattern; sign-extend from Bits.
		v &= 1<<s.Bits - 1
		if s.Signed && v >= 1<<(s.Bits-1) {
			v -= 1 << s.Bits
		}
		out[i] = v
	}
	return out
}

func raw(s spec, samples []int32) []byte {
	width := 1
	if s.Bits > 8 {
		width = 2
	}
	out := make([]byte, len(samples)*width)
	for i, v := range samples {
		if width == 1 {
			out[i] = byte(v)
		} else {
			binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
		}
	}
	return out
}

func equal(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func required(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", name)
		os.Exit(2)
	}
	return v
}

func run(name string, args ...string) {
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "%s %v: %v\n%s", name, args, err, out)
		os.Exit(1)
	}
}

func read(path string) []byte {
	data, err := os.ReadFile(path)
	check(err)
	return data
}

func sum(program string) string {
	path, err := exec.LookPath(program)
	check(err)
	h := sha256.Sum256(read(path))
	return hex.EncodeToString(h[:])
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
