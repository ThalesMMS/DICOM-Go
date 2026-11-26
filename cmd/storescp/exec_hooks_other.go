//go:build !darwin && !linux && !windows

package main

import (
	"context"
	"io"
	"os"
)

func storageHookPlatformReady() error                     { return errStorageFeatureUsage }
func storageExecutablePlatform(string, os.FileInfo) error { return errStorageFeatureUsage }
func runStorageHookPlatform(context.Context, []string, string, io.Writer) error {
	return errStorageHook
}

func storageHookArgumentProfile(_ *storageHookSpec, _ storageEventFields) error { return nil }
