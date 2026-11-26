package registration

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

func newITKTransform2D(name string) (Transform2D, error) {
	p := strings.Split(name, "_")
	if len(p) != 4 || (p[1] != "double" && p[1] != "float") || p[2] != "2" || p[3] != "2" {
		return nil, fmt.Errorf("%w: %s is not a supported 2D type", ErrUnsupportedTransform, name)
	}
	switch Kind(p[0]) {
	case KindTranslation:
		return NewTranslation2D(), nil
	case KindEuler2D:
		return NewEuler2D(), nil
	case KindSimilarity2D:
		return NewSimilarity2D(), nil
	case KindAffine:
		return NewAffine2D(), nil
	case KindComposite:
		return NewComposite2D()
	}
	if p[0] == "MatrixOffsetTransformBase" {
		return NewAffine2D(), nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnsupportedTransform, name)
}
func itkTypeName2D(t Transform2D) (string, error) {
	if !builtin2D(t, true) {
		return "", ErrUnsupportedTransform
	}
	switch t.Kind() {
	case KindTranslation, KindEuler2D, KindSimilarity2D, KindAffine, KindComposite:
		return string(t.Kind()) + "_double_2_2", nil
	}
	return "", ErrUnsupportedTransform
}
func assembleITK2D(rs []itkParameters, composite bool) ([]Transform2D, error) {
	ts := make([]Transform2D, len(rs))
	for i, t := range rs {
		ts[i] = t.(Transform2D)
	}
	if !composite {
		return ts, nil
	}
	for i, j := 0, len(ts)-1; i < j; i, j = i+1, j-1 {
		ts[i], ts[j] = ts[j], ts[i]
	}
	c, err := NewComposite2D(ts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	return []Transform2D{c}, nil
}

// ReadTransformFile2D recognizes text, MATLAB and HDF5 and requires one
// native 2D transform (possibly composite). It never lifts to 3D.
func ReadTransformFile2D(data []byte) (Transform2D, error) {
	var ts []Transform2D
	var err error
	if isHDF5(data) {
		ts, err = ReadITKHDF52D(bytes.NewReader(data), int64(len(data)))
	} else if bytes.HasPrefix(bytes.TrimLeft(data, " \t\r\n"+utf8BOM), []byte("#")) {
		ts, err = ReadITKText2D(bytes.NewReader(data))
	} else {
		ts, err = ReadITKMatlab2D(bytes.NewReader(data))
	}
	if err != nil {
		return nil, err
	}
	if len(ts) != 1 {
		return nil, fmt.Errorf("%w: file must hold one 2D transform or composite", ErrTransformFile)
	}
	return ts[0], nil
}

// ReadITKText2D reads the supported linear 2D families and composites.
func ReadITKText2D(r io.Reader) ([]Transform2D, error) {
	rs, c, err := readITKText(r, 2)
	if err != nil {
		return nil, err
	}
	return assembleITK2D(rs, c)
}

// ReadITKMatlab2D reads linear 2D MATLAB Level4 records; composites are refused.
func ReadITKMatlab2D(r io.Reader) ([]Transform2D, error) {
	rs, err := readITKMatlab(r, 2)
	if err != nil {
		return nil, err
	}
	return assembleITK2D(rs, false)
}

// ReadITKHDF52D reads native 2D HDF5 records using the existing bounded reader.
func ReadITKHDF52D(r io.ReaderAt, size int64) ([]Transform2D, error) {
	rs, c, err := readITKHDF5(r, size, 2)
	if err != nil {
		return nil, err
	}
	return assembleITK2D(rs, c)
}

// WriteITKText2D writes double_2_2 records, reversing component application
// order once into ITK queue order. Empty/nested composites are refused.
func WriteITKText2D(w io.Writer, t Transform2D) error {
	if !builtin2D(t, true) || w == nil {
		return fmt.Errorf("%w: nil transform/writer", ErrTransformFile)
	}
	ts := []Transform2D{t}
	if c, ok := t.(*Composite2D); ok {
		if c == nil || c.Len() == 0 || c.Len()+1 > maxHDF5Transforms {
			return fmt.Errorf("%w: empty/oversized 2D composite", ErrTransformFile)
		}
		for i := c.Len() - 1; i >= 0; i-- {
			ts = append(ts, c.Transform(i))
		}
	}
	var b strings.Builder
	b.WriteString(ITKTextHeader + "\n")
	for i, t := range ts {
		name, err := itkTypeName2D(t)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrTransformFile, err)
		}
		fmt.Fprintf(&b, "#Transform %d\nTransform: %s\n", i, name)
		if t.Kind() == KindComposite {
			if i != 0 {
				return fmt.Errorf("%w: nested composite", ErrTransformFile)
			}
			continue
		}
		if !finiteSlice(t.Parameters()) || !finiteSlice(t.FixedParameters()) {
			return fmt.Errorf("%w: non-finite parameters", ErrTransformFile)
		}
		fmt.Fprintf(&b, "Parameters: %s\nFixedParameters: %s\n", formatITKVector(t.Parameters()), formatITKVector(t.FixedParameters()))
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// WriteITKMatlab2D writes one linear 2D transform as little-endian double.
func WriteITKMatlab2D(w io.Writer, t Transform2D) error {
	if !builtin2D(t, false) || w == nil {
		return fmt.Errorf("%w: MATLAB requires one 2D linear transform", ErrTransformFile)
	}
	name, err := itkTypeName2D(t)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrTransformFile, err)
	}
	var b bytes.Buffer
	for _, v := range []struct {
		name   string
		values []float64
	}{{name, t.Parameters()}, {"fixed", t.FixedParameters()}} {
		if len(v.values) > 64 || !finiteSlice(v.values) {
			return fmt.Errorf("%w: invalid vector", ErrTransformFile)
		}
		for _, n := range []int32{0, int32(len(v.values)), 1, 0, int32(len(v.name) + 1)} {
			_ = binary.Write(&b, binary.LittleEndian, n)
		}
		b.WriteString(v.name)
		b.WriteByte(0)
		for _, x := range v.values {
			_ = binary.Write(&b, binary.LittleEndian, x)
		}
	}
	n, err := w.Write(b.Bytes())
	if err == nil && n != b.Len() {
		err = io.ErrShortWrite
	}
	return err
}
