package registration

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/hdf5"
)

// ITK HDF5 transform files (#1252). ITK's HDF5TransformIO, which ANTs and
// Slicer use for .h5 files, stores
//
//	/TransformGroup/<N>/TransformType             the type name, a string
//	/TransformGroup/<N>/TransformFixedParameters  1-D float64 vector
//	/TransformGroup/<N>/TransformParameters       1-D float64 or float32
//
// for N = 0, 1, … with a composite transform first and its components in
// queue order (the last applied first), the parameters optionally chunked
// and deflated, alongside version strings at the root. Files of old ITK
// versions spell the parameter datasets "TranformFixedParameters" and
// "TranformParameters"; both spellings are read. The file is decoded by a
// minimal pure-Go HDF5 reader (internal/hdf5) covering the subset these
// writers produce. Superblock versions 0 to 3 support compact and
// symbol-table groups, and dense hard links in unfiltered managed fractal
// heaps with version 2 indexes. Dataset support covers contiguous, compact
// and classic chunked layouts, deflate/shuffle/Fletcher-32 filters, and
// floating-point and string data.

// maxHDF5Transforms bounds the transforms of one file.
const maxHDF5Transforms = 1024

func isHDF5(data []byte) bool { return hdf5.IsHDF5(data) }

// ReadITKHDF5 reads an ITK HDF5 transform file of size bytes. A file
// opening with a composite transform returns one Composite of the
// transforms that follow, in application order (ITK's queue reversed), as
// ReadITKText does; otherwise the transforms in file order.
func ReadITKHDF5(r io.ReaderAt, size int64) ([]Transform, error) {
	records, composite, err := readITKHDF5(r, size, 3)
	if err != nil {
		return nil, err
	}
	return assembleITK3D(records, composite)
}

func readITKHDF5(r io.ReaderAt, size int64, dimension int) ([]itkParameters, bool, error) {
	return readITKHDF5Bounded(r, size, dimension, maxDenseTransformParameters)
}

func readITKHDF5Bounded(r io.ReaderAt, size int64, dimension, remaining int) ([]itkParameters, bool, error) {
	file, err := hdf5.Open(r, size)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	const group = "/TransformGroup"
	links, err := file.Links(group)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	if len(links) == 0 {
		return nil, false, fmt.Errorf("%w: no transform", ErrTransformFile)
	}
	if len(links) > maxHDF5Transforms {
		return nil, false, fmt.Errorf("%w: %d transforms", ErrTransformFile, len(links))
	}
	var transforms []itkParameters
	composite := false
	for i := range links {
		base := group + "/" + strconv.Itoa(i)
		typeName, err := file.ReadString(base + "/TransformType")
		if err != nil {
			return nil, false, fmt.Errorf("%w: transform %d: %w", ErrTransformFile, i, err)
		}
		t, err := newITKParameters(strings.TrimSpace(typeName), dimension)
		if err != nil {
			return nil, false, fmt.Errorf("%w: transform %d: %w", ErrTransformFile, i, err)
		}
		if t.Kind() == KindComposite {
			if i != 0 {
				return nil, false, fmt.Errorf("%w: transform %d: a composite transform must be the first", ErrTransformFile, i)
			}
			composite = true
			continue
		}
		fixed, err := readHDF5Vector(file, base, "FixedParameters", maxTransformFileParameters)
		if err != nil {
			return nil, false, fmt.Errorf("%w: transform %d (%s): %w", ErrTransformFile, i, t.Kind(), err)
		}

		parameters, err := readHDF5Vector(file, base, "Parameters", min(parameterLimit(t), remaining))
		if err != nil {
			return nil, false, fmt.Errorf("%w: transform %d (%s): %w", ErrTransformFile, i, t.Kind(), err)
		}
		if err := setITKParameters(t, parameters, fixed); err != nil {
			return nil, false, err
		}
		remaining -= len(parameters)
		transforms = append(transforms, t)
	}
	if composite && len(transforms) == 0 {
		return nil, false, fmt.Errorf("%w: empty composite transform", ErrTransformFile)
	}
	return transforms, composite, nil
}

// readHDF5Vector reads base/Transform<name>, or the misspelled
// base/Tranform<name> of old ITK versions.
func readHDF5Vector(file *hdf5.File, base, name string, limit int) ([]float64, error) {
	path := base + "/Transform" + name
	if !file.Exists(path) {
		if misspelled := base + "/Tranform" + name; file.Exists(misspelled) {
			path = misspelled
		}
	}
	values, err := file.ReadFloat64s(path, limit)
	if errors.Is(err, hdf5.ErrNotFound) {
		return nil, fmt.Errorf("no %s", "Transform"+name)
	}
	return values, err
}
