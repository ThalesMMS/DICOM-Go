package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/iconimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type metadataOptions struct {
	removeGroupLength, removePrivate, removeRetired bool
	split                                           int64
	splitSet                                        bool
	generateIcon, iconAutomatic, checkMeta          bool
	iconMinMax                                      string
	iconWindow                                      iconimage.WindowMode
	iconMin, iconMax                                float64
	uidRoot                                         string
	j2kBackend, j2kRate, j2kTile, j2kExecutable     string
	j2kResolutions                                  int
	j2kRates                                        []float64
	j2kWidth, j2kHeight                             int
}

func (m *metadataOptions) register(fs *flag.FlagSet) {
	fs.BoolVar(&m.removeGroupLength, "remove-gl", false, "alias for -group-length remove")
	fs.BoolVar(&m.removePrivate, "remove-private-tags", false, "recursively remove odd-group private attributes and creators; not complete de-identification")
	fs.BoolVar(&m.removeRetired, "remove-retired-tags", false, "recursively remove public attributes marked retired in the standard dictionary")
	fs.Int64Var(&m.split, "split", 0, "alias for fragment payload bytes (2..4294967295); odd values round down to even; encapsulated targets only")
	fs.BoolVar(&m.generateIcon, "generate-icon", false, "generate a native 64x64 Icon Image Sequence from the library's representative frame")
	fs.StringVar(&m.iconMinMax, "icon-minmax", "", "with -generate-icon: finite modality-domain MIN,MAX for monochrome pixels")
	fs.BoolVar(&m.iconAutomatic, "icon-auto-minmax", false, "with -generate-icon: ignore stored VOI and use monochrome automatic min/max")
	fs.BoolVar(&m.checkMeta, "check-meta", false, "report and rebuild the three Part 10 identity fields; not general IOD validation")
	fs.StringVar(&m.uidRoot, "root-uid", "", "organizational root for new SOP Instance UIDs from this call (not Study/Series/reference remapping)")
	fs.StringVar(&m.j2kBackend, "j2k-backend", "", "explicit optional encoder profile: openjpeg (requires the source-checkout build profile)")
	fs.StringVar(&m.j2kRate, "j2k-rate", "", "OpenJPEG .91 compression ratios, decreasing 1..10000 (requires -allow-lossy); unavailable for .90")
	fs.StringVar(&m.j2kTile, "j2k-tile", "", "OpenJPEG tile dimensions W,H, both 1..65535; includes conservative edge-tile admission")
	fs.IntVar(&m.j2kResolutions, "j2k-resolutions", 0, "OpenJPEG resolution levels 1..33 (0: finite geometry-derived default)")
	fs.StringVar(&m.j2kExecutable, "j2k-executable", "", "qualified OpenJPEG 2.5.4 opj_compress executable (trusted runtime path)")
}

func (m *metadataOptions) validate(opts *options, explicit map[string]bool) error {
	if m.removeGroupLength {
		if explicit["group-length"] && opts.groupLength != "remove" {
			return errors.New("-remove-gl conflicts with -group-length")
		}
		opts.groupLength = "remove"
	}
	m.splitSet = explicit["split"]
	if m.splitSet {
		if m.split < 2 || m.split > int64(^uint32(0)) {
			return errors.New("-split must be 2..4294967295 bytes")
		}
		effective := m.split - m.split%2
		if explicit["fragment-bytes"] && opts.fragmentBytes != effective {
			return errors.New("-split conflicts with -fragment-bytes")
		}
		opts.fragmentBytes = effective
	}
	if explicit["icon-minmax"] {
		if !m.generateIcon || m.iconAutomatic {
			return errors.New("-icon-minmax requires -generate-icon and conflicts with -icon-auto-minmax")
		}
		if len(m.iconMinMax) > 128 || strings.Count(m.iconMinMax, ",") != 1 {
			return errors.New("-icon-minmax requires two bounded numeric tokens")
		}
		parts := strings.Split(m.iconMinMax, ",")
		if len(parts) != 2 {
			return errors.New("-icon-minmax requires MIN,MAX")
		}
		lo, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		hi, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if e1 != nil || e2 != nil || math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(lo, 0) || math.IsInf(hi, 0) || lo >= hi {
			return errors.New("-icon-minmax requires finite MIN < MAX")
		}
		m.iconWindow, m.iconMin, m.iconMax = iconimage.WindowMinMax, lo, hi
	} else if m.iconAutomatic {
		if !m.generateIcon {
			return errors.New("-icon-auto-minmax requires -generate-icon")
		}
		m.iconWindow = iconimage.WindowAutomatic
	}
	if m.uidRoot != "" {
		if !core.IsValidUID(m.uidRoot) || len(m.uidRoot) > 64-1-core.MinRootedUIDDigits || m.uidRoot == "1.2.840.10008" || strings.HasPrefix(m.uidRoot, "1.2.840.10008.") || m.uidRoot == core.UUIDRoot || strings.HasPrefix(m.uidRoot, core.UUIDRoot+".") {
			return errors.New("-root-uid requires an organizational root with room for the library UID suffix")
		}
		if opts.uidPolicy == "preserve" {
			return errors.New("-root-uid conflicts with -uid-policy preserve")
		}
	}
	if m.j2kBackend != "" && m.j2kBackend != "openjpeg" {
		return errors.New("unknown JPEG 2000 backend")
	}
	controls := explicit["j2k-rate"] || explicit["j2k-tile"] || explicit["j2k-resolutions"] || explicit["j2k-executable"]
	if controls && m.j2kBackend != "openjpeg" {
		return errors.New("JPEG 2000 coding options require -j2k-backend openjpeg")
	}
	if m.j2kResolutions < 0 || m.j2kResolutions > 33 {
		return errors.New("JPEG 2000 resolutions must be 0..33")
	}
	if explicit["j2k-tile"] {
		if len(m.j2kTile) > 32 || strings.Count(m.j2kTile, ",") != 1 {
			return errors.New("JPEG 2000 tile requires two bounded integer tokens")
		}
		parts := strings.Split(m.j2kTile, ",")
		if len(parts) != 2 {
			return errors.New("JPEG 2000 tile requires W,H")
		}
		w, e1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		h, e2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if e1 != nil || e2 != nil || w < 1 || h < 1 || w > 65535 || h > 65535 {
			return errors.New("JPEG 2000 tile dimensions must be 1..65535")
		}
		m.j2kWidth, m.j2kHeight = w, h
	}
	if explicit["j2k-rate"] {
		if len(m.j2kRate) > 1024 || strings.Count(m.j2kRate, ",") >= 16 {
			return errors.New("JPEG 2000 rate tokens exceed bounded 16-layer admission")
		}
		parts := strings.Split(m.j2kRate, ",")
		if len(parts) > 16 {
			return errors.New("JPEG 2000 supports at most 16 rate layers")
		}
		for i, part := range parts {
			if len(part) > 64 {
				return errors.New("JPEG 2000 rate token exceeds 64 bytes")
			}
			v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > 10000 || i > 0 && v >= m.j2kRates[i-1] {
				return errors.New("JPEG 2000 rates must be finite 1..10000 and decreasing")
			}
			m.j2kRates = append(m.j2kRates, v)
		}
	}
	// Explicit coding controls request a new codestream, including the
	// automatic resolution setting. Profile/runtime selection alone may copy.
	if explicit["j2k-rate"] || explicit["j2k-tile"] || explicit["j2k-resolutions"] {
		opts.force = true
	}
	return nil
}

func (m metadataOptions) prepareEnabled() bool {
	return m.removePrivate || m.removeRetired || m.generateIcon || m.checkMeta
}

func applyMetadata(ctx context.Context, opts options, file *object.File, source io.ReaderAt, registry pixeldata.Registry) ([]string, error) {
	var lines []string
	if opts.metadata.checkMeta {
		report, err := file.ValidatePart10Identity()
		if err != nil {
			return nil, err
		}
		for _, finding := range report.Findings {
			lines = append(lines, fmt.Sprintf("file meta: %s %s", finding.Code, finding.Tag.String()))
		}
		if len(report.Findings) == 0 {
			lines = append(lines, "file meta: identities consistent")
		}
	}
	if opts.metadata.generateIcon {
		icon, err := iconimage.Generate(ctx, file, iconimage.GenerateOptions{Source: source, Registry: registry, Window: opts.metadata.iconWindow, Min: opts.metadata.iconMin, Max: opts.metadata.iconMax})
		if err != nil {
			return nil, err
		}
		file.Dataset.Put(icon)
		lines = append(lines, "generated native icon: 64x64")
	}
	if opts.metadata.removePrivate || opts.metadata.removeRetired {
		report, err := object.StripAttributes(ctx, file.Dataset, object.StripAttributesOptions{RemovePrivate: opts.metadata.removePrivate, RemoveRetired: opts.metadata.removeRetired})
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("removed attributes: private=%d retired=%d", report.RemovedPrivate, report.RemovedRetired))
	}
	if opts.metadata.checkMeta {
		if err := file.RebuildFileMeta(); err != nil {
			return nil, err
		}
		report, err := file.ValidatePart10Identity()
		if err != nil {
			return nil, err
		}
		if len(report.Findings) > 0 {
			return nil, errors.New("rebuilt file meta identities remain inconsistent")
		}
		lines = append(lines, "file meta: rebuilt identities verified")
	}
	return lines, nil
}

func distinctPreparationPaths(input, output string) error {
	in, err := filepath.Abs(input)
	if err != nil {
		return err
	}
	out, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	if in == out {
		return errors.New("input and output must be different files")
	}
	source, err := os.Stat(in)
	if err != nil {
		return err
	}
	dest, err := os.Stat(out)
	if err == nil && os.SameFile(source, dest) {
		return errors.New("input and output must be different files")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func preparationDigest(ctx context.Context, source *os.File, limit int64) ([sha256.Size]byte, error) {
	var zero [sha256.Size]byte
	info, err := source.Stat()
	if err != nil {
		return zero, err
	}
	if info.Size() < 0 || info.Size() > limit {
		return zero, errors.New("metadata preparation input byte limit exceeded")
	}
	hash := sha256.New()
	reader := io.NewSectionReader(source, 0, info.Size())
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		n, e := reader.Read(buffer)
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return zero, e
		}
	}
	copy(zero[:], hash.Sum(nil))
	return zero, nil
}

func verifyPreparationSource(ctx context.Context, source *os.File, path string, before [sha256.Size]byte, limit int64) error {
	after, err := preparationDigest(ctx, source, limit)
	if err != nil {
		return err
	}
	if before != after {
		return errors.New("input changed during metadata preparation")
	}
	original, err := source.Stat()
	if err != nil {
		return err
	}
	current, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(original, current) {
		return errors.New("input identity changed during metadata preparation")
	}
	return nil
}

func validateJPEG2000Target(opts options, target transfer.Syntax) error {
	m := opts.metadata
	if m.j2kBackend == "" {
		return nil
	}
	if target.UID != transfer.JPEG2000LosslessOnly.UID && target.UID != transfer.JPEG2000.UID {
		return errors.New("OpenJPEG profile requires a Part 1 JPEG 2000 target")
	}
	if target.UID == transfer.JPEG2000LosslessOnly.UID && len(m.j2kRates) > 0 {
		return errors.New("-j2k-rate is unavailable for the .90 lossless-only target")
	}
	if target.UID == transfer.JPEG2000.UID && (!opts.allowLossy || len(m.j2kRates) == 0) {
		return errors.New(".91 requires -allow-lossy and explicit -j2k-rate")
	}
	return nil
}
