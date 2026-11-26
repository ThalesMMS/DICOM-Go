package iod

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"strings"
)

type fgRule struct {
	tag       core.Tag
	section   string
	required  func(*object.Object, int) (bool, bool)
	forbidden func(*object.Object) bool
}

func fgTag(n uint32) core.Tag                      { return core.NewTag(uint16(n>>16), uint16(n)) }
func fgMandatory(*object.Object, int) (bool, bool) { return true, true }
func fgOptional(*object.Object, int) (bool, bool)  { return false, true }
func fgManual(*object.Object, int) (bool, bool)    { return false, false }
func fgAndOriginal(o *object.Object, tag uint32, allowed func(string) bool) (bool, bool) {
	original, known := fgOriginal(o, 0)
	if known && !original {
		return false, true
	}
	v := fgText(o, tag)
	return original && allowed(v), known && v != ""
}
func fgCardiac(o *object.Object, _ int) (bool, bool) {
	return fgAndOriginal(o, 0x00189037, func(v string) bool { return v != "NONE" })
}
func fgRespiratory(o *object.Object, _ int) (bool, bool) {
	return fgAndOriginal(o, 0x00189170, func(v string) bool { return v != "NONE" && v != "REALTIME" && v != "BREATH_HOLD" })
}
func fgKSpace(o *object.Object, _ int) (bool, bool) {
	return fgAndOriginal(o, 0x00189032, func(v string) bool { return v == "RECTILINEAR" })
}
func fgVelocity(o *object.Object, _ int) (bool, bool) {
	return fgAndOriginal(o, 0x00189014, func(v string) bool { return v == "YES" })
}
func fgImageThird(wanted string) func(*object.Object, int) (bool, bool) {
	return func(o *object.Object, _ int) (bool, bool) {
		e, ok := o.Get(fgTag(0x00080008))
		if !ok || !fgPrimitiveWithin(e, 256) {
			return false, false
		}
		v := e.StringValues()
		if len(v) < 3 {
			return false, false
		}
		return v[2] == wanted, true
	}
}

func fgText(o *object.Object, n uint32) string {
	e, ok := o.Get(fgTag(n))
	if !ok || !fgPrimitiveWithin(e, 256) {
		return ""
	}
	v := e.StringValues()
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
func fgOriginal(o *object.Object, _ int) (bool, bool) {
	v := fgText(o, 0x00080008)
	return v == "ORIGINAL" || v == "MIXED", v != ""
}
func fgMono(o *object.Object, _ int) (bool, bool) {
	v := fgText(o, 0x00280004)
	return v == "MONOCHROME2", v != ""
}
func fgContrast(o *object.Object, _ int) (bool, bool) { return o.Has(fgTag(0x00180012)), true }
func fgDerived(o *object.Object, frame int) (bool, bool) {
	v := fgText(o, 0x00080008)
	if v == "DERIVED" {
		return true, true
	}
	value := fgFrameText(o, frame, fgTag(0x00189329), fgTag(0x00089007))
	if value == "" {
		value = fgFrameText(o, frame, fgTag(0x00189226), fgTag(0x00089007))
	}
	if value == "DERIVED" {
		return true, true
	}
	if value == "ORIGINAL" {
		return false, true
	}
	return false, false
}

func fgSegmentation(o *object.Object, _ int) (bool, bool) {
	v := fgText(o, 0x00620001)
	return fgText(o, 0x00209311) != "TILED_FULL" && v != "LABELMAP", v == "BINARY" || v == "FRACTIONAL" || v == "LABELMAP"
}
func fgSegDerivation(o *object.Object, _ int) (bool, bool) {
	if !o.Has(fgTag(0x00200052)) {
		return true, true
	}
	return false, false
}
func fgRules(iod string) ([]fgRule, bool) {
	var rules []fgRule
	add := func(tag uint32, sec string, required func(*object.Object, int) (bool, bool)) {
		rules = append(rules, fgRule{tag: fgTag(tag), section: sec, required: required})
	}
	if iod == "Segmentation" {
		for _, tag := range []uint32{0x00289110, 0x00209113, 0x00209116, 0x0048021A} {
			add(tag, "A.51.5", fgManual)
		}
		add(0x00089124, "C.7.6.16.2.6", fgSegDerivation)
		add(0x00209111, "C.7.6.16.2.2", fgOptional)
		add(0x0062000A, "C.8.20.3.1", fgSegmentation)
		return rules, true
	}
	if iod != "Enhanced CT Image" && iod != "Enhanced MR Image" {
		return nil, false
	}
	for _, tag := range []uint32{0x00289110, 0x00209111, 0x00209113, 0x00209116, 0x00209071} {
		add(tag, "C.7.6.16.2", fgMandatory)
	}
	add(0x00081140, "C.7.6.16.2.5", fgManual)
	add(0x00089124, "C.7.6.16.2.6", fgDerived)
	add(0x00189118, "C.7.6.16.2.7", fgCardiac)
	add(0x00209253, "C.7.6.16.2.17", fgRespiratory)
	add(0x00189341, "C.7.6.16.2.12", fgContrast)
	add(0x00289132, "C.7.6.16.2.10", fgOptional)
	add(0x00409096, "C.7.6.16.2.11", fgOptional)
	add(0x00209310, "C.7.6.16.2.23", fgOptional)
	if iod == "Enhanced CT Image" {
		for _, tag := range []uint32{0x00189477, 0x00189329, 0x00289145} {
			add(tag, "A.38.1.4", fgMandatory)
		}
		for _, tag := range []uint32{0x00189301, 0x00189304, 0x00189308, 0x00189326, 0x00189312, 0x00189321, 0x00189325} {
			add(tag, "A.38.1.4", fgOriginal)
		}
		add(0x00189314, "A.38.1.4", fgCTReconstruction)
		add(0x00189360, "A.38.1.4", fgManual)
		// Dataset-decidable RWVM presence for Multi-energy CT.
		for i := range rules {
			if rules[i].tag == fgTag(0x00409096) {
				rules[i].required = func(o *object.Object, _ int) (bool, bool) {
					v := fgText(o, 0x00189361)
					return v == "YES", v == "YES" || v == "NO" || !o.Has(fgTag(0x00189361))
				}
			}
		}
	} else {
		add(0x00189226, "C.8.13.5.1", fgMandatory)
		add(0x00289145, "C.7.6.16.2.9", fgMono)
		for _, tag := range []uint32{0x00189112, 0x00189114, 0x00189115, 0x00189006, 0x00189042, 0x00189049, 0x00189119} {
			add(tag, "A.36.2.4", fgOriginal)
		}
		add(0x00189125, "A.36.2.4", fgKSpace)
		add(0x00189197, "A.36.2.4", fgVelocity)
		add(0x00189152, "A.36.2.4", fgImageThird("METABOLITE_MAP"))
		add(0x00189251, "A.36.2.4", fgImageThird("ASL"))
		for _, tag := range []uint32{0x00189117, 0x00189107} {
			add(tag, "A.36.2.4", fgManual)
		}
		for i := range rules {
			if rules[i].tag == fgTag(0x00289132) || rules[i].tag == fgTag(0x00409096) {
				rules[i].forbidden = func(o *object.Object) bool { v := fgText(o, 0x00280004); return v != "" && v != "MONOCHROME2" }
			}
		}
	}
	return rules, true
}
func fgProfileSection(iod string) string {
	switch iod {
	case "Enhanced CT Image":
		return "A.38.1.4"
	case "Enhanced MR Image":
		return "A.36.2.4"
	default:
		return "A.51.5"
	}
}
func fgSource(section string, tag core.Tag) ConstraintSource {
	sha := "ad70f442400a53643a86a42344ba0d8f93e1e66f33ab6b20791a12f998524634"
	switch section {
	case "A.38.1.4":
		sha = "9c38e8348e573f8d8691d5dddfc4479fab2ee64bf83151458fcd8340cf11063d"
	case "A.36.2.4":
		sha = "689cfd725cd9d894e74c88d034dc36be89cae36f59e58d4d61ede69226018d4c"
	case "A.51.5":
		sha = "6159d1a48a22d3b904642897046617fdac319ef9ce4622074f8958e38ad8e050"
	case "C.7.6.17":
		sha = "5ad3dbabec7f5495ed8c856518020c6349481b27e097f50ea9ed8a7e39bf9b7c"
	}
	for key, digest := range fgCardinalitySourceHashes {
		if section == key {
			sha = digest
		}
	}
	if strings.HasPrefix(section, "C.7.6.16.2") {
		sha = "7e6c176242d323b9295862bbade84ea42f4eb4b5f8a6076c93c0ea47410a83fe"
	}
	if strings.HasPrefix(section, "C.8.13.5") && fgCardinalitySourceHashes[section] == "" {
		sha = "293c7993d416bd2b7e8f0ad6d07806fdd17846f59974dd31ee3c5e6f9b0d0bfc"
	}
	if strings.HasPrefix(section, "C.8.20.3") {
		sha = "f32b3b524f19b78f213ebee2c9e930a079dd9ad4bf725fae672efcc4d678cd8c"
	}
	if section == "PS3.5 7.1" {
		return ConstraintSource{Table: "Data Element structure", TableID: "7.1", Index: -1, Tag: tag, Section: "7.1", Edition: "PS3.5 2026d", SHA256: "7646dc7a3a0603657bf5c8c08b2fb513ec3ca3d2274f3959bc2ba431e01718d5"}
	}
	return ConstraintSource{Table: "Functional Group structural profile", TableID: section, Index: -1, Tag: tag, Section: section, Edition: "PS3.3 2026d", SHA256: sha}
}

// Read bounded raw sequence item metadata, without allocating all frame facades.
func fgFrameText(o *object.Object, frame int, macro, attribute core.Tag) string {
	read := func(ds core.DataSet) string {
		for _, e := range ds.Elements {
			if e.Tag() != macro {
				continue
			}
			sq, ok := e.Value.(core.SequenceValue)
			if !ok || len(sq.Items) != 1 {
				return ""
			}
			for _, v := range sq.Items[0].Elements {
				if v.Tag() == attribute && fgPrimitiveWithin(v, 256) {
					return v.StringValue()
				}
			}
		}
		return ""
	}
	if per, ok := fgRawSequence(o, fgPerFrame); ok && frame >= 0 && frame < len(per) {
		if v := read(per[frame]); v != "" {
			return v
		}
	}
	if shared, ok := fgRawSequence(o, fgShared); ok && len(shared) == 1 {
		return read(shared[0])
	}
	return ""
}
func fgCTReconstruction(o *object.Object, frame int) (bool, bool) {
	original, known := fgOriginal(o, frame)
	if known && !original {
		return false, true
	}
	v := fgFrameText(o, frame, fgTag(0x00189301), fgTag(0x00189302))
	return original && v != "CONSTANT_ANGLE", known && v != ""
}
func (s *fgState) bindInstanceConditions(obj *object.Object, rules []fgRule, frames int) error {
	original, originalKnown := fgOriginal(obj, 0)
	for i := range rules {
		tag := rules[i].tag
		if tag != fgTag(0x00189117) && tag != fgTag(0x00189107) {
			continue
		}
		required, known := false, true
		if !originalKnown {
			known = false
		} else if original {
			macro, attribute, wanted := fgTag(0x00189226), fgTag(0x00089209), "DIFFUSION"
			if tag == fgTag(0x00189107) {
				macro, attribute, wanted = fgTag(0x00189006), fgTag(0x00189027), "SLAB"
			}
			for frame := 0; frame < frames; frame++ {
				if err := s.work(); err != nil {
					return err
				}
				v := fgFrameText(obj, frame, macro, attribute)
				if v == wanted {
					required = true
				}
				if v == "" {
					known = false
				}
			}
			if required {
				known = true
			}
		}
		a, b := required, known
		rules[i].required = func(*object.Object, int) (bool, bool) { return a, b }
	}
	return nil
}

func (s *fgState) checkSegGeometry(obj *object.Object, frame int) {
	if !obj.Has(fgTag(0x00200052)) {
		return
	}
	projection := s.projections[frame]
	has := func(tag core.Tag) bool { present, _ := fgMacroPresent(projection, tag); return present }
	measures := has(fgTag(0x00289110))
	patient := has(fgTag(0x00209113)) && has(fgTag(0x00209116))
	slide := has(fgTag(0x0048021A))
	// A.51-2 permits omission of Plane Position (Slide) for TILED_FULL.
	// The root slide orientation and total-matrix origin establish that branch;
	// TILED_FULL alone never substitutes for patient position/orientation.
	orientation, hasOrientation := obj.Get(fgTag(0x00480102))
	origin, hasOrigin := fgRawSequence(obj, fgTag(0x00480008))
	implicitSlide := fgText(obj, 0x00209311) == "TILED_FULL" && hasOrientation && orientation.VR() == core.VRDS && !fgIndexedEmpty(orientation) && hasOrigin && len(origin) == 1
	if !measures || !patient && !slide && !implicitSlide {
		s.add(MissingFunctionalGroup, fgPath(frame, fgTag(0x00289110)), fgTag(0x00289110), "seg-frame-of-reference-geometry", "A.51.5", "Frame of Reference requires Pixel Measures and patient position/orientation or the slide geometry branch")
	}
}

// fgMacroCardinality binds the top-level SQ item count, not inner attributes.
// Zero maximum denotes no finite normative maximum; resource caps still apply.
// CT acquisition details/geometry/exposure/X-Ray details are single-item unless
// Multi-energy CT Acquisition is YES. Unknown multi-energy context cannot prove
// a maximum, but the normative minimum of one remains dataset-decidable.
func fgMacroCardinality(o *object.Object, r fgRule) (min, max int, known bool) {
	switch uint32(r.tag.Group)<<16 | uint32(r.tag.Element) {
	case 0x00081140, 0x00089124, 0x00189107:
		return 0, 0, true
	case 0x00409096, 0x00189341, 0x00189042, 0x00189197, 0x00189251, 0x00189360:
		return 1, 0, true
	case 0x00189304, 0x00189312, 0x00189321, 0x00189325:
		v := fgText(o, 0x00189361)
		if v == "YES" {
			return 1, 0, true
		}
		if v == "NO" || !o.Has(fgTag(0x00189361)) {
			return 1, 1, true
		}
		return 1, 0, false
	default:
		return 1, 1, true
	}
}

var fgCardinalitySections = map[uint32]string{
	0x00189477: "C.7.6.16.2.18",
	0x00189329: "C.8.15.3.1",
	0x00289145: "C.8.15.3.10",
	0x00189301: "C.8.15.3.2",
	0x00189304: "C.8.15.3.3",
	0x00189308: "C.8.15.3.4",
	0x00189326: "C.8.15.3.5",
	0x00189312: "C.8.15.3.6",
	0x00189314: "C.8.15.3.7",
	0x00189321: "C.8.15.3.8",
	0x00189325: "C.8.15.3.9",
	0x00189360: "C.8.15.3.11",
	0x00189112: "C.8.13.5.2",
	0x00189125: "C.8.13.5.3",
	0x00189114: "C.8.13.5.4",
	0x00189115: "C.8.13.5.5",
	0x00189006: "C.8.13.5.6",
	0x00189042: "C.8.13.5.7",
	0x00189049: "C.8.13.5.8",
	0x00189117: "C.8.13.5.9",
	0x00189119: "C.8.13.5.10",
	0x00189107: "C.8.13.5.11",
	0x00189152: "C.8.13.5.12",
	0x00189197: "C.8.13.5.13",
	0x00189251: "C.8.13.5.14",
}
var fgCardinalitySourceHashes = map[string]string{
	"C.8.15.3.1":  "f9e29df26e1ff74aca9b544a6a8a83d0b9aedec978bdd62403d270754f2a7e7b",
	"C.8.13.5.2":  "f26a42ae5cfb73aeff9a7a2f11ed9d045cbe54c9bdf3d8d27cdfb5b2f262a4f2",
	"C.8.13.5.3":  "54d3b9abdd094193464bdcc38ffa33f305786c3bdf09fd22484bb912892219ea",
	"C.8.13.5.4":  "3f8b3ae2dfbc604f42784246a369775157ebb838ddadcc22a364f3cb7f450b94",
	"C.8.13.5.5":  "34e1bf79893b334bfaf44353cccf69a5eba6b01a5ecccf7269271aebd8d34e46",
	"C.8.13.5.6":  "74baff2f1529840c0dbc3f10f35841870eaa0b2da6aa6020ac3c3ee16f8bd0d6",
	"C.8.13.5.7":  "5cc57ad7e0c93c997e39162dcac4d2304180a8f6f1439ee3e249ac995cd90c78",
	"C.8.13.5.8":  "ef81eee2b5de9715abf6eed80e3217d528491f468d94cb74a363c33fcaea3d3a",
	"C.8.13.5.9":  "83775287aa2048d9a4c79fcd8d3dbe4356904459caac9b7fd53998190892c8bf",
	"C.8.13.5.10": "93dc807e43fe020cdce25059844c4a1b516f46820430ade3fbd05e57f5aaad12",
	"C.8.13.5.11": "aa507de516fd717fb8d7c7cb7a663863486feb2f35b5aac5657caa56a332547b",
	"C.8.13.5.12": "6dd5cbd4e196d3d8190f407d71ef8ff02a6f4521047997255546bba1721bbd8e",
	"C.8.13.5.13": "a60eda173a2aaa9693a533033e28ef0ec48693d81ea5a869e1ccb765b543088a",
	"C.8.13.5.14": "82bb387e66ede168188b5f9181b09cb6eb1dc00e78780ea8117586e3253a9d8d",
	"C.8.13.5.15": "39a7fe871435d5beb815c2be289b83c28ecb2944ff43baa4ccd826947b2b47e6",
	"C.8.15.3.2":  "9474c63861a3ec1830d1012b9646f1d5d663e117bb01ebf6fe781abc103a7995",
	"C.8.15.3.3":  "e79013da45b6084e31055b596a3ecaffaf9e38502f93ed2e253096fb4b7ecc73",
	"C.8.15.3.4":  "b38ea58688480ac308ac2e470cde858fa8a85f76ad311b48c2f22b47397b3907",
	"C.8.15.3.5":  "be6f2da6340ae0959b40026129e9c389786b0f94b8d1539d1c0ff64a08f6f3d1",
	"C.8.15.3.6":  "b15af979f763dedcbbfd8b0ae6f86dd52f56c59ca717521045af0112df180fa4",
	"C.8.15.3.7":  "7ab02fd74cc3d2e98a336ff0ff58df8c6f5effe12043bc987ebab7faf1e5ff1e",
	"C.8.15.3.8":  "8b23a859c3c41cc9498621bdb1e1ae275355cdafeaa490993f07edb291e858ac",
	"C.8.15.3.9":  "6b7040dca3ca2525dcbbf3b32b79ea12f2ff84c51d3c178865a06aa3e193e6f4",
	"C.8.15.3.10": "82dc06ccc539c875c53316c635ae7cfa0a4e8bae68cfb53f9be2fd41275ac45f",
	"C.8.15.3.11": "cf9c147ed6e28946b22f7e07475c9e8ee08f9bfe201da11aa1e538c5221c9500",
	"C.8.15.3.12": "b6b519e7fea18119f7f2f12dda560067ba3613408e0ac6ee1a1f2c6df49b900b",
	"C.8.15.3.13": "342a6e0c518655ee836be25f15ee34e28f766b6b3a29ca390386ef8a15e20d7d",
	"C.8.15.3.14": "97833caac605e471cda4901c3e21e2fe0f5746f72ebb1294d55f16d0e09b19a9",
	"C.8.15.3.15": "445b6db53cd9d38ed905b4fca768881909aec7c8b7dd371eef0bd1ff64de222c",
}

func fgCardinalitySection(r fgRule) string {
	if section := fgCardinalitySections[uint32(r.tag.Group)<<16|uint32(r.tag.Element)]; section != "" {
		return section
	}
	return r.section
}
