package mesh

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Material contains declared classic Wavefront material parameters. Texture
// references are inspection data only: readers never resolve paths or URLs.
type Material struct {
	Name                                 string
	Ambient, Diffuse, Specular, Emissive [3]float32
	Opacity, Shininess, OpticalDensity   float64
	Illumination                         int
	TextureReferences                    []MaterialResource
	Declared                             []string
}
type MaterialResource struct{ Directive, Arguments string }

func ReadMTL(ctx context.Context, r io.Reader, options ReadOptions) ([]Material, error) {
	o, err := options.normalized(false)
	if err != nil {
		return nil, err
	}
	data, err := modelInput(ctx, r, o)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%w: MTL must be UTF-8 text", ErrInvalidModel)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, min(1024, o.MaxLineBytes+2)), o.MaxLineBytes+2)
	var out []Material
	seen := map[string]bool{}
	var current *Material
	stringsBytes := int64(0)
	for scanner.Scan() {
		if len(scanner.Bytes()) > o.MaxLineBytes {
			return nil, ErrModelLimit
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		line := strings.TrimSpace(scanner.Text())
		if pos := strings.IndexByte(line, '#'); pos >= 0 {
			line = strings.TrimSpace(line[:pos])
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "newmtl" {
			name, err := modelName(strings.TrimSpace(strings.TrimPrefix(line, "newmtl")), o)
			if err != nil {
				return nil, err
			}
			if seen[name] {
				return nil, fmt.Errorf("%w: duplicate material %q", ErrInvalidModel, name)
			}
			if len(out) >= o.MaxMaterials {
				return nil, ErrModelLimit
			}
			seen[name] = true
			out = append(out, Material{Name: name, Opacity: 1, OpticalDensity: 1})
			current = &out[len(out)-1]
			stringsBytes += int64(len(name))
		} else {
			if current == nil {
				return nil, fmt.Errorf("%w: material properties before newmtl", ErrInvalidModel)
			}
			for _, key := range current.Declared {
				if key == f[0] || (key == "d" && f[0] == "Tr") || (key == "Tr" && f[0] == "d") {
					return nil, fmt.Errorf("%w: duplicate MTL property %s", ErrInvalidModel, f[0])
				}
			}
			current.Declared = append(current.Declared, f[0])
			stringsBytes += int64(len(f[0]))
			switch f[0] {
			case "Ka", "Kd", "Ks", "Ke":
				rgb, err := modelPoint(f[1:], 1)
				if err != nil {
					return nil, err
				}
				for _, value := range rgb {
					if value < 0 || value > 1 {
						return nil, fmt.Errorf("%w: RGB material channel outside [0,1]", ErrInvalidModel)
					}
				}
				switch f[0] {
				case "Ka":
					current.Ambient = rgb
				case "Kd":
					current.Diffuse = rgb
				case "Ks":
					current.Specular = rgb
				case "Ke":
					current.Emissive = rgb
				}
			case "d", "Tr", "Ns", "Ni":
				if len(f) != 2 {
					return nil, fmt.Errorf("%w: unsupported MTL scalar syntax", ErrUnsupportedModel)
				}
				value, err := modelNumber(f[1])
				if err != nil {
					return nil, err
				}
				switch f[0] {
				case "d", "Tr":
					if value < 0 || value > 1 {
						return nil, ErrInvalidModel
					}
					if f[0] == "Tr" {
						value = 1 - value
					}
					current.Opacity = value
				case "Ns":
					if value < 0 || value > 1000 {
						return nil, ErrInvalidModel
					}
					current.Shininess = value
				case "Ni":
					if value <= 0 || value > 10 {
						return nil, ErrInvalidModel
					}
					current.OpticalDensity = value
				}
			case "illum":
				if len(f) != 2 {
					return nil, ErrInvalidModel
				}
				value, err := strconv.Atoi(f[1])
				if err != nil || value < 0 || value > 10 {
					return nil, ErrInvalidModel
				}
				current.Illumination = value
			case "map_Ka", "map_Kd", "map_Ks", "map_Ke", "map_Ns", "map_d", "bump", "map_bump", "disp", "decal", "refl":
				args, err := modelName(strings.TrimSpace(strings.TrimPrefix(line, f[0])), o)
				if err != nil {
					return nil, err
				}
				current.TextureReferences = append(current.TextureReferences, MaterialResource{Directive: f[0], Arguments: args})
				stringsBytes += int64(len(args) + 32)
			default:
				return nil, fmt.Errorf("%w: MTL directive %s", ErrUnsupportedModel, f[0])
			}
		}
		if int64(len(out))*256+stringsBytes > o.MaxOutputBytes {
			return nil, ErrModelLimit
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: MTL line exceeds capacity", ErrModelLimit)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: empty material library", ErrInvalidModel)
	}
	return out, nil
}
