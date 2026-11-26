// Package encaplayout plans still-image encapsulation without retaining pixels.
// Both materialized transcodes and the streaming frame writer use this layout.
package encaplayout

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

type Policy uint8

const (
	Auto Policy = iota
	Basic
	Extended
	Empty
)

const MaxItem = uint64(math.MaxUint32 - 1)

var ErrFragmentLimit = errors.New("fragment count exceeds limit")

type Layout struct {
	Basic, Extended, Lengths []byte
	Fragments                int
}

// Plan counts headers and even padding in offsets. maxFragment is zero for
// one Item per frame, otherwise an even Item payload cap. maxFragments=0 has
// no caller cap. Extended tables can only index single-fragment frames.
func Plan(lengths []uint64, maxFragment uint64, policy Policy, maxFragments int, single bool) (Layout, error) {
	var result Layout
	if policy > Empty || maxFragments < 0 || maxFragment != 0 && (maxFragment < 2 || maxFragment%2 != 0 || maxFragment > MaxItem) {
		return result, fmt.Errorf("invalid encapsulation options")
	}
	offsets := make([]uint64, len(lengths))
	var next uint64
	fits, split := true, false
	for i, length := range lengths {
		if length == 0 {
			return result, fmt.Errorf("frame %d is empty", i+1)
		}
		cap := maxFragment
		if cap == 0 {
			cap = MaxItem
		}
		count := (length-1)/cap + 1
		if maxFragment == 0 && count > 1 || single && count > 1 {
			return result, fmt.Errorf("frame %d must fit one Fragment", i+1)
		}
		if count > uint64(math.MaxInt-result.Fragments) || maxFragments > 0 && count > uint64(maxFragments-result.Fragments) {
			return result, ErrFragmentLimit
		}
		result.Fragments += int(count)
		split = split || count > 1
		offsets[i] = next
		fits = fits && next <= math.MaxUint32
		if length == math.MaxUint64 || count > (math.MaxUint64-length-length%2)/8 {
			return result, fmt.Errorf("encapsulated length overflow")
		}
		span := length + length%2 + 8*count
		if next > math.MaxUint64-span {
			return result, fmt.Errorf("encapsulated offsets overflow")
		}
		next += span
	}
	if policy == Empty {
		return result, nil
	}
	if policy == Basic && !fits {
		return result, fmt.Errorf("frame offsets exceed Basic Offset Table")
	}
	if policy == Extended || !fits {
		if split {
			return result, fmt.Errorf("Extended Offset Table requires one Fragment per frame")
		}
		result.Extended, result.Lengths = make([]byte, 8*len(lengths)), make([]byte, 8*len(lengths))
		for i, offset := range offsets {
			binary.LittleEndian.PutUint64(result.Extended[8*i:], offset)
			binary.LittleEndian.PutUint64(result.Lengths[8*i:], lengths[i])
		}
		return result, nil
	}
	result.Basic = make([]byte, 4*len(lengths))
	for i, offset := range offsets {
		binary.LittleEndian.PutUint32(result.Basic[4*i:], uint32(offset))
	}
	return result, nil
}

// SingleFragment reports syntaxes whose frames must occupy one Fragment:
// RLE (PS3.5 A.4.2), Encapsulated Uncompressed (A.4.11), Deflated Image
// Frame (A.4.13), HTJ2K (A.4.4) and JPEG XL (A.4.12).
func SingleFragment(uid string) bool {
	switch uid {
	case "1.2.840.10008.1.2.5", "1.2.840.10008.1.2.1.98", "1.2.840.10008.1.2.8.1",
		"1.2.840.10008.1.2.4.201", "1.2.840.10008.1.2.4.202", "1.2.840.10008.1.2.4.203",
		"1.2.840.10008.1.2.4.110", "1.2.840.10008.1.2.4.111", "1.2.840.10008.1.2.4.112":
		return true
	}
	return false
}

// FragmentLength selects the next unpadded payload length under the cap.
// Avoid a one/two-byte terminal Fragment when cap>=4: moving two bytes from
// the penultimate Fragment keeps that Item even and leaves a final payload
// of three/four bytes. JPEG-family end markers (possibly followed by one pad
// byte on a repack) then stay together for readers that inspect terminal Items.
// The number of Items and offsets planned above are unchanged.
func FragmentLength(remaining, cap uint64) uint64 {
	if cap == 0 || remaining <= cap {
		return remaining
	}
	if cap >= 4 && remaining-cap <= 2 {
		return cap - 2
	}
	return cap
}
