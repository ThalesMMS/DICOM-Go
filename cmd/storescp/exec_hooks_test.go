package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func storageHookJSON(argv []string) string { b, _ := json.Marshal(argv); return string(b) }
func TestStorageHookStrictArgvAndExpansion(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "trusted-helper")
	for _, raw := range []string{"null", "[]", "{}", "[\"relative\"]", "[\"/absolute\"] true", "[\"/absolute\",\"x${path}\"]", "[\"${path}\"]", "[\"/absolute\",\"${unknown}\"]", storageHookJSON([]string{"/absolute", strings.Repeat("x", 8193)}), storageHookJSON([]string{"/absolute", "x\x00y"})} {
		if _, err := compileStorageHook(raw, false); !errors.Is(err, errStorageFeatureUsage) {
			t.Fatalf("accepted argv %q/%v", raw, err)
		}
	}
	args := []string{helper, "${path}", "${calling_ae}", "${called_ae}", "${sop_class_uid}", "${study_uid}", "literal ; $(touch SHOULD_NOT_EXIST) ' \""}
	spec, err := compileStorageHook(storageHookJSON(args), false)
	if err != nil {
		t.Fatal(err)
	}
	fields := storageEventFields{path: "/root/space and quote/file.dcm", calling: "SENDER", called: "MixedAE", class: "1.2.3", study: "1.2.4"}
	got, err := spec.expand(fields, true)
	want := []string{helper, fields.path, fields.calling, fields.called, fields.class, fields.study, args[6]}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q/%v", got, err)
	}
	fields.path = strings.Repeat("x", 8193)
	if _, err = spec.expand(fields, true); !errors.Is(err, errStorageFeatureUsage) {
		t.Fatal("expanded arg limit")
	}
	for _, token := range []string{"${path}", "${calling_ae}", "${sop_instance_uid}", "${directory}"} {
		if _, err := compileStorageHook(storageHookJSON([]string{helper, token}), true); err == nil {
			t.Fatalf("misleading study field %s", token)
		}
	}
	spec, err = compileStorageHook(storageHookJSON([]string{helper, "${generation}", "${count}", "${last_path}"}), true)
	if err != nil {
		t.Fatal(err)
	}
	got, err = spec.expand(storageEventFields{generation: 42, count: 7, path: "last"}, true)
	if err != nil || got[1] != "42" || got[2] != "7" || got[3] != "last" {
		t.Fatal(got, err)
	}
}
func TestStorageHookOutputCumulativeAndPreCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	output := &storageOutput{cancel: cancel}
	if n, err := output.Write(make([]byte, 4096)); n != 4096 || err != nil {
		t.Fatal(n, err)
	}
	if _, err := output.Write(make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{1}); !errors.Is(err, errStorageHookOutput) || !output.overLimit() || ctx.Err() == nil {
		t.Fatal("output cap did not cancel")
	}
	if err := runStorageHook(ctx, []string{"/never/start"}, t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
