package fhir

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type searchBundle struct {
	ResourceType string `json:"resourceType"`
	Type         string `json:"type"`
	Links        []struct {
		Relation string `json:"relation"`
		URL      string `json:"url"`
	} `json:"link"`
	Entries []struct {
		Search *struct {
			Mode string `json:"mode"`
		} `json:"search"`
		Resource json.RawMessage `json:"resource"`
	} `json:"entry"`
}

func decodePage(ctx context.Context, raw []byte) ([]ImagingStudy, string, error) {
	if err := checkJSON(ctx, raw); err != nil {
		return nil, "", err
	}
	var bundle searchBundle
	if err := json.Unmarshal(raw, &bundle); err != nil || bundle.ResourceType != "Bundle" || bundle.Type != "searchset" {
		return nil, "", ErrResponse
	}
	var next string
	for _, link := range bundle.Links {
		if link.Relation != "next" {
			continue
		}
		if next != "" || link.URL == "" {
			return nil, "", ErrResponse
		}
		next = link.URL
	}

	var studies []ImagingStudy
	for _, entry := range bundle.Entries {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if entry.Search != nil {
			switch entry.Search.Mode {
			case "include", "outcome":
				continue
			case "", "match":
			default:
				return nil, "", ErrResponse
			}
		}
		var study ImagingStudy
		if err := json.Unmarshal(entry.Resource, &study); err != nil ||
			study.ResourceType != imagingStudyType || !validDateTime(study.Started) {
			return nil, "", ErrResponse
		}
		for _, series := range study.Series {
			if !validDateTime(series.Started) {
				return nil, "", ErrResponse
			}
		}
		studies = append(studies, study)
	}
	return studies, next, nil
}

var preciseDateTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-]((0[0-9]|1[0-3]):[0-5][0-9]|14:00))$`)

func validDateTime(value string) bool {
	if value == "" { // Optional FHIR element.
		return true
	}
	if len(value) < 4 || value[:4] == "0000" {
		return false
	}
	var format string
	switch len(value) {
	case 4:
		format = "2006"
	case 7:
		format = "2006-01"
	case 10:
		format = "2006-01-02"
	default:
		if !preciseDateTime.MatchString(value) {
			return false
		}
		// time.Parse rejects leap seconds and truncates fractions after
		// nanoseconds. Preserve the source string and only check its shape.
		if value[17:19] == "60" {
			value = value[:17] + "59" + value[19:]
		}
		format = time.RFC3339Nano
	}
	_, err := time.Parse(format, value)
	return err == nil
}

func checkJSON(ctx context.Context, raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrResponse
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	remaining := 500_000
	if err := scanJSONValue(ctx, decoder, 0, &remaining); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrResponse
	}
	return nil
}

func scanJSONValue(ctx context.Context, decoder *json.Decoder, depth int, remaining *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 64 || *remaining == 0 {
		return ErrLimit
	}
	*remaining--
	value, err := decoder.Token()
	if err != nil {
		return ErrResponse
	}
	delim, ok := value.(json.Delim)
	if !ok {
		return nil
	}
	if delim == '{' {
		keys := make(map[string]struct{})
		for decoder.More() {
			token, err := decoder.Token()
			key, ok := token.(string)
			if err != nil || !ok || len(key) > 256 || !isASCIIString(key) {
				return ErrResponse
			}
			// Go struct decoding is case-insensitive, so a case variant
			// must not override an earlier field such as a pagination link.
			// FHIR property names use ASCII.
			key = strings.ToUpper(key)
			if _, repeated := keys[key]; repeated {
				return ErrResponse
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(ctx, decoder, depth+1, remaining); err != nil {
				return err
			}
		}
	} else if delim == '[' {
		for decoder.More() {
			if err := scanJSONValue(ctx, decoder, depth+1, remaining); err != nil {
				return err
			}
		}
	} else {
		return ErrResponse
	}
	if _, err := decoder.Token(); err != nil {
		return ErrResponse
	}
	return nil
}
