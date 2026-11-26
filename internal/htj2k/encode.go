package htj2k

import "fmt"

// MaxMagnitude bounds the magnitude of a sample EncodeCleanup accepts; it
// keeps every exponent and MagSgn word within 32 bits.
const MaxMagnitude = 1<<30 - 1

// quad is one 2x2 group of samples, numbered 0 (top left), 1 (bottom left),
// 2 (top right) and 3 (bottom right).
type quad struct {
	rho   uint8     // significance pattern
	e     [4]uint8  // exponent of each sample
	v     [4]uint32 // MagSgn value of each sample: 2(mu-1) + sign
	emax  uint8
	u     uint32 // U minus kappa
	bound uint8  // U, the exponent bound
	code  vlcCode
}

// EncodeCleanup codes a width x height code-block as one HT cleanup pass at
// bit-plane zero. samples holds the signed quantization indices in raster
// order; at least one must be nonzero and none may exceed MaxMagnitude. The
// result is the cleanup segment, Lcup bytes long.
func EncodeCleanup(samples []int32, width, height int) ([]byte, error) {
	if width <= 0 || height <= 0 || width > 1024 || height > 1024 || width*height > 4096 || len(samples) != width*height {
		return nil, fmt.Errorf("%w: %dx%d code-block with %d samples", ErrUnsupported, width, height, len(samples))
	}
	nonzero := false
	for _, s := range samples {
		if s > MaxMagnitude || s < -MaxMagnitude {
			return nil, fmt.Errorf("%w: sample magnitude above 2^30", ErrUnsupported)
		}
		nonzero = nonzero || s != 0
	}
	if !nonzero {
		return nil, fmt.Errorf("%w: a code-block without significant samples is not coded", ErrUnsupported)
	}

	quadCols := (width + 1) / 2
	quadRows := (height + 1) / 2
	at := func(x, y int) int32 {
		if x < 0 || y < 0 || x >= width || y >= height {
			return 0
		}
		return samples[y*width+x]
	}
	// Exponents of the row above the current quad row, with a guard
	// column on each side.
	above := make([]uint8, 2*quadCols+3)
	quads := make([]quad, quadCols)

	ms := magSgnWriter{max: 8}
	mel := newMELWriter()
	vlc := newVLCWriter()

	for qy := 0; qy < quadRows; qy++ {
		initial := qy == 0
		row := 0
		if !initial {
			row = 1
		}
		for qx := range quads {
			q := &quads[qx]
			*q = quad{}
			for i := 0; i < 4; i++ {
				s := at(2*qx+i>>1, 2*qy+i&1)
				if s == 0 {
					continue
				}
				mu := uint32(s)
				sign := uint32(0)
				if s < 0 {
					mu, sign = uint32(-s), 1
				}
				q.rho |= 1 << i
				q.e[i] = exponent(mu)
				q.v[i] = 2*(mu-1) + sign
				if q.e[i] > q.emax {
					q.emax = q.e[i]
				}
			}
		}

		for qx := 0; qx < quadCols; qx += 2 {
			pair := 1
			if qx+1 < quadCols {
				pair = 2
			}
			for k := 0; k < pair; k++ {
				c := qx + k
				q := &quads[c]
				context := quadContext(quads, above, c, initial)
				kappa := uint8(1)
				if !initial && q.rho&(q.rho-1) != 0 {
					m := max(above[2*c], above[2*c+1], above[2*c+2], above[2*c+3])
					if m > 2 {
						kappa = m - 1
					}
				}
				q.bound = max(kappa, q.emax)
				q.u = uint32(q.bound - kappa)
				emb := uint8(0)
				if q.u > 0 {
					for i := 0; i < 4; i++ {
						if q.rho>>i&1 != 0 && q.e[i] == q.bound {
							emb |= 1 << i
						}
					}
				}
				if context == 0 {
					mel.event(q.rho != 0)
					if q.rho == 0 {
						continue
					}
				}
				q.code = encodeTables[row][context][q.rho][emb]
				if !q.code.ok {
					return nil, fmt.Errorf("%w: no codeword for context %d rho %#x emb %#x", ErrMalformed, context, q.rho, emb)
				}
				vlc.put(uint32(q.code.codeword), uint(q.code.length))
			}

			a := &quads[qx]
			var b *quad
			if pair == 2 {
				b = &quads[qx+1]
			}
			if err := putUValues(&vlc, &mel, a, b, initial); err != nil {
				return nil, err
			}
			for k := 0; k < pair; k++ {
				q := &quads[qx+k]
				for i := 0; i < 4; i++ {
					if q.rho>>i&1 == 0 {
						continue
					}
					m := uint(q.bound) - uint(q.code.ek>>i&1)
					ms.put(q.v[i], m)
				}
			}
		}

		// The exponents of this quad row's bottom row feed the next one.
		for x := -1; x <= 2*quadCols+1; x++ {
			above[x+1] = exponent(magnitude(at(x, 2*qy+1)))
		}
	}

	magsgn := ms.finish()
	melBytes := mel.finish()
	vlcBytes := vlc.finish()
	scup := len(melBytes) + len(vlcBytes)
	if scup > MaxScup {
		return nil, fmt.Errorf("%w: MEL and VLC need %d bytes, more than %d", ErrUnsupported, scup, MaxScup)
	}
	segment := make([]byte, 0, len(magsgn)+scup)
	segment = append(segment, magsgn...)
	segment = append(segment, melBytes...)
	segment = append(segment, vlcBytes...)
	n := len(segment)
	segment[n-1] = byte(scup >> 4)
	segment[n-2] = segment[n-2]&0xF0 | byte(scup&0x0F)
	return segment, nil
}

func magnitude(s int32) uint32 {
	if s < 0 {
		return uint32(-s)
	}
	return uint32(s)
}

// quadContext is the VLC context of quad c. In the initial row it depends on
// the quad to the left; in later rows on the row above (from one column left
// of the quad to one column right of it) and on the right column of the quad
// to the left.
func quadContext(quads []quad, above []uint8, c int, initial bool) uint8 {
	if initial {
		if c == 0 {
			return 0
		}
		left := quads[c-1].rho
		return left>>1 | left&1
	}
	sig := func(i int) uint8 { // above[i] covers column i-1
		if above[i] != 0 {
			return 1
		}
		return 0
	}
	context := sig(2*c) | sig(2*c+1) | (sig(2*c+2)|sig(2*c+3))<<2
	if c > 0 {
		left := quads[c-1].rho
		if left&0xC != 0 {
			context |= 2
		}
	}
	return context
}

// putUValues writes the u values of a quad pair after its codewords. In the
// initial row a pair whose quads both have one first spends a MEL symbol on
// whether both exceed two; the other cases code each u in turn, prefixes
// before suffixes.
func putUValues(vlc *vlcWriter, mel *melWriter, a, b *quad, initial bool) error {
	ua := uint32(0)
	if a.rho != 0 {
		ua = a.u
	}
	ub := uint32(0)
	if b != nil && b.rho != 0 {
		ub = b.u
	}
	if ua > 96 || ub > 96 {
		return fmt.Errorf("%w: u value above 96", ErrUnsupported)
	}
	if initial && ua > 0 && ub > 0 {
		both := ua > 2 && ub > 2
		mel.event(both)
		if both {
			vlc.putUPrefix(ua - 2)
			vlc.putUPrefix(ub - 2)
			vlc.putUSuffix(ua - 2)
			vlc.putUSuffix(ub - 2)
			return nil
		}
		vlc.putUPrefix(ua)
		if ua > 2 {
			vlc.put(ub-1, 1)
			vlc.putUSuffix(ua)
			return nil
		}
		vlc.putUPrefix(ub)
		vlc.putUSuffix(ua)
		vlc.putUSuffix(ub)
		return nil
	}
	if ua > 0 {
		vlc.putUPrefix(ua)
	}
	if ub > 0 {
		vlc.putUPrefix(ub)
	}
	if ua > 0 {
		vlc.putUSuffix(ua)
	}
	if ub > 0 {
		vlc.putUSuffix(ub)
	}
	return nil
}
