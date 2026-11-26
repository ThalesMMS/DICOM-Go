package dicomweb

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"mime"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/video"
)

// A whole bulk profile is one document or one complete video representation.
// It never uses the still-image /frames/{n} binding convention.
type storeWholeBulkProfile struct {
	media, sop, modality string
	video                bool
}

var storeDocumentProfiles = [...]storeWholeBulkProfile{
	{"application/pdf", encapdoc.EncapsulatedPDFStorage, "DOC", false},
	{"text/xml", encapdoc.EncapsulatedCDAStorage, "DOC", false},
	{"model/stl", encapdoc.EncapsulatedSTLStorage, "M3D", false},
	{"model/obj", encapdoc.EncapsulatedOBJStorage, "M3D", false},
	{"model/mtl", encapdoc.EncapsulatedMTLStorage, "M3D", false},
}

func storeWholeProfile(media string) (storeWholeBulkProfile, bool) {
	for _, p := range storeDocumentProfiles {
		if media == p.media {
			return p, true
		}
	}
	if media == "video/mp4" || media == "video/mpeg" {
		return storeWholeBulkProfile{media: media, video: true}, true
	}
	return storeWholeBulkProfile{}, false
}
func storeWholePartParameters(media string, params map[string]string) bool {
	p, ok := storeWholeProfile(media)
	if !ok {
		return false
	}
	for name, v := range params {
		switch name {
		case "transfer-syntax":
			if p.video {
				if media == "video/mp4" && v != transfer.MPEG4HP41.UID || media == "video/mpeg" && v != transfer.MPEG2MPML.UID && v != transfer.MPEG2MPHL.UID {
					return false
				}
			} else if v != explicitVRLittleEndianUID {
				return false
			}
		case "charset":
			if media != "text/xml" || !strings.EqualFold(v, "utf-8") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

var storeDocumentTag = core.NewTag(0x0042, 0x0011)
var storeDocumentMIMETag = core.NewTag(0x0042, 0x0012)

func storeSingleString(obj *object.Object, tag core.Tag) (string, bool) {
	values, ok := obj.GetStrings(tag)
	if !ok || len(values) != 1 {
		return "", false
	}
	return strings.TrimSpace(values[0]), true
}
func storeRequiredString(obj *object.Object, tag core.Tag) (string, error) {
	v, ok := storeSingleString(obj, tag)
	if !ok || v == "" {
		return "", ErrInvalidRequest
	}
	return v, nil
}
func storeWholeString(obj *object.Object, tag core.Tag, vr core.VR, want string, derive bool) error {
	if element, present := obj.Get(tag); present {
		if element.VR() != vr {
			return ErrInvalidRequest
		}
		v, ok := storeSingleString(obj, tag)
		if !ok || v != want {
			return ErrInvalidRequest
		}
		return nil
	}
	if !derive {
		return ErrInvalidRequest
	}
	obj.Put(core.NewRawElement(tag, vr, []byte(want)))
	return nil
}
func storeWholeSequence(obj *object.Object, tag core.Tag, minimum, maximum int) error {
	e, ok := obj.Get(tag)
	if !ok || e.VR() != core.VRSQ {
		return ErrInvalidRequest
	}
	v, ok := e.Value.(core.SequenceValue)
	if !ok || len(v.Items) < minimum || len(v.Items) > maximum {
		return ErrInvalidRequest
	}
	return nil
}
func storeWholeUID(obj *object.Object, tag core.Tag) error {
	v, err := storeRequiredString(obj, tag)
	if err != nil || !validStoreMediaUID(v) {
		return ErrInvalidRequest
	}
	return nil
}
func validStoreMediaUID(v string) bool { return core.IsValidUID(v) }

// prepareWholeStore validates content/binding before touching the reconstruction
// spool. Only verified missing length/coding/timing/HL7 facts may be derived;
// required clinical metadata is never supplied by a constructor's defaults.
func (s *Server) prepareWholeStore(ctx context.Context, instance storeMetadataInstance, bulk map[string]stagedStoreMIMEPart) (storeMetadataInstance, transfer.Syntax, string, string, error) {
	syntax := transfer.ExplicitVRLittleEndian
	var target core.Tag
	var part stagedStoreMIMEPart
	var profile storeWholeBulkProfile
	ref := ""
	err := instance.object.WalkContext(ctx, object.WalkOptions{MaxDepth: s.limits.MaxJSONDepth, MaxElements: s.limits.MaxJSONValues}, func(path []core.Tag, e core.Element) error {
		b, ok := e.Value.(core.BulkDataValue)
		if !ok {
			return nil
		}
		p, present := bulk[b.URI]
		if !present {
			return nil
		}
		if p.semanticFailure {
			return ErrInvalidRequest
		}
		w, whole := storeWholeProfile(p.contentType)
		if !whole {
			return nil
		}
		if ref != "" || len(path) != 1 || w.video && e.Tag() != core.TagPixelData || !w.video && e.Tag() != storeDocumentTag || e.VR() != core.VROB {
			return ErrInvalidRequest
		}
		target, part, profile, ref = e.Tag(), p, w, b.URI
		return nil
	})
	if err != nil {
		return instance, syntax, "", "", err
	}
	if ref == "" {
		// A document's normative media type is its declared MIME, not octet-stream.
		if e, ok := instance.object.Get(storeDocumentTag); ok {
			if _, bulkRef := e.Value.(core.BulkDataValue); bulkRef {
				return instance, syntax, "", "", ErrInvalidRequest
			}
		}
		return instance, syntax, "", "", nil
	}
	for _, other := range instance.refs {
		if other == ref {
			continue
		}
		if _, ok := bulk[other]; !ok {
			return instance, syntax, "", "", ErrInvalidRequest
		}
	}
	obj := object.FromElements(instance.object.Elements(), nil)
	obj.SetValueByteOrder(instance.object.ValueByteOrder())
	sop, e := storeRequiredString(obj, core.NewTag(0x0008, 0x0016))
	if e != nil {
		return instance, syntax, "", "", e
	}
	for _, tag := range []core.Tag{core.NewTag(0x0020, 0x0011), core.NewTag(0x0020, 0x0013)} {
		element, present := obj.Get(tag)
		values, valid := obj.GetStrings(tag)
		if !present || element.VR() != core.VRIS || !valid || len(values) > 1 {
			return instance, syntax, "", "", ErrInvalidRequest
		}
		value := ""
		if len(values) == 1 {
			value = strings.TrimSpace(values[0])
		}
		if value == "" {
			// General Series Number and video Instance Number are Type 2;
			// Encapsulated Document Instance Number is Type 1.
			if tag == core.NewTag(0x0020, 0x0013) && !profile.video {
				return instance, syntax, "", "", ErrInvalidRequest
			}
		} else if _, err := strconv.ParseInt(value, 10, 32); err != nil {
			return instance, syntax, "", "", ErrInvalidRequest
		}
	}
	max := s.limits.MaxPartBytes
	if s.limits.MaxDecodedPartBytes < max {
		max = s.limits.MaxDecodedPartBytes
	}
	if max > 1<<30 {
		max = 1 << 30
	}
	data, e := readStagedStoreMIMEPart(ctx, part, max)
	if e != nil {
		return instance, syntax, "", "", e
	}
	if profile.video {
		if sop != "1.2.840.10008.5.1.4.1.1.77.1.1.1" && sop != "1.2.840.10008.5.1.4.1.1.77.1.2.1" && sop != "1.2.840.10008.5.1.4.1.1.77.1.4.1" {
			return instance, syntax, "", "", ErrInvalidRequest
		}
		if _, exists := obj.Get(storeDocumentTag); exists {
			return instance, syntax, "", "", ErrInvalidRequest
		}
		modality := map[string]string{"1.2.840.10008.5.1.4.1.1.77.1.1.1": "ES", "1.2.840.10008.5.1.4.1.1.77.1.2.1": "GM", "1.2.840.10008.5.1.4.1.1.77.1.4.1": "XC"}[sop]
		if e = storeWholeString(obj, core.NewTag(0x0008, 0x0060), core.VRCS, modality, false); e != nil {
			return instance, syntax, "", "", e
		}
		region := core.NewTag(0x0008, 0x2218)
		if _, present := obj.Get(region); present {
			if e = storeWholeCode(obj, region); e != nil {
				return instance, syntax, "", "", e
			}
		} else if e = storeWholeSequence(obj, core.NewTag(0x0040, 0x0560), 1, s.limits.MaxJSONValues); e != nil {
			return instance, syntax, "", "", e
		}
		info, e := video.InspectSource(ctx, data, video.ImportLimits{MaxBytes: max, MaxFrames: min(s.limits.MaxFrames, 100000), MaxPixels: s.limits.MaxDecodedPartBytes / 3, MaxDecodedBytes: s.limits.MaxDecodedPartBytes})
		if e != nil {
			if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
				return instance, syntax, "", "", e
			}
			if errors.Is(e, video.ErrMediaTooLarge) {
				return instance, syntax, "", "", ErrResourceLimit
			}
			return instance, syntax, "", "", ErrInvalidRequest
		}
		if profile.media == "video/mp4" && info.Container != video.ContainerMP4 || profile.media == "video/mpeg" && info.Container != video.ContainerMPEGProgram || part.transferSyntaxUID != "" && part.transferSyntaxUID != info.TransferSyntax.UID {
			return instance, syntax, "", "", ErrInvalidRequest
		}
		for tag, want := range map[core.Tag]int{core.NewTag(0x0028, 0x0010): info.Rows, core.NewTag(0x0028, 0x0011): info.Columns, core.NewTag(0x0028, 0x0002): 3, core.NewTag(0x0028, 0x0100): 8, core.NewTag(0x0028, 0x0101): 8, core.NewTag(0x0028, 0x0102): 7, core.NewTag(0x0028, 0x0103): 0, core.NewTag(0x0028, 0x0006): 0} {
			if old, exists := obj.Get(tag); exists && old.VR() != core.VRUS {
				return instance, syntax, "", "", ErrInvalidRequest
			}
			if e = compressedStoreUS(obj, tag, want, true); e != nil {
				return instance, syntax, "", "", e
			}
		}
		for tag, value := range map[core.Tag]string{core.NewTag(0x0028, 0x0004): "YBR_PARTIAL_420", core.NewTag(0x0028, 0x2110): "01", core.NewTag(0x0028, 0x2114): info.CompressionMethod} {
			if e = storeWholeString(obj, tag, core.VRCS, value, true); e != nil {
				return instance, syntax, "", "", e
			}
		}
		if e = storeWholeString(obj, core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(info.Frames), true); e != nil {
			return instance, syntax, "", "", e
		}
		frameTime := 1000 / info.FrameRate
		if _, exists := obj.Get(core.NewTag(0x0018, 0x1063)); exists {
			v, ok := storeSingleString(obj, core.NewTag(0x0018, 0x1063))
			f, e := strconv.ParseFloat(v, 64)
			if element, _ := obj.Get(core.NewTag(0x0018, 0x1063)); element.VR() != core.VRDS || !ok || e != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f-frameTime) > math.Abs(frameTime)*1e-8 {
				return instance, syntax, "", "", ErrInvalidRequest
			}
		} else {
			obj.Put(core.NewRawElement(core.NewTag(0x0018, 0x1063), core.VRDS, []byte(strconv.FormatFloat(frameTime, 'g', 12, 64))))
		}
		pointer := core.NewTag(0x0028, 0x0009)
		if existing, ok := obj.Get(pointer); ok {
			v, ok := existing.Value.(core.TagValue)
			if !ok || len(v) != 1 || v[0] != core.NewTag(0x0018, 0x1063) {
				return instance, syntax, "", "", ErrInvalidRequest
			}
		} else {
			obj.Put(core.Element{Header: core.ElementHeader{Tag: pointer, VR: core.VRAT}, Value: core.TagValue{core.NewTag(0x0018, 0x1063)}})
		}
		obj.Put(core.Element{Header: core.ElementHeader{Tag: target, VR: core.VROB, Length: core.UndefinedLength}, Value: core.FragmentSequence{Fragments: [][]byte{data}}})
		instance.object = obj
		return instance, info.TransferSyntax, ref, "", nil
	}
	if sop != profile.sop {
		return instance, syntax, "", "", ErrInvalidRequest
	}
	if _, present := obj.Get(core.TagPixelData); present {
		return instance, syntax, "", "", ErrInvalidRequest
	}
	if e = storeWholeString(obj, core.NewTag(0x0008, 0x0060), core.VRCS, profile.modality, false); e != nil {
		return instance, syntax, "", "", e
	}
	declared, e := storeRequiredString(obj, storeDocumentMIMETag)
	if e != nil {
		return instance, syntax, "", "", e
	}
	media, params, e := mime.ParseMediaType(declared)
	if e != nil || strings.ToLower(media) != profile.media || len(params) != 0 {
		return instance, syntax, "", "", ErrInvalidRequest
	}
	burned, e := storeRequiredString(obj, core.NewTag(0x0028, 0x0301))
	if e != nil || burned != "YES" && burned != "NO" {
		return instance, syntax, "", "", ErrInvalidRequest
	}
	for tag, vr := range map[core.Tag]core.VR{core.NewTag(0x0008, 0x0023): core.VRDA, core.NewTag(0x0008, 0x0033): core.VRTM, core.NewTag(0x0008, 0x002a): core.VRDT, core.NewTag(0x0042, 0x0010): core.VRST} {
		if element, present := obj.Get(tag); !present || element.VR() != vr {
			return instance, syntax, "", "", ErrInvalidRequest
		}
	}
	if e = storeWholeSequence(obj, core.NewTag(0x0040, 0xa043), 0, 1); e != nil {
		return instance, syntax, "", "", e
	}
	info, e := encapdoc.InspectPayload(ctx, profile.media, data, encapdoc.PayloadLimits{MaxBytes: max, MaxDepth: min(s.limits.MaxJSONDepth, 256), MaxRecords: min(s.limits.MaxJSONValues, 1000000)})
	if e != nil {
		if errors.Is(e, context.Canceled) || errors.Is(e, context.DeadlineExceeded) {
			return instance, syntax, "", "", e
		}
		if errors.Is(e, encapdoc.ErrEncapsulatedDocumentTooLarge) {
			return instance, syntax, "", "", ErrResourceLimit
		}
		return instance, syntax, "", "", ErrInvalidRequest
	}
	if info.HL7InstanceIdentifier != "" {
		if e = storeWholeString(obj, core.NewTag(0x0040, 0xe001), core.VRST, info.HL7InstanceIdentifier, true); e != nil {
			return instance, syntax, "", "", e
		}
	}
	if profile.modality == "M3D" {
		if profile.sop != encapdoc.EncapsulatedMTLStorage {
			if e = storeWholeUID(obj, core.NewTag(0x0020, 0x0052)); e != nil {
				return instance, syntax, "", "", e
			}
			if _, ok := obj.Get(core.NewTag(0x0020, 0x1040)); !ok {
				return instance, syntax, "", "", ErrInvalidRequest
			}
		}
		if e = storeWholeSequence(obj, core.NewTag(0x0040, 0x08ea), 1, 1); e != nil {
			return instance, syntax, "", "", e
		}
		items, _ := obj.GetSequence(core.NewTag(0x0040, 0x08ea))
		for _, tag := range []core.Tag{core.NewTag(0x0008, 0x0100), core.NewTag(0x0008, 0x0102), core.NewTag(0x0008, 0x0104)} {
			if _, e = storeRequiredString(items[0], tag); e != nil {
				return instance, syntax, "", "", e
			}
		}
	}
	lengthTag := core.NewTag(0x0042, 0x0015)
	if element, exists := obj.Get(lengthTag); exists {
		n, e := obj.GetInt(lengthTag)
		size, known := element.Value.EncodedLength()
		if e != nil || element.VR() != core.VRUL || !known || size != 4 || n != part.size {
			return instance, syntax, "", "", ErrInvalidRequest
		}
	} else {
		v := make([]byte, 4)
		binary.LittleEndian.PutUint32(v, uint32(part.size))
		obj.Put(core.NewRawElement(lengthTag, core.VRUL, v))
	}
	instance.object = obj
	return instance, syntax, "", ref, nil
}

func storeInstanceUsesWholeBulk(instance storeMetadataInstance, bulk map[string]stagedStoreMIMEPart) bool {
	if element, ok := instance.object.Get(storeDocumentTag); ok {
		if _, isBulk := element.Value.(core.BulkDataValue); isBulk {
			return true
		}
	}
	for _, ref := range instance.refs {
		if part, ok := bulk[ref]; ok {
			if _, whole := storeWholeProfile(part.contentType); whole || part.semanticFailure {
				return true
			}
		}
	}
	return false
}
func (s *Server) failedWholeStoreInstance(instance storeMetadataInstance, routeStudyUID string) (stagedStorePart, error) {
	var candidate stagedStorePart
	candidate.preflightFailure = 0xc000
	fields := []struct {
		tag   core.Tag
		value *string
	}{{core.NewTag(0x0008, 0x0016), &candidate.sopClassUID}, {core.NewTag(0x0008, 0x0018), &candidate.ref.SOPInstanceUID}, {core.NewTag(0x0020, 0x000d), &candidate.ref.StudyInstanceUID}, {core.NewTag(0x0020, 0x000e), &candidate.ref.SeriesInstanceUID}}
	for _, f := range fields {
		v, err := storeRequiredString(instance.object, f.tag)
		if err != nil || s.validateUID(v) != nil {
			return stagedStorePart{}, ErrInvalidRequest
		}
		*f.value = v
	}
	if routeStudyUID != "" && candidate.ref.StudyInstanceUID != routeStudyUID {
		return stagedStorePart{}, ErrConflict
	}
	return candidate, nil
}

func validateStoreWholeMetadata(obj *object.Object, element core.Element, p storeWholeBulkProfile) error {
	sop, err := storeRequiredString(obj, core.NewTag(0x0008, 0x0016))
	if err != nil {
		return err
	}
	if element.VR() != core.VROB {
		return ErrInvalidRequest
	}
	if p.video {
		if element.Tag() != core.TagPixelData || sop != "1.2.840.10008.5.1.4.1.1.77.1.1.1" && sop != "1.2.840.10008.5.1.4.1.1.77.1.2.1" && sop != "1.2.840.10008.5.1.4.1.1.77.1.4.1" {
			return ErrInvalidRequest
		}
		return nil
	}
	if element.Tag() != storeDocumentTag || sop != p.sop {
		return ErrInvalidRequest
	}
	text, err := storeRequiredString(obj, storeDocumentMIMETag)
	if err != nil {
		return err
	}
	media, params, err := mime.ParseMediaType(text)
	if err != nil || strings.ToLower(media) != p.media || len(params) != 0 {
		return ErrInvalidRequest
	}
	return nil
}

func storeWholeCode(obj *object.Object, tag core.Tag) error {
	if err := storeWholeSequence(obj, tag, 1, 1); err != nil {
		return err
	}
	items, ok := obj.GetSequence(tag)
	if !ok || len(items) != 1 {
		return ErrInvalidRequest
	}
	for _, codeTag := range []core.Tag{core.NewTag(0x0008, 0x0100), core.NewTag(0x0008, 0x0102), core.NewTag(0x0008, 0x0104)} {
		if _, err := storeRequiredString(items[0], codeTag); err != nil {
			return err
		}
	}
	return nil
}
