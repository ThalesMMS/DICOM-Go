package siemens

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var gdcmdumpCSALine = regexp.MustCompile(`^(\d+) - '([^']*)' VM (\S+), VR (\S+), SyngoDT (\d+), NoOfItems (\d+), Data ?(.*)$`)

type gdcmCSAElement struct {
	index                 int
	name, vm, vr, data    string
	syngoDT, items        int
	singleLine, hasValues bool
}

// gdcmdumpCSA returns the elements gdcmdump --csa prints for the image
// (0029,xx10) and series (0029,xx20) headers.
func gdcmdumpCSA(t *testing.T, gdcmdump, path string) map[string][]gdcmCSAElement {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, gdcmdump, "--csa", path).CombinedOutput()
	if err != nil {
		return nil
	}
	sections := map[string][]gdcmCSAElement{}
	section := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "Image shadow data"):
			section = "image"
			continue
		case strings.HasPrefix(line, "Series shadow data"):
			section = "series"
			continue
		}
		m := gdcmdumpCSALine.FindStringSubmatch(line)
		if m == nil || section == "" {
			continue
		}
		index, _ := strconv.Atoi(m[1])
		syngoDT, _ := strconv.Atoi(m[5])
		items, _ := strconv.Atoi(m[6])
		data := m[7]
		sections[section] = append(sections[section], gdcmCSAElement{
			index: index, name: m[2], vm: m[3], vr: m[4], syngoDT: syngoDT, items: items, data: data,
			hasValues:  data != "",
			singleLine: data == "" || (strings.HasPrefix(data, "'") && strings.HasSuffix(data, "'")),
		})
	}
	return sections
}

// TestCSAHeadersMatchGDCM parses every CSA header of the gdcmData corpus and
// compares names, VM, VR, SyngoDT, item counts and single-line values with
// gdcmdump --csa. Enable with DICOMGO_GDCMDATA_ROOT and gdcmdump on PATH.
func TestCSAHeadersMatchGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT with gdcmdump on PATH")
	}
	gdcmdump, err := exec.LookPath("gdcmdump")
	if err != nil {
		t.Fatal("gdcmdump is required")
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*"))
	headers, elements := 0, 0
	formats := map[CSAFormat]int{}
	for _, path := range paths {
		file, err := object.OpenFile(path)
		if err != nil {
			continue
		}
		ours := map[string]LocatedHeader{}
		for section, locate := range map[string]func(*object.Object, LookupOptions) (LocatedHeader, error){"image": ImageHeader, "series": SeriesHeader} {
			if header, err := locate(file.Dataset, LookupOptions{}); err == nil {
				ours[section] = header
			} else if !strings.Contains(err.Error(), "no CSA header") {
				t.Errorf("%s %s: %v", filepath.Base(path), section, err)
			}
		}
		_ = file.Close()
		if len(ours) == 0 {
			continue
		}
		theirs := gdcmdumpCSA(t, gdcmdump, path)
		for section, header := range ours {
			formats[header.Format]++
			if header.Format != CSAFormatSV10 && header.Format != CSAFormatNOSV {
				continue
			}
			headers++
			reference := theirs[section]
			if len(reference) != len(header.Elements) {
				t.Errorf("%s %s: %d elements, gdcmdump %d", filepath.Base(path), section, len(header.Elements), len(reference))
				continue
			}
			for i, element := range header.Elements {
				elements++
				ref := reference[i]
				got := fmt.Sprintf("%s VR %s SyngoDT %d items %d", element.Name, element.VR, element.SyngoDT, element.Items)
				want := fmt.Sprintf("%s VR %s SyngoDT %d items %d", ref.name, ref.vr, ref.syngoDT, ref.items)
				if got != want || (isDigits(ref.vm) && ref.vm != strconv.Itoa(element.VM)) {
					t.Errorf("%s %s element %d: %s VM %d, gdcmdump %s VM %s", filepath.Base(path), section, i, got, element.VM, want, ref.vm)
					continue
				}
				if ref.singleLine && !strings.ContainsAny(strings.Join(element.Values, ""), "\n\r") {
					data := ""
					if len(element.Values) > 0 {
						data = "'" + strings.Join(element.Values, `'\'`) + "'"
					}
					if data != ref.data {
						t.Errorf("%s %s %s: data %q, gdcmdump %q", filepath.Base(path), section, element.Name, data, ref.data)
					}
				}
			}
		}
	}
	t.Logf("%d SV10/NOSV headers, %d elements compared with gdcmdump; formats %v", headers, elements, formats)
	if headers == 0 {
		t.Fatal("no CSA header found in the corpus")
	}
}

// isDigits reports a plain VM; gdcmdump prints others as ranges ("1-n").
func isDigits(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// TestScrubCSAMatchesGDCM scrubs every SV10/NOSV header of the gdcmData
// corpus and compares the result byte by byte with gdcmclean --scrub.
// Enable with DICOMGO_GDCMDATA_ROOT and gdcmclean on PATH.
func TestScrubCSAMatchesGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT with gdcmclean on PATH")
	}
	gdcmclean, err := exec.LookPath("gdcmclean")
	if err != nil {
		t.Fatal("gdcmclean is required")
	}
	paths, _ := filepath.Glob(filepath.Join(root, "*"))
	headers, cleared, identical, extraCleared := 0, 0, 0, 0
	for _, path := range paths {
		file, err := object.OpenFile(path)
		if err != nil {
			continue
		}
		var located []LocatedHeader
		for _, locate := range []func(*object.Object, LookupOptions) (LocatedHeader, error){ImageHeader, SeriesHeader} {
			if header, err := locate(file.Dataset, LookupOptions{}); err == nil && (header.Format == CSAFormatSV10 || header.Format == CSAFormatNOSV) {
				located = append(located, header)
			}
		}
		raws := map[core.Tag][]byte{}
		for _, header := range located {
			raws[header.Tag], _ = file.Dataset.GetRaw(header.Tag)
		}
		_ = file.Close()
		if len(located) == 0 {
			continue
		}
		output := filepath.Join(t.TempDir(), "scrubbed.dcm")
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out, err := exec.CommandContext(ctx, gdcmclean, "--scrub", "0029,10,SIEMENS CSA HEADER", "--scrub", "0029,20,SIEMENS CSA HEADER", "-i", path, "-o", output).CombinedOutput()
		cancel()
		if err != nil {
			t.Logf("%s: gdcmclean failed, skipped: %v %s", filepath.Base(path), err, out)
			continue
		}
		reference, err := object.OpenFile(output)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
			continue
		}
		for _, header := range located {
			want, _ := reference.Dataset.GetRaw(header.Tag)
			got, report, err := ScrubCSA(raws[header.Tag], CSALimits{})
			if err != nil {
				t.Errorf("%s %s: %v", filepath.Base(path), header.Tag, err)
				continue
			}
			headers++
			cleared += report.Cleared
			// Every byte gdcmclean clears is cleared; the other bytes this
			// scrub clears (the fourth VR byte and value padding, which
			// gdcmclean keeps) become zero; the header reads the same.
			source := raws[header.Tag]
			if len(got) != len(want) || len(got) != len(source) {
				t.Errorf("%s %s: %d bytes, gdcmclean %d, source %d", filepath.Base(path), header.Tag, len(got), len(want), len(source))
				continue
			}
			extra := 0
			for i := range got {
				switch {
				case got[i] == want[i]:
				case got[i] == 0 && want[i] == source[i]:
					extra++
				default:
					t.Errorf("%s %s (%s): byte %d is %#x, gdcmclean %#x, source %#x", filepath.Base(path), header.Tag, header.Format, i, got[i], want[i], source[i])
				}
			}
			if bytes.Equal(got, want) {
				identical++
			}
			extraCleared += extra
			reparsed, err := ParseCSA(got)
			if err != nil || !reflect.DeepEqual(reparsed.Elements, header.Elements) {
				t.Errorf("%s %s: scrubbed header reads differently: %v", filepath.Base(path), header.Tag, err)
			}
			if again, _, _ := ScrubCSA(got, CSALimits{}); !bytes.Equal(again, got) {
				t.Errorf("%s %s: scrub is not idempotent", filepath.Base(path), header.Tag)
			}
		}
		_ = reference.Close()
	}
	t.Logf("%d SV10/NOSV headers scrubbed (%d bytes cleared), %d identical to gdcmclean --scrub; %d bytes cleared beyond it", headers, cleared, identical, extraCleared)
	if headers == 0 {
		t.Fatal("no CSA header found in the corpus")
	}
}
