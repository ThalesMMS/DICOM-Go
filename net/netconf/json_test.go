package netconf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) *Device {
	t.Helper()
	d, err := ReadFile(context.Background(), "testdata/two-aes.json", Options{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/two-aes.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestJSONRoundTripAndExactAdmission(t *testing.T) {
	d := fixture(t)
	d.DeviceName = "é<>&\u2028\""
	d.Description = "Patient-free synthetic catalog"
	yes := true
	d.Installed = &yes
	d.Connections[0].Installed = &yes
	raw, err := Marshal(context.Background(), d, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) != jsonDeviceSize(d) {
		t.Fatalf("encoded size %d != admitted %d", len(raw), jsonDeviceSize(d))
	}
	again, err := Parse(context.Background(), bytes.NewReader(append([]byte(" \n"), raw...)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Marshal(context.Background(), again, Options{})
	if err != nil || !bytes.Equal(raw, second) {
		t.Fatalf("nondeterministic round trip: %v", err)
	}
	if _, err = Marshal(context.Background(), d, Options{Limits: Limits{MaxJSONBytes: int64(len(raw) - 1)}}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("size limit: %v", err)
	}
}
func TestJSONSchemaAndAdmissionFailures(t *testing.T) {
	base := string(fixtureBytes(t))
	for _, tc := range []struct {
		name, raw string
		options   Options
		want      error
	}{
		{"duplicate", strings.Replace(base, "\"schemaVersion\":1", "\"schemaVersion\":1,\"schemaVersion\":1", 1), Options{}, ErrInvalid},
		{"unknown", strings.Replace(base, "\"schemaVersion\":1", "\"secret\":\"do-not-report\",\"schemaVersion\":1", 1), Options{}, ErrInvalid},
		{"version", strings.Replace(base, "\"schemaVersion\":1", "\"schemaVersion\":2", 1), Options{}, ErrInvalid},
		{"null installed", strings.Replace(base, "\"schemaVersion\":1", "\"installed\":null,\"schemaVersion\":1", 1), Options{}, ErrInvalid},
		{"boolean", strings.Replace(base, "\"tlsRequired\":false", "\"tlsRequired\":0", 1), Options{}, ErrInvalid},
		{"non-JSON integer", strings.Replace(base, "\"port\":0", "\"port\":+0", 1), Options{}, ErrInvalid},
		{"bytes", base, Options{Limits: Limits{MaxJSONBytes: 20}}, ErrResourceLimit},
		{"ae count", base, Options{Limits: Limits{MaxAEs: 1}}, ErrResourceLimit},
		{"cumulative caps", base, Options{Limits: Limits{MaxCapabilities: 3}}, ErrResourceLimit},
		{"cumulative TS", base, Options{Limits: Limits{MaxTransferSyntaxes: 3}}, ErrResourceLimit},
		{"depth", base, Options{Limits: Limits{MaxDepth: 3}}, ErrResourceLimit},
		{"backing", base, Options{Limits: Limits{MaxCompiledBytes: 500}}, ErrResourceLimit},
		{"scalar", base, Options{Limits: Limits{MaxScalarBytes: 10}}, ErrResourceLimit},
		{"trailing", base + " {}", Options{}, ErrInvalid},
		{"invalid UTF8", strings.Replace(base, "Synthetic", string([]byte{0xff}), 1), Options{}, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Parse(context.Background(), strings.NewReader(tc.raw), tc.options)
			if d != nil || !errors.Is(err, tc.want) {
				t.Fatalf("result %v/%v", d, err)
			}
			if strings.Contains(err.Error(), "do-not-report") {
				t.Fatal("configuration value leaked")
			}
		})
	}
}

type forbiddenReader struct{ reads int }

func (r *forbiddenReader) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

type failedWriter struct{}

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("secret writer error") }
func TestJSONCancellationAndSanitizedIO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &forbiddenReader{}
	if _, err := Parse(ctx, r, Options{}); !errors.Is(err, context.Canceled) || r.reads != 0 {
		t.Fatalf("read after cancel: %v/%d", err, r.reads)
	}
	if _, err := ReadFile(context.Background(), "/not-present/secret-network-config.json", Options{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("path error: %v", err)
	}
	if err := Write(context.Background(), failedWriter{}, fixture(t), Options{}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("writer error: %v", err)
	}
}

type observingWriter struct{ writes int }

func (w *observingWriter) Write(p []byte) (int, error) { w.writes++; return len(p), nil }
func TestSerializationAdmissionBeforeWriter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limits      Limits
		description string
	}{{"depth", Limits{MaxDepth: 3}, ""}, {"scalar", Limits{MaxScalarBytes: 16}, ""}, {"escaped caller value", Limits{}, strings.Repeat("\\", 4096)}} {
		t.Run(tc.name, func(t *testing.T) {
			d := fixture(t)
			d.Description = tc.description
			w := &observingWriter{}
			err := Write(context.Background(), w, d, Options{Limits: tc.limits})
			if !errors.Is(err, ErrResourceLimit) || w.writes != 0 {
				t.Fatalf("admission %v/writes%d", err, w.writes)
			}
			var field *ConfigurationError
			if !errors.As(err, &field) || field.LogicalField() == "" {
				t.Fatalf("missing fixed logical location: %v", err)
			}
		})
	}
}
func TestRawSchemaFuzzSeedsDoNotPanic(t *testing.T) {
	for _, s := range []string{"", " ", "{", "{\"", "{\"schemaVersion\":1,", "{\"schemaVersion\":01}", "[]", "null"} {
		if d, err := Parse(context.Background(), strings.NewReader(s), Options{}); err == nil || d != nil {
			t.Fatalf("accepted %q", s)
		}
	}
	d := fixture(t)
	var raw bytes.Buffer
	if err := json.NewEncoder(&raw).Encode(d); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(bytes.TrimSpace(raw.Bytes())); n++ {
		if _, err := Parse(context.Background(), bytes.NewReader(raw.Bytes()[:n]), Options{}); err == nil {
			t.Fatalf("accepted truncated offset %d", n)
		}
	}
}

func TestEscapedUnicodeDecodedLimits(t *testing.T) {
	base := string(fixtureBytes(t))
	encoded := strings.Repeat("\\uD83D\\uDE00", 8)
	raw := strings.Replace(base, "Synthetic two-AE receiver", encoded, 1)
	for _, tc := range []struct {
		max  int
		want error
	}{{31, ErrResourceLimit}, {32, nil}} {
		d, err := Parse(context.Background(), strings.NewReader(raw), Options{Limits: Limits{MaxNameBytes: tc.max}})
		if !errors.Is(err, tc.want) {
			t.Fatalf("decoded bound %d: %v", tc.max, err)
		}
		if err == nil {
			if len(d.DeviceName) != 32 {
				t.Fatalf("decoded length %d", len(d.DeviceName))
			}
			if _, err = Marshal(context.Background(), d, Options{Limits: Limits{MaxNameBytes: tc.max}}); err != nil {
				t.Fatalf("Unicode serialization: %v", err)
			}
		}
	}
}
