package registration

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/render"
)

const (
	mincTransformHeader = "MNI Transform File"
	maxMINCFileBytes    = 16 << 20
	maxMINCTokenBytes   = 256
)

// mincTransformBody looks for the exact header line. An optional UTF-8 BOM
// and leading ASCII whitespace are tolerated as in the ITK text reader.
func mincTransformBody(data []byte) ([]byte, bool) {
	data = bytes.TrimLeft(data, " \t\r\n")
	data = bytes.TrimLeft(bytes.TrimPrefix(data, []byte(utf8BOM)), " \t\r\n")
	if !bytes.HasPrefix(data, []byte(mincTransformHeader)) {
		return nil, false
	}
	header, body, _ := bytes.Cut(data, []byte("\n"))
	return body, string(bytes.TrimRight(header, " \t\r")) == mincTransformHeader
}

func readMINCTransform(r io.Reader, limit int) (Transform, error) {
	transform, err := parseMINCTransform(mincTokens{bufio.NewReader(r)}, limit)
	if err != nil {
		return nil, fmt.Errorf("%w: MINC XFM: %w", ErrTransformFile, err)
	}
	return transform, nil
}

func parseMINCTransform(tokens mincTokens, limit int) (Transform, error) {
	const parameters = 12
	var transforms []Transform
	for {
		tag, err := tokens.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if tag != "Transform_Type" {
			return nil, fmt.Errorf("expected Transform_Type, got %q", tag)
		}
		if len(transforms) == maxHDF5Transforms {
			return nil, fmt.Errorf("transform count exceeds %d", maxHDF5Transforms)
		}
		if limit < parameters {
			return nil, fmt.Errorf("total parameter budget")
		}
		t, err := parseMINCLinear(tokens)
		if err != nil {
			return nil, fmt.Errorf("component %d: %w", len(transforms)+1, err)
		}
		transforms = append(transforms, t)
		limit -= parameters
	}
	switch len(transforms) {
	case 0:
		return nil, fmt.Errorf("no transform")
	case 1:
		return transforms[0], nil
	}

	// As with libminc, apply the records in file order.
	chain, err := NewComposite(transforms...)
	if err != nil {
		return nil, err
	}
	matrix, _, err := chain.linear()
	if err != nil {
		return nil, err
	}
	if _, err := matrix.Inverse(); err != nil {
		return nil, fmt.Errorf("composed matrix: %w", err)
	}
	return chain, nil
}

func parseMINCLinear(tokens mincTokens) (*Affine, error) {
	if err := tokens.expect("="); err != nil {
		return nil, err
	}
	kind, err := tokens.required()
	if err != nil {
		return nil, err
	}
	if kind != "Linear" {
		return nil, fmt.Errorf("%w: MINC Transform_Type %q", ErrUnsupportedTransform, kind)
	}
	if err := tokens.expect(";"); err != nil {
		return nil, err
	}
	next, err := tokens.required()
	if err != nil {
		return nil, err
	}
	var invert bool
	if next == "Invert_Flag" {
		if err := tokens.expect("="); err != nil {
			return nil, err
		}
		flag, err := tokens.required()
		if err != nil {
			return nil, err
		}
		if flag != "True" && flag != "False" {
			return nil, fmt.Errorf("invalid Invert_Flag %q", flag)
		}
		invert = flag == "True"
		if err := tokens.expect(";"); err != nil {
			return nil, err
		}
		next, err = tokens.required()
		if err != nil {
			return nil, err
		}
	}
	if next != "Linear_Transform" {
		return nil, fmt.Errorf("expected Linear_Transform, got %q", next)
	}
	if err := tokens.expect("="); err != nil {
		return nil, err
	}
	var values [3][4]float64
	for row := range values {
		for column := range values[row] {
			token, err := tokens.required()
			if err != nil {
				return nil, err
			}
			value, err := strconv.ParseFloat(token, 64)
			if err != nil || !finite(value) || strings.ContainsRune(token, '_') {
				return nil, fmt.Errorf("matrix value %q is not finite numeric input", token)
			}
			values[row][column] = value
		}
	}
	if err := tokens.expect(";"); err != nil {
		return nil, err
	}

	// Both the source and target coordinates switch RAS to LPS. The last
	// column is an offset in millimetres, and changes sign only by row.
	flip := [3]float64{-1, -1, 1}
	var matrix Matrix3
	for row := range matrix {
		for column := range matrix[row] {
			matrix[row][column] = flip[row] * values[row][column] * flip[column]
		}
	}
	offset := render.Vec3{X: -values[0][3], Y: -values[1][3], Z: values[2][3]}
	inverse, err := matrix.Inverse()
	if err != nil {
		return nil, err
	}
	if invert {
		offset = inverse.MulVec(offset).Scale(-1)
		matrix = inverse
	}
	affine := NewAffine()
	affine.matrix = matrix
	if err := affine.SetOffset(offset); err != nil {
		return nil, err
	}
	return affine, nil
}

type mincTokens struct {
	*bufio.Reader
}

func (r mincTokens) required() (string, error) {
	token, err := r.next()
	if err == io.EOF {
		return "", io.ErrUnexpectedEOF
	}
	return token, err
}

func (r mincTokens) expect(want string) error {
	got, err := r.required()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("expected %q, got %q", want, got)
	}
	return nil
}

func (r mincTokens) next() (string, error) {
	var word [maxMINCTokenBytes]byte
	length := 0
	for {
		ch, err := r.ReadByte()
		if err != nil {
			if err == io.EOF && length != 0 {
				return string(word[:length]), nil
			}
			return "", err
		}
		switch ch {
		case ' ', '\t', '\r', '\n':
			if length != 0 {
				return string(word[:length]), nil
			}
		case '=', ';', '%', '#':
			if length != 0 {
				if err := r.UnreadByte(); err != nil {
					return "", err
				}
				return string(word[:length]), nil
			}
			if ch == '=' || ch == ';' {
				return string(ch), nil
			}
			// ReadSlice discards one buffer at a time even for long comments.
			for {
				_, err := r.ReadSlice('\n')
				if err == bufio.ErrBufferFull {
					continue
				}
				if err != nil {
					return "", err
				}
				break
			}
		default:
			if length == len(word) {
				return "", fmt.Errorf("token exceeds %d bytes", len(word))
			}
			word[length] = ch
			length++
		}
	}
}
