package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pathtemplate"
	"io"
	"path/filepath"
	"strings"
	"time"
)

var errStorageLayout = errors.New("storescp: invalid storage layout metadata")
var errStorageFeatureUsage = errors.New("storescp: invalid layout or hook configuration")

type storageFeatureFlags struct {
	pattern, extension, reception, endStudy string
	sortStudy, unique, timeNames            bool
	inactivity, hookTimeout                 time.Duration
	capacity, studies                       int
	hookBytes, studyBytes                   int64
}
type storageLayoutPolicy struct {
	template                     *pathtemplate.Template
	sortStudy, unique, timeNames bool
	extension                    string
	reception, endStudy          *storageHookSpec
	flags                        storageFeatureFlags
	random                       io.Reader
}

func (f *storageFeatureFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.pattern, "path-template", "", "relative attribute path template including extension; errors refuse publication")
	fs.BoolVar(&f.sortStudy, "sort-on-study-uid", false, "store under each canonical StudyInstanceUID")
	fs.BoolVar(&f.unique, "unique-filenames", false, "use random 128-bit hexadecimal basenames")
	fs.BoolVar(&f.timeNames, "timenames", false, "use UTC receipt timestamps as basenames")
	fs.StringVar(&f.extension, "filename-extension", ".dcm", "suffix for built-in filename modes; explicit empty omits extension")
	fs.StringVar(&f.reception, "exec-on-reception", "", "JSON argv array for a bounded hook after successful storage; no shell")
	fs.StringVar(&f.endStudy, "exec-on-eostudy", "", "JSON argv array for per-AE/study inactivity; no completeness assertion")
	fs.DurationVar(&f.inactivity, "eostudy-timeout", 30*time.Second, "study inactivity interval when its hook is enabled")
	fs.DurationVar(&f.hookTimeout, "hook-timeout", 30*time.Second, "per-hook process timeout")
	fs.IntVar(&f.capacity, "hook-queue-depth", 256, "maximum outstanding hook reservations/events including future study events")
	fs.IntVar(&f.studies, "max-active-studies", 128, "maximum tracked study generations")
	fs.Int64Var(&f.hookBytes, "hook-max-bytes", 16<<20, "logical bytes reserved for active and queued hook events")
	fs.Int64Var(&f.studyBytes, "max-study-state-bytes", 1<<20, "logical retained study state byte ceiling")
}

func compileStorageFeatures(fs *flag.FlagSet, f storageFeatureFlags, single bool) (*storageLayoutPolicy, error) {
	explicit := map[string]bool{}
	fs.Visit(func(x *flag.Flag) { explicit[x.Name] = true })
	hooks := f.reception != "" || f.endStudy != ""
	for _, n := range []string{"hook-timeout", "hook-queue-depth", "hook-max-bytes"} {
		if explicit[n] && !hooks {
			return nil, errStorageFeatureUsage
		}
	}
	for _, n := range []string{"eostudy-timeout", "max-active-studies", "max-study-state-bytes"} {
		if explicit[n] && f.endStudy == "" {
			return nil, errStorageFeatureUsage
		}
	}
	if explicit["path-template"] && f.pattern == "" || explicit["exec-on-reception"] && f.reception == "" || explicit["exec-on-eostudy"] && f.endStudy == "" {
		return nil, errStorageFeatureUsage
	}
	if f.pattern != "" && (f.sortStudy || f.unique || f.timeNames || explicit["filename-extension"]) || f.unique && f.timeNames || single && f.endStudy != "" {
		return nil, errStorageFeatureUsage
	}
	if !hooks && !f.sortStudy && !f.unique && !f.timeNames && f.pattern == "" && !explicit["filename-extension"] {
		return nil, nil
	}
	p := &storageLayoutPolicy{sortStudy: f.sortStudy, unique: f.unique, timeNames: f.timeNames, extension: f.extension, flags: f, random: rand.Reader}
	if f.pattern != "" {
		var err error
		p.template, err = pathtemplate.Compile(f.pattern, pathtemplate.Options{})
		if err != nil {
			return nil, errStorageFeatureUsage
		}
	}
	if len(f.extension) > 32 || f.extension != "" && (len(f.extension) < 2 || f.extension[0] != '.' || strings.HasSuffix(f.extension, ".")) {
		return nil, errStorageFeatureUsage
	}
	for _, c := range f.extension {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return nil, errStorageFeatureUsage
		}
	}
	if f.inactivity <= 0 || f.inactivity > 24*time.Hour || f.hookTimeout <= 0 || f.hookTimeout > 10*time.Minute || f.capacity <= 0 || f.capacity > 4096 || f.hookBytes <= 0 || f.hookBytes > 256<<20 || f.studies <= 0 || f.studies > 4096 || f.studyBytes <= 0 || f.studyBytes > 64<<20 || f.endStudy != "" && f.studies >= f.capacity {
		return nil, errStorageFeatureUsage
	}
	if len(f.reception) > 65536-len(f.endStudy) {
		return nil, errStorageFeatureUsage
	}
	var err error
	if f.reception != "" {
		p.reception, err = compileStorageHook(f.reception, false)
		if err != nil {
			return nil, errStorageFeatureUsage
		}
	}
	if f.endStudy != "" {
		p.endStudy, err = compileStorageHook(f.endStudy, true)
		if err != nil {
			return nil, errStorageFeatureUsage
		}
	}
	return p, nil
}

func storageStudyUID(dataset *object.Object) (string, error) {
	if dataset == nil {
		return "", errStorageLayout
	}
	tag := core.NewTag(0x0020, 0x000D)
	e, ok := dataset.Get(tag)
	if !ok || e.VR() != core.VRUI {
		return "", errStorageLayout
	}
	if raw, ok := e.RawBytes(); ok {
		if len(raw) > 64 {
			return "", errStorageLayout
		}
	} else if v, ok := e.Value.(core.StringValue); !ok || len(v) != 1 || len(v[0]) > 64 {
		return "", errStorageLayout
	}
	values, ok := dataset.GetUIDs(tag)
	if !ok || len(values) != 1 || !core.IsValidUID(values[0]) {
		return "", errStorageLayout
	}
	return values[0], nil
}

func (p *storageLayoutPolicy) format(ctx context.Context, dataset *object.Object, instance, study string, now time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.template != nil {
		result, err := p.template.Format(dataset)
		if err != nil {
			return "", errStorageLayout
		}
		return result, nil
	}
	name := instance
	if p.unique {
		var token [16]byte
		if _, err := io.ReadFull(p.random, token[:]); err != nil {
			return "", err
		}
		name = hex.EncodeToString(token[:])
	}
	if p.timeNames {
		name = now.UTC().Format("20060102T150405.000000000Z")
	}
	relative := name + p.extension
	if p.sortStudy {
		relative = study + "/" + relative
	}
	if err := pathtemplate.ValidateRelativePath(relative); err != nil {
		return "", errStorageLayout
	}
	return relative, nil
}
func storageAbsoluteRoot(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil || len(root) > 4096 {
		return "", errStorageFeatureUsage
	}
	return root, nil
}
