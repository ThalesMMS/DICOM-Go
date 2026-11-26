package jpeg

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type progressive12Fixture struct {
	Name, UID, Pattern, Generator                      string
	Metadata                                           pixeldata.Metadata
	Streams, References, StreamHashes, ReferenceHashes []string
	NativeSHA256, CompressedSHA256, OracleSHA256       string
	Commands                                           [][]string
	PSNR                                               []string
	MaximumSourceError                                 []int
	MaximumDecoderDifference                           int
}

type progressive12Receipt struct {
	License                                             string
	NoPHI                                               bool
	Schema, SourceBase, Provenance, OS, Arch, GoVersion string
	Successful                                          bool
	Tools                                               map[string]extendedOracleTool
	Sources, RuntimeFiles                               map[string]string
	Cases                                               []progressive12Fixture
}

func progressive12CompressedObject(m pixeldata.Metadata, streams [][]byte) *object.Object {
	obj := extendedProfileObject(m, nil)
	fragments := make([][]byte, len(streams))
	for i, stream := range streams {
		fragments[i] = append([]byte(nil), stream...)
		if len(fragments[i])%2 != 0 {
			fragments[i] = append(fragments[i], 0)
		}
	}
	obj.Put(dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, fragments...))
	return obj
}

// This opt-in generator encodes only own synthetic rasters. Expected stored
// samples always come from DCMTK's independently executed IJG12 decoder;
// the codec under test never provides the oracle reference. Export is explicit.
func TestProgressive12DCMTKQualification(t *testing.T) {
	if os.Getenv("DICOMGO_JPEG_PROGRESSIVE12_INTEROP") != "1" {
		t.Skip("set DICOMGO_JPEG_PROGRESSIVE12_INTEROP=1 with DCMTK and cjpeg")
	}
	dir := os.Getenv("DICOMGO_JPEG_PROGRESSIVE12_EVIDENCE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	export := os.Getenv("DICOMGO_JPEG_PROGRESSIVE12_EXPORT_DIR")
	if export != "" {
		if err := os.MkdirAll(export, 0700); err != nil {
			t.Fatal(err)
		}
	}
	r := progressive12Receipt{Schema: "jpeg-progressive12-v1", License: "Apache-2.0", NoPHI: true, SourceBase: "7bb575edc44d195d3c6a9c343277b17d8c8a9962", Provenance: "Own deterministic unsigned 12-bit rasters and Part10/PNM files; no patient data, vendor implementation source or vendor image assets.", OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), Tools: map[string]extendedOracleTool{}, Sources: map[string]string{}, RuntimeFiles: map[string]string{}}
	t.Cleanup(func() {
		r.Successful = !t.Failed() && len(r.Cases) == 16
		data, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		data = append(data, '\n')
		if err := os.WriteFile(filepath.Join(dir, "qualification.json"), data, 0600); err != nil {
			t.Error(err)
		}
		if r.Successful && export != "" {
			if err := os.WriteFile(filepath.Join(export, "manifest.json"), data, 0600); err != nil {
				t.Error(err)
			}
		}
	})
	for _, name := range []string{"progressive12.go", "progressive12_interop_test.go", "jpeg.go", "extended.go", "extended_encoder_profiles_test.go", "extended_encoder_test.go", "progressive12_test.go", "dcmcjpeg_test.go"} {
		r.Sources[name] = extendedOracleHash(t, name)
	}
	for _, name := range []string{"dcmcjpeg", "dcmdjpeg", "cjpeg"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		flag := "--version"
		if name == "cjpeg" {
			flag = "-version"
		}
		r.Tools[name] = extendedOracleTool{Path: path, Version: strings.TrimSpace(string(extendedOracleRun(t, path, flag))), SHA256: extendedOracleHash(t, path)}
	}
	if runtime.GOOS == "windows" {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(r.Tools["dcmcjpeg"].Path), "libijg12*.dll"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Fatal("IJG12 runtime not found")
		}
		for _, path := range matches {
			r.RuntimeFiles[filepath.Base(path)] = extendedOracleHash(t, path)
		}
	}
	for _, color := range []bool{false, true} {
		for _, spectral := range []bool{true, false} {
			for _, pattern := range []string{"gradient", "flat", "edges", "restart"} {
				label, photo := "mono", "MONOCHROME2"
				if color {
					label, photo = "rgb", "RGB"
				}
				process, uid, syntax := "+ep", UIDFullProgression, transfer.JPEGFullProgression
				if spectral {
					process, uid, syntax = "+es", UIDSpectralSelection, transfer.JPEGSpectralSelection
				}
				name := label + "-" + strings.TrimPrefix(process, "+") + "-" + pattern
				t.Run(name, func(t *testing.T) {
					m := extendedProfileMetadata(12, photo)
					m.NumberOfFrames = 2
					var raw []byte
					for phase := 0; phase < 2; phase++ {
						frame := extendedProfileFrame(m, phase)
						if pattern == "flat" {
							for i := 0; i < len(frame); i += 2 {
								binary.LittleEndian.PutUint16(frame[i:], uint16(600+phase*800+(i/2%int(m.SamplesPerPixel))*1000))
							}
						}
						if pattern == "edges" {
							for y := 0; y < int(m.Rows); y++ {
								for x := 0; x < int(m.Columns); x++ {
									for c := 0; c < int(m.SamplesPerPixel); c++ {
										i := ((y*int(m.Columns)+x)*int(m.SamplesPerPixel) + c) * 2
										v := uint16((x*43 + y*67 + c*997 + x*y*19 + phase*773) & 4095)
										binary.LittleEndian.PutUint16(frame[i:], v)
									}
								}
							}
						}
						raw = append(raw, frame...)
					}
					source := extendedProfileObject(m, raw)
					nativePath, compressedPath, oraclePath := filepath.Join(dir, name+"-native.dcm"), filepath.Join(dir, name+".dcm"), filepath.Join(dir, name+"-oracle.dcm")
					writeExtendedTestFile(t, nativePath, source, transfer.ExplicitVRLittleEndian)
					f := progressive12Fixture{Name: name, UID: uid, Pattern: pattern, Metadata: m, Generator: "DCMTK dcmcjpeg"}
					if pattern != "restart" {
						quality := "90"
						if pattern == "edges" {
							quality = "100"
						}
						args := []string{process, "+bt", "+sr", "+cr", "+q", quality, nativePath, compressedPath}
						extendedOracleRun(t, r.Tools["dcmcjpeg"].Path, args...)
						f.Commands = append(f.Commands, append([]string{r.Tools["dcmcjpeg"].Path}, args...))
					} else {
						f.Generator = "libjpeg-turbo cjpeg -restart 3B, independently decoded by DCMTK IJG12"
						var streams [][]byte
						for phase := 0; phase < 2; phase++ {
							pnm := filepath.Join(dir, fmt.Sprintf("%s-%d.pnm", name, phase))
							jpg := filepath.Join(dir, fmt.Sprintf("%s-%d.jpg", name, phase))
							magic := "P5"
							if color {
								magic = "P6"
							}
							data := []byte(fmt.Sprintf("%s\n%d %d\n4095\n", magic, m.Columns, m.Rows))
							frame := raw[phase*int(m.FrameSize()) : (phase+1)*int(m.FrameSize())]
							for i := 0; i < len(frame); i += 2 {
								data = append(data, frame[i+1], frame[i])
							}
							if err := os.WriteFile(pnm, data, 0600); err != nil {
								t.Fatal(err)
							}
							args := []string{"-precision", "12", "-quality", "90", "-restart", "3B", "-outfile", jpg}
							if color {
								args = append(args, "-rgb", "-sample", "1x1,1x1,1x1")
							} else {
								args = append(args, "-grayscale")
							}
							if spectral {
								script := "0:0 0 0 0;\n0:1 63 0 0;\n"
								if color {
									script = "0,1,2:0 0 0 0;\n0:1 63 0 0;\n1:1 63 0 0;\n2:1 63 0 0;\n"
								}
								scriptPath := filepath.Join(dir, name+"-scans.txt")
								if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
									t.Fatal(err)
								}
								args = append(args, "-scans", scriptPath)
							} else {
								args = append(args, "-progressive")
							}
							args = append(args, pnm)
							extendedOracleRun(t, r.Tools["cjpeg"].Path, args...)
							f.Commands = append(f.Commands, append([]string{r.Tools["cjpeg"].Path}, args...))
							stream, err := os.ReadFile(jpg)
							if err != nil {
								t.Fatal(err)
							}
							streams = append(streams, stream)
						}
						writeExtendedTestFile(t, compressedPath, progressive12CompressedObject(m, streams), syntax)
					}
					args := []string{"+cn", "+px", "-bs", "+te", compressedPath, oraclePath}
					extendedOracleRun(t, r.Tools["dcmdjpeg"].Path, args...)
					f.Commands = append(f.Commands, append([]string{r.Tools["dcmdjpeg"].Path}, args...))
					f.NativeSHA256, f.CompressedSHA256, f.OracleSHA256 = extendedOracleHash(t, nativePath), extendedOracleHash(t, compressedPath), extendedOracleHash(t, oraclePath)
					oracle, err := object.OpenFile(oraclePath)
					if err != nil {
						t.Fatal(err)
					}
					defer oracle.Close()
					want, err := pixeldata.ExtractNativeFrames(oracle.Dataset)
					if err != nil {
						t.Fatal(err)
					}
					if want.Metadata != m {
						t.Fatalf("oracle metadata got %+v want %+v", want.Metadata, m)
					}
					file, err := object.OpenFile(compressedPath)
					if err != nil {
						t.Fatal(err)
					}
					defer file.Close()
					if file.TransferSyntax.UID != uid {
						t.Fatal("wrong generated transfer syntax", file.TransferSyntax)
					}
					pixel, err := pixeldata.Extract(file.Dataset)
					if err != nil {
						t.Fatal(err)
					}
					got, err := progressiveRegistry(t).DecodeFrames(uid, pixel, file.Dataset)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := encapsulated.FromFragments(context.Background(), pixel.Sequence, file.Dataset, 2, encapsulated.JPEG, encapsulated.Limits{})
					if err != nil {
						t.Fatal(err)
					}
					var expected, actual []int
					for phase := 0; phase < 2; phase++ {
						view, err := plan.Frame(context.Background(), phase)
						if err != nil {
							t.Fatal(err)
						}
						streamName, refName := fmt.Sprintf("%s-%d.jpg", name, phase), fmt.Sprintf("%s-%d.raw", name, phase)
						streamPath, refPath := filepath.Join(dir, streamName), filepath.Join(dir, refName)
						if err := os.WriteFile(streamPath, view.Data, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(refPath, want.Data[phase], 0600); err != nil {
							t.Fatal(err)
						}
						f.Streams = append(f.Streams, streamName)
						f.References = append(f.References, refName)
						f.StreamHashes = append(f.StreamHashes, extendedOracleHash(t, streamPath))
						f.ReferenceHashes = append(f.ReferenceHashes, extendedOracleHash(t, refPath))
						if export != "" {
							if err := os.WriteFile(filepath.Join(export, streamName), view.Data, 0600); err != nil {
								t.Fatal(err)
							}
							if err := os.WriteFile(filepath.Join(export, refName), want.Data[phase], 0600); err != nil {
								t.Fatal(err)
							}
						}
						actual = append(actual, extendedProfileSamples(got.Data[phase], m)...)
						expected = append(expected, extendedProfileSamples(want.Data[phase], m)...)
					}
					if len(actual) != len(expected) {
						t.Fatal("sample count")
					}
					for i := range actual {
						f.MaximumDecoderDifference = max(f.MaximumDecoderDifference, abs(actual[i]-expected[i]))
					}
					if f.MaximumDecoderDifference > 2 {
						t.Fatal("independent decoder difference", f.MaximumDecoderDifference)
					}
					psnr, worst := extendedOracleMetrics(actual, extendedProfileSamples(raw, m), m)
					f.MaximumSourceError = worst
					for _, db := range psnr {
						if math.IsInf(db, 1) {
							f.PSNR = append(f.PSNR, "Infinity (exact)")
						} else {
							f.PSNR = append(f.PSNR, strconv.FormatFloat(db, 'f', 6, 64))
						}
					}
					t.Logf("max decoder delta=%d PSNR=%v", f.MaximumDecoderDifference, f.PSNR)
					r.Cases = append(r.Cases, f)
				})
			}
		}
	}
}
