package pixeldata

import (
	"math/bits"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DiagnosticBitCountMasks: Bits Allocated, Bits Stored or High Bit held a bit
// mask instead of a count (0xFFFF for 16, 0xFFFE for High Bit 15), as some
// FUJIFILM CR devices write; DecodeRecovery.RepairBitCountMasks replaced it.
const DiagnosticBitCountMasks = "bit-count-masks-repaired"

// DiagnosticHighBitZero: High Bit was 0 with Bits Stored above 1, as the
// MFSPLIT export of a Siemens MAGNETOM Skyra writes;
// DecodeRecovery.RepairBitCountMasks replaced it by Bits Stored minus one.
const DiagnosticHighBitZero = "high-bit-zero-repaired"

// maskCount returns n when v is the mask 2^n-1 of a count no attribute can
// hold as such (above 16), n being at most 16.
func maskCount(v uint16) (uint16, bool) {
	if v <= 16 || bits.OnesCount16(v+1) != 1 && v != 0xffff {
		return 0, false
	}
	return uint16(bits.OnesCount16(v)), true
}

// highBitMask returns n-1 when v is 2^n-2, the High Bit such writers derive
// from the mask as Bits Stored minus one.
func highBitMask(v uint16) (uint16, bool) {
	if v <= 15 || v%2 != 0 {
		return 0, false
	}
	n, ok := maskCount(v + 1)
	if !ok {
		return 0, false
	}
	return n - 1, true
}

// withBitCountMaskRepair returns dataset, or a copy with Bits Allocated, Bits
// Stored and High Bit written as masks replaced by counts, and a High Bit of
// 0 with Bits Stored above 1 replaced by Bits Stored minus one, when the
// results are consistent: Bits Stored at most Bits Allocated, High Bit Bits
// Stored minus one, Bits Allocated 8 or 16 (or 32 when no mask was
// replaced), and native Pixel Data of exactly the size they give (one pad
// byte allowed). It returns the replacements and their diagnostics. The
// input is never mutated.
func withBitCountMaskRepair(dataset *object.Object, source transfer.Syntax) (*object.Object, []MetadataReconciliation, []string) {
	allocated, okAllocated := getUint16(dataset, tagBitsAllocated)
	stored, okStored := getUint16(dataset, tagBitsStored)
	high, okHigh := getUint16(dataset, tagHighBit)
	if !okAllocated || !okStored || !okHigh {
		return dataset, nil, nil
	}
	var reconciled []MetadataReconciliation
	replace := func(name string, tag core.Tag, declared, used uint16) {
		reconciled = append(reconciled, MetadataReconciliation{Attribute: name, Tag: tag, Declared: strconv.Itoa(int(declared)), Used: strconv.Itoa(int(used))})
	}
	newAllocated, newStored, newHigh := allocated, stored, high
	if n, ok := maskCount(allocated); ok {
		newAllocated = n
		replace("BitsAllocated", tagBitsAllocated, allocated, n)
	}
	if n, ok := maskCount(stored); ok {
		newStored = n
		replace("BitsStored", tagBitsStored, stored, n)
	}
	if n, ok := highBitMask(high); ok {
		newHigh = n
		replace("HighBit", tagHighBit, high, n)
	}
	masked := len(reconciled) > 0
	highZero := high == 0 && newStored > 1
	if highZero {
		newHigh = newStored - 1
		replace("HighBit", tagHighBit, high, newHigh)
	}
	allocatedOK := newAllocated == 8 || newAllocated == 16 || newAllocated == 32 && !masked
	if len(reconciled) == 0 || !allocatedOK || newStored == 0 || newStored > newAllocated || newHigh != newStored-1 {
		return dataset, nil, nil
	}
	var diagnostics []string
	if masked {
		diagnostics = append(diagnostics, DiagnosticBitCountMasks)
	}
	if highZero {
		diagnostics = append(diagnostics, DiagnosticHighBitZero)
	}
	order := dataset.ValueByteOrder()
	elements := dataset.Elements()
	elements = replaceElement(elements, uint16Element(tagBitsAllocated, newAllocated, order))
	elements = replaceElement(elements, uint16Element(tagBitsStored, newStored, order))
	elements = replaceElement(elements, uint16Element(tagHighBit, newHigh, order))
	out := object.FromElements(elements, nil)
	out.SetValueByteOrder(order)
	pixel, err := ExtractView(out)
	if err != nil {
		return dataset, nil, nil
	}
	if !pixel.Encapsulated && !source.Encapsulated {
		metadata, err := ExtractMetadata(out)
		if err != nil {
			return dataset, nil, nil
		}
		total, size := metadata.TotalSize(), int64(len(pixel.Raw))
		if total <= 0 || size != total && !(total%2 == 1 && size == total+1) {
			return dataset, nil, nil
		}
	}
	return out, reconciled, diagnostics
}
