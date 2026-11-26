// Package vendorfixture builds synthetic, PHI-free vendor private headers
// for tests of vendors.DecodePrivateHeaders and of the applications that show
// them: a Siemens CSA image and series header (with an ASCCONV MR protocol),
// a Siemens MedCom History, a GE Protocol Data Block, an ELSCINT1 protocol
// and VEPRO data. Every value is invented; the "names" are placeholders such
// as "Synthetic^Name".
package vendorfixture

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"unicode/utf16"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
)

// Values the fixture headers carry, for assertions.
const (
	CSAImageElementName  = "NumberOfImagesInMosaic"
	CSAImageElementValue = "18"
	MRProtocolKey        = "sProtConsistencyInfo.tBaselineString"
	MRProtocolValue      = `"N4_VE11C_LATEST"`
	MedComPatientName    = "Synthetic^Name"
	GEProtocolKey        = "SCANTYPE"
	GEProtocolValue      = "AXIAL"
	ElscintProtocolName  = "Chest"
	VEPROPatientName     = "Synthetic Vepro Name"
)

// Put adds every fixture header, with its private creator, to dataset.
func Put(dataset *object.Object) error {
	csaImage, err := siemens.EncodeCSA([]siemens.CSAElement{
		{Name: CSAImageElementName, VM: 1, VR: "US", SyngoDT: 10, Items: 6, Values: []string{CSAImageElementValue}},
		{Name: "SliceNormalVector", VM: 3, VR: "FD", SyngoDT: 4, Items: 6, Values: []string{"0", "0", "1"}},
	})
	if err != nil {
		return err
	}
	protocol := "### ASCCONV BEGIN ###\n" + MRProtocolKey + " = " + MRProtocolValue + "\nsKSpace.lBaseResolution = 256\n### ASCCONV END ###"
	csaSeries, err := siemens.EncodeCSA([]siemens.CSAElement{
		{Name: "MrPhoenixProtocol", VM: 1, VR: "UN", SyngoDT: 0, Items: 1, Values: []string{protocol}},
	})
	if err != nil {
		return err
	}
	medcom := utf16le("MEDCOM HISTORY V1.1\x00CsaPatient\x000%&" + MedComPatientName + "%&20260926%&user\x00")
	put := func(tag core.Tag, vr core.VR, value []byte) {
		if len(value)%2 != 0 {
			pad := byte(0)
			if vr == core.VRLO {
				pad = ' '
			}
			value = append(value, pad)
		}
		dataset.Put(core.NewRawElement(tag, vr, value))
	}
	put(core.NewTag(0x0025, 0x0010), core.VRLO, []byte("GEMS_SERS_01"))
	put(core.NewTag(0x0025, 0x101B), core.VROB, geBlock(GEProtocolKey+" \""+GEProtocolValue+"\"\nTE \"30\"\n"))
	put(core.NewTag(0x0029, 0x0010), core.VRLO, []byte("SIEMENS CSA HEADER"))
	put(core.NewTag(0x0029, 0x0011), core.VRLO, []byte("SIEMENS MEDCOM HEADER"))
	put(core.NewTag(0x0029, 0x1010), core.VROB, csaImage)
	put(core.NewTag(0x0029, 0x1020), core.VROB, csaSeries)
	put(core.NewTag(0x0029, 0x1120), core.VROB, append(medcom, 0, 0))
	put(core.NewTag(0x0055, 0x0010), core.VRLO, []byte("VEPRO VIM 5.0 DATA"))
	put(core.NewTag(0x0055, 0x1020), core.VROB, veproBlob())
	put(core.NewTag(0x01F7, 0x0010), core.VRLO, []byte("ELSCINT1"))
	put(core.NewTag(0x01F7, 0x1026), core.VROB, elscintBlob())
	return nil
}

func utf16le(s string) []byte {
	var out []byte
	for _, unit := range utf16.Encode([]rune(s)) {
		out = binary.LittleEndian.AppendUint16(out, unit)
	}
	return out
}

func u32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func cstr(s string) []byte { return append([]byte(s), 0) }

// geBlock is a Protocol Data Block: a 32-bit length and a gzip stream.
func geBlock(text string) []byte {
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	_, _ = writer.Write([]byte(text))
	_ = writer.Close()
	return append(u32(uint32(compressed.Len())), compressed.Bytes()...)
}

// elscintBlob is a binary ELSCINT1 protocol with a single group (protocol
// name and kV) and a multiple group of one item, in the layout gdcmdump
// --elscint prints (magic F22D, single 0006981A, multiple 00000925).
func elscintBlob() []byte {
	var b []byte
	b = append(b, u32(0xF22D)...)
	b = append(b, u32(1)...)
	b = append(b, u32(2)...)
	b = append(b, u32(0x0006981A)...)
	b = append(b, cstr("")...)
	b = append(b, u32(2)...)
	b = append(b, cstr("protocol-name")...)
	b = append(b, u32(1)...)
	b = append(b, cstr(ElscintProtocolName)...)
	b = append(b, cstr("kv")...)
	b = append(b, u32(1)...)
	b = append(b, cstr("120")...)
	b = append(b, u32(0x00000925)...)
	b = append(b, cstr("scans")...)
	b = append(b, u32(1)...)
	b = append(b, u32(1)...)
	b = append(b, cstr("mas")...)
	b = append(b, u32(1)...)
	b = append(b, cstr("200")...)
	return b
}

// veproBlob is VIM 5.0 data whose second data field holds a placeholder
// patient name.
func veproBlob() []byte {
	data := bytes.Repeat([]byte{' '}, 1786)
	put := func(offset int, value string) { copy(data[offset:], value) }
	put(0, "VIM5.0")
	put(198, "20260926")
	put(340, "PID-1")
	put(404, VEPROPatientName)
	put(1770, "XA")
	return data
}
