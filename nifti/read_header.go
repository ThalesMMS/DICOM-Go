package nifti

import (
	"encoding/binary"
	"math"
)

const (
	nifti1HeaderSize = 348
	nifti2HeaderSize = 540
	// maxHeaderDimension bounds every dim[] entry of either version. NIfTI-1
	// dimensions are int16 already; NIfTI-2 int64 dimensions must still fit a
	// signed 32-bit index, as they do in the reference readers.
	maxHeaderDimension = math.MaxInt32
)

// XformCode is a NIfTI qform_code/sform_code value (NIFTI_XFORM_*).
type XformCode int32

const (
	XformUnknown       XformCode = 0 // no transform of this kind
	XformScannerAnat   XformCode = 1 // scanner-based anatomical coordinates
	XformAlignedAnat   XformCode = 2 // aligned to another file's scanner coordinates
	XformTalairach     XformCode = 3 // Talairach-Tournoux atlas space
	XformMNI152        XformCode = 4 // MNI 152 template space
	XformTemplateOther XformCode = 5 // another named template space
)

func (c XformCode) String() string {
	switch c {
	case XformUnknown:
		return "unknown"
	case XformScannerAnat:
		return "scanner-anat"
	case XformAlignedAnat:
		return "aligned-anat"
	case XformTalairach:
		return "talairach"
	case XformMNI152:
		return "mni-152"
	case XformTemplateOther:
		return "template-other"
	default:
		return "invalid"
	}
}

func (c XformCode) valid() bool { return c >= XformUnknown && c <= XformTemplateOther }

// SpatialUnit is the xyzt_units spatial field.
type SpatialUnit uint8

const (
	SpatialUnitUnknown    SpatialUnit = 0
	SpatialUnitMeter      SpatialUnit = 1
	SpatialUnitMillimeter SpatialUnit = 2
	SpatialUnitMicrometer SpatialUnit = 3
)

const (
	spatialUnitMask                  uint8 = 0x07
	temporalUnitMask                 uint8 = 0x38
	nifti1SingleFileMagic                  = "n+1\x00"
	nifti1PairMagic                        = "ni1\x00"
	nifti2SingleFileMagic                  = "n+2\x00\r\n\x1a\n"
	nifti2PairMagic                        = "ni2\x00\r\n\x1a\n"
	nifti2MagicSignaturePrefixLength       = 4
)

// millimeters returns the factor converting this unit to millimeters. An
// unknown unit is read as millimeters, like the ITK and nibabel readers.
func (u SpatialUnit) millimeters() float64 {
	switch u {
	case SpatialUnitMeter:
		return 1000
	case SpatialUnitMicrometer:
		return 0.001
	default:
		return 1
	}
}

// Header holds the fields of a NIfTI-1 or NIfTI-2 header that describe voxel
// layout, scaling, and geometry, converted to float64/int64 regardless of the
// on-disk version. Free-text fields (descrip, aux_file, intent_name, db_name)
// are deliberately not exposed: third-party writers put arbitrary text there,
// including identifiers.
type Header struct {
	Version   int  // 1 or 2
	Paired    bool // magic "ni1"/"ni2": voxels live in a separate .img file
	BigEndian bool
	// Dim is dim[0..7] with dimensions above dim[0] normalized to 1.
	Dim [8]int64
	// PixDim is pixdim[0..7] as stored; pixdim[0] carries qfac.
	PixDim        [8]float64
	Datatype      int16
	BitPix        int16
	VoxelOffset   float64 // vox_offset as stored (float32 in NIfTI-1)
	SclSlope      float64
	SclInter      float64
	CalMax        float64
	CalMin        float64
	SliceDuration float64
	TOffset       float64
	SliceStart    int64
	SliceEnd      int64
	SliceCode     uint8
	DimInfo       uint8
	XYZTUnits     uint8
	IntentCode    int32
	IntentParams  [3]float64
	QFormCode     XformCode
	SFormCode     XformCode
	QuaternB      float64
	QuaternC      float64
	QuaternD      float64
	QOffset       [3]float64
	SRow          [3][4]float64
	// Extended reports a non-zero extension flag after a single-file header.
	// Extension payloads are skipped, never parsed: code 2 carries DICOM data
	// and can hold PHI.
	Extended bool
}

// Size is the on-disk header size in bytes (348 or 540).
func (h Header) Size() int {
	if h.Version == 2 {
		return nifti2HeaderSize
	}
	return nifti1HeaderSize
}

// SpatialUnit returns the spatial unit encoded in xyzt_units.
func (h Header) SpatialUnit() SpatialUnit { return SpatialUnit(h.XYZTUnits & spatialUnitMask) }

// TemporalUnit returns the raw temporal xyzt_units code (8 s, 16 ms, 24 us,
// 32 Hz, 40 ppm, 48 rad/s, 0 unknown).
func (h Header) TemporalUnit() uint8 { return h.XYZTUnits & temporalUnitMask }

// QFac is -1 when pixdim[0] is negative and +1 otherwise, as NIfTI specifies.
func (h Header) QFac() float64 {
	if h.PixDim[0] < 0 {
		return -1
	}
	return 1
}

// ParseHeader decodes a NIfTI-1 (348-byte) or NIfTI-2 (540-byte) header from
// the start of data, detecting the version and byte order from sizeof_hdr.
// It checks the magic signature and dim[] structure only; datatype support,
// voxel offset, resource limits, and geometry are checked by the readers.
func ParseHeader(data []byte) (Header, error) {
	if len(data) < 4 {
		return Header{}, readError(ReadCodeFormat, "sizeof_hdr", ErrNotNIfTI, nil)
	}
	var (
		order   binary.ByteOrder
		version int
	)
	switch {
	case binary.LittleEndian.Uint32(data) == nifti1HeaderSize:
		order, version = binary.LittleEndian, 1
	case binary.BigEndian.Uint32(data) == nifti1HeaderSize:
		order, version = binary.BigEndian, 1
	case binary.LittleEndian.Uint32(data) == nifti2HeaderSize:
		order, version = binary.LittleEndian, 2
	case binary.BigEndian.Uint32(data) == nifti2HeaderSize:
		order, version = binary.BigEndian, 2
	default:
		return Header{}, readError(ReadCodeFormat, "sizeof_hdr", ErrNotNIfTI, nil)
	}
	size := nifti1HeaderSize
	if version == 2 {
		size = nifti2HeaderSize
	}
	if len(data) < size {
		return Header{}, readError(ReadCodeTruncated, "header", ErrTruncatedData, nil)
	}
	var header Header
	var err error
	if version == 1 {
		header, err = parseNIfTI1(data[:size], order)
	} else {
		header, err = parseNIfTI2(data[:size], order)
	}
	if err != nil {
		return Header{}, err
	}
	header.BigEndian = order == binary.BigEndian
	if err := normalizeHeaderDimensions(&header); err != nil {
		return Header{}, err
	}
	return header, nil
}

func parseNIfTI1(data []byte, order binary.ByteOrder) (Header, error) {
	header := Header{Version: 1}
	switch string(data[344:348]) {
	case nifti1SingleFileMagic:
	case nifti1PairMagic:
		header.Paired = true
	default:
		// Analyze 7.5 and anything else without a NIfTI signature.
		return Header{}, readError(ReadCodeFormat, "magic", ErrNotNIfTI, nil)
	}
	i16 := func(offset int) int16 { return int16(order.Uint16(data[offset:])) }
	f32 := func(offset int) float64 { return float64(math.Float32frombits(order.Uint32(data[offset:]))) }

	header.DimInfo = data[39]
	for index := range header.Dim {
		header.Dim[index] = int64(i16(40 + 2*index))
	}
	for index := range header.IntentParams {
		header.IntentParams[index] = f32(56 + 4*index)
	}
	header.IntentCode = int32(i16(68))
	header.Datatype = i16(70)
	header.BitPix = i16(72)
	header.SliceStart = int64(i16(74))
	for index := range header.PixDim {
		header.PixDim[index] = f32(76 + 4*index)
	}
	header.VoxelOffset = f32(108)
	header.SclSlope = f32(112)
	header.SclInter = f32(116)
	header.SliceEnd = int64(i16(120))
	header.SliceCode = data[122]
	header.XYZTUnits = data[123]
	header.CalMax = f32(124)
	header.CalMin = f32(128)
	header.SliceDuration = f32(132)
	header.TOffset = f32(136)
	header.QFormCode = XformCode(i16(252))
	header.SFormCode = XformCode(i16(254))
	header.QuaternB = f32(256)
	header.QuaternC = f32(260)
	header.QuaternD = f32(264)
	for index := range header.QOffset {
		header.QOffset[index] = f32(268 + 4*index)
	}
	for row := range header.SRow {
		for column := range header.SRow[row] {
			header.SRow[row][column] = f32(280 + 16*row + 4*column)
		}
	}
	return header, nil
}

func parseNIfTI2(data []byte, order binary.ByteOrder) (Header, error) {
	header := Header{Version: 2}
	switch magic := string(data[4:12]); magic {
	case nifti2SingleFileMagic:
	case nifti2PairMagic:
		header.Paired = true
	default:
		if magic[:nifti2MagicSignaturePrefixLength] == nifti2SingleFileMagic[:nifti2MagicSignaturePrefixLength] ||
			magic[:nifti2MagicSignaturePrefixLength] == nifti2PairMagic[:nifti2MagicSignaturePrefixLength] {
			// The trailing \r\n\032\n bytes exist to detect text-mode
			// newline translation; a damaged tail means damaged content.
			return Header{}, readError(ReadCodeHeader, "magic", ErrMalformedHeader, nil)
		}
		return Header{}, readError(ReadCodeFormat, "magic", ErrNotNIfTI, nil)
	}
	i16 := func(offset int) int16 { return int16(order.Uint16(data[offset:])) }
	i32 := func(offset int) int32 { return int32(order.Uint32(data[offset:])) }
	i64 := func(offset int) int64 { return int64(order.Uint64(data[offset:])) }
	f64 := func(offset int) float64 { return math.Float64frombits(order.Uint64(data[offset:])) }

	header.Datatype = i16(12)
	header.BitPix = i16(14)
	for index := range header.Dim {
		header.Dim[index] = i64(16 + 8*index)
	}
	for index := range header.IntentParams {
		header.IntentParams[index] = f64(80 + 8*index)
	}
	for index := range header.PixDim {
		header.PixDim[index] = f64(104 + 8*index)
	}
	voxOffset := i64(168)
	header.VoxelOffset = float64(voxOffset)
	if int64(header.VoxelOffset) != voxOffset {
		return Header{}, readError(ReadCodeHeader, "vox_offset", ErrMalformedHeader, nil)
	}
	header.SclSlope = f64(176)
	header.SclInter = f64(184)
	header.CalMax = f64(192)
	header.CalMin = f64(200)
	header.SliceDuration = f64(208)
	header.TOffset = f64(216)
	header.SliceStart = i64(224)
	header.SliceEnd = i64(232)
	header.QFormCode = XformCode(i32(344))
	header.SFormCode = XformCode(i32(348))
	header.QuaternB = f64(352)
	header.QuaternC = f64(360)
	header.QuaternD = f64(368)
	for index := range header.QOffset {
		header.QOffset[index] = f64(376 + 8*index)
	}
	for row := range header.SRow {
		for column := range header.SRow[row] {
			header.SRow[row][column] = f64(400 + 32*row + 8*column)
		}
	}
	header.SliceCode = uint8(i32(496))
	header.XYZTUnits = uint8(i32(500))
	header.IntentCode = i32(504)
	header.DimInfo = data[524]
	return header, nil
}

func normalizeHeaderDimensions(header *Header) error {
	count := header.Dim[0]
	if count < 1 || count > 7 {
		return readError(ReadCodeDimensions, "dim[0]", ErrMalformedHeader, nil)
	}
	for index := 1; index < len(header.Dim); index++ {
		if int64(index) > count {
			header.Dim[index] = 1
			continue
		}
		if header.Dim[index] < 1 || header.Dim[index] > maxHeaderDimension {
			return readError(ReadCodeDimensions, "dim", ErrMalformedHeader, nil)
		}
	}
	return nil
}
