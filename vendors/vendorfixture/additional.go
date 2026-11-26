package vendorfixture

import (
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// AdditionalValue is invented metadata shared by the new decoder/consumer tests.
const AdditionalValue = "Synthetic^MEC"

// PhilipsSDSBlob constructs a v8 directory with a uint32 and string field.
// Offsets and byte layout are authored independently of the decoders.
func PhilipsSDSBlob() []byte {
	raw := make([]byte, 32+100+4+81)
	binary.LittleEndian.PutUint32(raw[24:], 8)
	binary.LittleEndian.PutUint32(raw[28:], 2)
	for i, name := range []string{"AcquisitionCount", "ScanLabel"} {
		start := 32 + i*50
		copy(raw[start:], name)
		typ := uint32(4)
		target := 132
		if i == 1 {
			typ = 2
			target = 136
		}
		binary.LittleEndian.PutUint32(raw[start+34:], typ)
		binary.LittleEndian.PutUint32(raw[start+38:], 1)
		binary.LittleEndian.PutUint32(raw[start+46:], uint32(target-start-46))
	}
	binary.LittleEndian.PutUint32(raw[132:], 42)
	copy(raw[136:], "Synthetic Scan")
	return raw
}

// EmbeddedHeaderData constructs a raw explicit VR little endian DICOM data
// set with an invented patient name, independently from the DICOM writer.
func EmbeddedHeaderData() []byte {
	value := []byte(AdditionalValue)
	if len(value)%2 != 0 {
		value = append(value, ' ')
	}
	raw := binary.LittleEndian.AppendUint16(nil, 0x0010)
	raw = binary.LittleEndian.AppendUint16(raw, 0x0010)
	raw = append(raw, 'P', 'N')
	raw = binary.LittleEndian.AppendUint16(raw, uint16(len(value)))
	return append(raw, value...)
}

// ToshibaCT3Blob constructs the two implicit UL wrapper fields plus raw data.
func ToshibaCT3Blob() []byte {
	data := EmbeddedHeaderData()
	var raw []byte
	for i, v := range []uint32{12, uint32(len(data))} {
		raw = binary.LittleEndian.AppendUint16(raw, 0)
		raw = binary.LittleEndian.AppendUint16(raw, uint16(i))
		raw = binary.LittleEndian.AppendUint32(raw, 4)
		raw = binary.LittleEndian.AppendUint32(raw, v)
	}
	return append(raw, data...)
}

// PMTFBlob reverses all the bytes in a raw explicit dataset.
func PMTFBlob() []byte {
	raw := EmbeddedHeaderData()
	for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}
	return raw
}

// MECMR3Blob constructs six groups, with UTF-8 text and uint32 typed records.
func MECMR3Blob() []byte {
	var raw []byte
	for group := 1; group <= 6; group++ {
		if group == 6 {
			raw = binary.LittleEndian.AppendUint32(raw, 1)
		}
		raw = binary.LittleEndian.AppendUint32(raw, 1)
		raw = binary.LittleEndian.AppendUint32(raw, uint32(group))
		typ := uint32(0xff000400)
		v := binary.LittleEndian.AppendUint32(nil, uint32(group))
		if group == 1 {
			typ = 0xff002c00
			v = append([]byte(AdditionalValue), 0)
		}
		raw = binary.LittleEndian.AppendUint32(raw, typ)
		raw = binary.LittleEndian.AppendUint32(raw, uint32(len(v)))
		for _, s := range []uint32{0, 0, 12, 0, 0} {
			raw = binary.LittleEndian.AppendUint32(raw, s)
		}
		raw = append(raw, v...)
	}
	return raw
}

// PutAdditional adds Philips SDS, CT3, Original Data, the three PMTF creators
// and Toshiba's second PMTF sequence, and UTF-8 PAS XML. It leaves Put's
// historical fixture unchanged. Every field is invented and contains no PHI.
func PutAdditional(o *object.Object) {
	put := func(o *object.Object, g, slot uint16, creator string, offset uint8, vr core.VR, raw []byte) {
		o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(g, slot), VR: core.VRLO}, Value: core.StringValue{creator}})
		o.Put(core.NewRawElement(core.NewTag(g, slot<<8|uint16(offset)), vr, raw))
	}
	put(o, 0x2005, 0x13, "Philips MR Imaging DD 002", 0x32, core.VROB, nil)
	item := object.New(nil)
	for _, f := range []struct {
		offset uint8
		value  string
	}{{0x37, "Synthetic protocol"}, {0x39, "SDS"}, {0x47, "1"}} {
		vr := core.VRLO
		if f.offset == 0x47 {
			vr = core.VRCS
		}
		put(item, 0x2005, 0x19, "Philips MR Imaging DD 002", f.offset, vr, []byte(f.value))
	}
	put(item, 0x2005, 0x19, "Philips MR Imaging DD 002", 0x44, core.VROB, PhilipsSDSBlob())
	o.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x2005, 0x1332), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: item.Elements()}}}})
	put(o, 0x7005, 0x17, "TOSHIBA_MEC_CT3", 0x10, core.VROB, ToshibaCT3Blob())
	put(o, 0x700d, 0x12, "TOSHIBA_MEC_MR3", 0x08, core.VROB, MECMR3Blob())
	for i, creator := range []string{"PMTF INFORMATION DATA", "CANON_MEC_MR3", "TOSHIBA_MEC_MR3"} {
		slot := uint16(0x11 + i)
		put(o, 0x0029, slot, creator, 1, core.VROB, nil)
		nested := object.New(nil)
		put(nested, 0x0029, 0x1e, creator, 0x90, core.VROB, PMTFBlob())
		el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0029, slot<<8|1), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: nested.Elements()}}}}
		o.Put(el)
		if creator == "TOSHIBA_MEC_MR3" {
			el.Header.Tag = core.NewTag(0x0029, slot<<8|2)
			o.Put(el)
		}
	}
	put(o, 0x700d, 0x1a, "CANON_MEC_MR3^10", 0x19, core.VROB, []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?><PAS><Label>Synthetic PAS</Label></PAS>"))
}
