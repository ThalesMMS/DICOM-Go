// Package htj2k implements the HT (high-throughput) block coder of ISO/IEC
// 15444-15: the cleanup pass that codes one JPEG 2000 code-block with a MEL
// run-length coder, context-dependent variable-length codes (VLC) and raw
// magnitude/sign bits (MagSgn), instead of the MQ arithmetic coder of Part 1.
//
// Only the HT cleanup pass is implemented. A code-block is coded completely by
// one cleanup pass at bit-plane zero (the coded magnitude is the whole
// quantization index), which is how a lossless or a non-truncated lossy
// encoder uses HTJ2K; the HT SigProp and MagRef refinement passes are not
// produced and are refused by the decoder.
//
// The package is written from the description of the block coder in ISO/IEC
// 15444-15 (ITU-T T.814). Only the CxtVLC codebook (vlc_codebook.go) is
// normative data obtained from another implementation; see that file.
package htj2k

import "errors"

// ErrMalformed reports an HT cleanup segment that cannot be decoded.
var ErrMalformed = errors.New("htj2k: malformed HT cleanup segment")

// ErrUnsupported reports a valid block the coder does not implement.
var ErrUnsupported = errors.New("htj2k: unsupported HT code-block")

// MaxScup is the largest MEL+VLC suffix length a cleanup segment can signal
// (ISO/IEC 15444-15: Scup is 12 bits and at most 4079).
const MaxScup = 4079

// vlcRecord is one codeword of the CxtVLC codebook.
type vlcRecord struct {
	context, rho, uOff, ek, e1, codeword, length uint8
}

// vlcCode is the codeword an encoder emits for a quad.
type vlcCode struct {
	codeword, length, ek, e1 uint8
	ok                       bool
}

// vlcSymbol is what a decoder learns from a codeword.
type vlcSymbol struct {
	rho, uOff, ek, e1, length uint8
}

var (
	// encodeTables[row][context][rho][emb]: row 0 is the initial quad
	// row, emb the samples whose exponent equals the quad's bound U.
	encodeTables [2][8][16][16]vlcCode
	// decodeTables[row][context][next 7 bits]: the symbol whose codeword
	// is a prefix of those bits; length 0 marks an invalid pattern.
	decodeTables [2][8][128]vlcSymbol
)

func init() {
	for row, records := range [2][]vlcRecord{vlcInitial[:], vlcNonInitial[:]} {
		for _, r := range records {
			for bits := 0; bits < 128; bits++ {
				if uint8(bits)&(1<<r.length-1) == r.codeword {
					decodeTables[row][r.context][bits] = vlcSymbol{rho: r.rho, uOff: r.uOff, ek: r.ek, e1: r.e1, length: r.length}
				}
			}
			// Every EMB pattern this codeword can stand for: the samples it
			// marks as known (e_k) must carry the bit it states (e_1), which is
			// one exactly for the samples at the bound.
			for emb := uint8(0); emb < 16; emb++ {
				if emb&^r.rho != 0 || (emb != 0) != (r.uOff != 0) || r.ek&emb != r.e1 {
					continue
				}
				current := &encodeTables[row][r.context][r.rho][emb]
				if !current.ok || cheaper(r, *current) {
					*current = vlcCode{codeword: r.codeword, length: r.length, ek: r.ek, e1: r.e1, ok: true}
				}
			}
		}
	}
}

// cheaper prefers the codeword that spends the fewest bits in total: its own
// length less the MagSgn bits its known samples save.
func cheaper(r vlcRecord, current vlcCode) bool {
	cost := int(r.length) - popcount4(r.ek)
	currentCost := int(current.length) - popcount4(current.ek)
	if cost != currentCost {
		return cost < currentCost
	}
	return r.length < current.length
}

func popcount4(v uint8) int {
	return int(v&1 + v>>1&1 + v>>2&1 + v>>3&1)
}

// melExponent is the MEL coder's run-length exponent for each of its 13
// states.
var melExponent = [13]uint8{0, 0, 0, 1, 1, 1, 2, 2, 2, 3, 3, 4, 5}

// exponent is E for a magnitude: the bit length of 2*mu-1, or zero for an
// insignificant sample.
func exponent(mu uint32) uint8 {
	if mu == 0 {
		return 0
	}
	v := 2*uint64(mu) - 1
	n := uint8(0)
	for v != 0 {
		n++
		v >>= 1
	}
	return n
}
