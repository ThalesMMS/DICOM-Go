package printmgmt

import (
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"sort"
)

// SupportedAbstractSyntaxes is the service's finite optional negotiation list.
// A server still owns actual association acceptance; Bind verifies that the
// optional classes were accepted separately before allowing their objects.
func (s *Service) SupportedAbstractSyntaxes() []string {
	out := []string{GrayscaleMetaSOPClassUID, ColorMetaSOPClassUID, PrinterSOPClassUID}
	if s.native != nil {
		if s.native.PresentationLUT {
			out = append(out, PresentationLUTSOPClassUID)
		}
		if len(s.native.AnnotationFormats) > 0 {
			out = append(out, AnnotationBoxSOPClassUID)
		}
	}
	return out
}
func (b *Binding) supportsLUT() bool {
	return b.service.native != nil && b.service.native.PresentationLUT && b.nativeContexts[PresentationLUTSOPClassUID]
}
func (b *Binding) supportsAnnotation() bool {
	return b.service.native != nil && len(b.service.native.AnnotationFormats) > 0 && b.nativeContexts[AnnotationBoxSOPClassUID]
}

func captureNativeCharset(ds *object.Object, prior string) string {
	if ds == nil {
		return prior
	}
	value, present, err := presentationText(ds, TagSpecificCharacterSet, core.VRCS)
	if err != nil {
		return "UNSUPPORTED"
	}
	if present {
		return value
	}
	return prior
}

func (b *Binding) nativeUIDUsed(uid string) bool {
	if _, ok := b.luts[uid]; ok {
		return true
	}
	if b.session == nil {
		return false
	}
	if b.session.uid == uid {
		return true
	}
	for _, f := range b.session.boxes {
		if f.SOPInstanceUID == uid {
			return true
		}
		for _, i := range f.ImageBoxes {
			if i.SOPInstanceUID == uid {
				return true
			}
		}
		for _, a := range f.AnnotationBoxes {
			if a.SOPInstanceUID == uid {
				return true
			}
		}
	}
	return false
}

func (b *Binding) createPresentationLUT(req dimse.NormalizedCreateRequest, ds *object.Object) (dimse.NormalizedCreateSCPResult, error) {
	if b.closed {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusProcessingFailure, "", errState)
	}
	if !b.supportsLUT() {
		return dimse.NormalizedCreateSCPResult{}, failure(0x0122, "", ErrNativeUnsupported)
	}
	if len(b.luts) >= b.service.native.Limits.MaxPresentationLUTs {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusResourceLimitation, "", ErrInvalidAttributes)
	}
	used := 0
	for _, p := range b.luts {
		used += 2 * len(p.LUT.Entries)
	}
	p, err := parsePresentationLUTBudget(ds, b.service.native.Limits.MaxLUTBytes-used)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	uid, err := b.newUID(req.AffectedSOPInstanceUID)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	if !core.IsValidUID(uid) {
		return dimse.NormalizedCreateSCPResult{}, failure(0x0117, "", ErrInvalidAttributes)
	}
	if b.nativeUIDUsed(uid) {
		return dimse.NormalizedCreateSCPResult{}, failure(0x0111, "", errState)
	}
	if b.luts == nil {
		b.luts = map[string]PresentationLUTInstance{}
	}
	b.luts[uid] = PresentationLUTInstance{SOPInstanceUID: uid, LUT: p}
	reply, _ := BuildPresentationLUT(p)
	response := dimse.NormalizedCreateResponse{Status: StatusSuccess, AffectedSOPInstanceUID: uid}
	r := newReader(ds)
	r.used[TagPresentationLUTShape] = true
	r.used[TagPresentationLUTSequence] = true
	ignored := r.ignored()
	if items, ok := ds.GetSequence(TagPresentationLUTSequence); ok && len(items) == 1 {
		nested := newReader(items[0])
		nested.used[TagLUTDescriptor] = true
		nested.used[TagLUTData] = true
		nested.used[TagLUTExplanation] = true
		ignored = append(ignored, nested.ignored()...)
	}
	if len(ignored) > 0 {
		response.Status, response.StatusFields = warning(StatusAttributeListError, ignored)
	}
	return dimse.NormalizedCreateSCPResult{Response: response, DataSet: reply}, nil
}

func (b *Binding) deletePresentationLUT(uid string) error {
	if !b.supportsLUT() {
		return failure(0x0122, "", ErrNativeUnsupported)
	}
	if _, ok := b.luts[uid]; !ok {
		return errUnknownBox
	}
	if b.session != nil {
		for _, f := range b.session.boxes {
			if f.Presentation.PresentationLUTUID == uid {
				return failure(StatusProcessingFailure, "", errState, TagReferencedPresentationLUT)
			}
			for _, i := range f.ImageBoxes {
				if i.PresentationLUTUID == uid {
					return failure(StatusProcessingFailure, "", errState, TagReferencedPresentationLUT)
				}
			}
		}
	}
	delete(b.luts, uid)
	return nil
}

func nativeUID(ds *object.Object, tag core.Tag) (string, error) {
	e, present := ds.Get(tag)
	if !present || e.Header.VR != core.VRUI {
		return "", invalid(tag)
	}
	var s string
	switch v := e.Value.(type) {
	case core.RawValue:
		if len(v) > 64 {
			return "", invalid(tag)
		}
		s = string(v)
	case core.StringValue:
		if len(v) != 1 || len(v[0]) > 64 {
			return "", invalid(tag)
		}
		s = v[0]
	default:
		return "", invalid(tag)
	}
	if len(s) > 0 && s[len(s)-1] == 0 {
		s = s[:len(s)-1]
	}
	if !core.IsValidUID(s) {
		return "", invalid(tag)
	}
	return s, nil
}

func readLUTReference(r *attributeReader, prior string) (string, error) {
	e, present := r.ds.Get(TagReferencedPresentationLUT)
	if !present {
		return prior, nil
	}
	r.used[TagReferencedPresentationLUT] = true
	seq, ok := e.Value.(core.SequenceValue)
	if e.Header.VR != core.VRSQ || !ok || len(seq.Items) > 1 {
		return "", invalid(TagReferencedPresentationLUT)
	}
	if len(seq.Items) == 0 {
		return "", nil
	} // explicit removal of an optional reference
	items, _ := r.ds.GetSequence(TagReferencedPresentationLUT)
	class, err := nativeUID(items[0], TagReferencedSOPClassUID)
	if err != nil || class != PresentationLUTSOPClassUID {
		return "", invalid(TagReferencedPresentationLUT, TagReferencedSOPClassUID)
	}
	uid, err := nativeUID(items[0], TagReferencedSOPInstanceUID)
	if err != nil {
		return "", invalid(TagReferencedPresentationLUT, TagReferencedSOPInstanceUID)
	}
	return uid, nil
}

func (b *Binding) imageLUTReference(r *attributeReader, prior string) (string, error) {
	if r.has(TagReferencedPresentationLUT) && !b.supportsLUT() {
		return "", invalid(TagReferencedPresentationLUT)
	}
	return readLUTReference(r, prior)
}

func (b *Binding) validateLUTImage(film FilmBoxPresentation, imageRef string, img *Image) error {
	ref := imageRef
	if ref == "" {
		ref = film.PresentationLUTUID
	}
	if ref == "" {
		return nil
	}
	if !b.supportsLUT() {
		return invalid(TagReferencedPresentationLUT)
	}
	p, ok := b.luts[ref]
	if !ok {
		return invalid(TagReferencedPresentationLUT, TagReferencedSOPInstanceUID)
	}
	if _, err := film.NativePrintDensity(); err != nil {
		return err
	}
	return p.LUT.validateImage(img)
}

func (b *Binding) configureNativeFilm(r *attributeReader, box *FilmBox, create bool) error {
	p := box.Presentation
	if b.supportsLUT() {
		for _, tag := range []core.Tag{TagMinDensity, TagMaxDensity, TagIllumination, TagReflectedAmbientLight} {
			if r.has(tag) {
				if _, err := nativeUSValues(r.ds, tag, 1); err != nil {
					return err
				}
			}
		}
	}
	if b.supportsAnnotation() && r.has(TagSpecificCharacterSet) {
		cs, _, err := presentationText(r.ds, TagSpecificCharacterSet, core.VRCS)
		if err != nil || cs != b.session.presentation.SpecificCharacterSet {
			return invalid(TagSpecificCharacterSet)
		}
		r.used[TagSpecificCharacterSet] = true
	}
	ref, err := b.imageLUTReference(r, p.PresentationLUTUID)
	if err != nil {
		return err
	}
	p.PresentationLUTUID = ref
	if p.PresentationLUTUID != "" {
		if box.Color {
			return invalid(TagReferencedPresentationLUT)
		}
		if err := b.validateLUTImage(p, "", nil); err != nil {
			return err
		}
	}
	for _, i := range box.ImageBoxes {
		if err := b.validateLUTImage(p, i.PresentationLUTUID, i.Image); err != nil {
			return err
		}
	}
	if r.has(TagAnnotationDisplayFormatID) && b.supportsAnnotation() {
		if !create {
			return invalid(TagAnnotationDisplayFormatID)
		}
		value, present, err := presentationText(r.ds, TagAnnotationDisplayFormatID, core.VRCS)
		if err != nil {
			return err
		}
		if present {
			p.AnnotationDisplayFormatID = value
			r.used[TagAnnotationDisplayFormatID] = true
		}
	}
	if create && p.AnnotationDisplayFormatID != "" {
		if !b.supportsAnnotation() {
			return invalid(TagAnnotationDisplayFormatID)
		}
		if !supportedAnnotationCharset(b.session.presentation.SpecificCharacterSet) {
			return invalid(TagSpecificCharacterSet)
		}
		slots := 0
		for _, f := range b.service.native.AnnotationFormats {
			if f.ID == p.AnnotationDisplayFormatID {
				slots = f.Slots
			}
		}
		if slots == 0 {
			return invalid(TagAnnotationDisplayFormatID)
		}
		box.AnnotationBoxes = make([]AnnotationBox, 0, slots)
		seen := map[string]bool{box.SOPInstanceUID: true}
		for i := 1; i <= slots; i++ {
			uid, err := b.service.uids()
			if err != nil {
				return err
			}
			if !core.IsValidUID(uid) {
				return failure(0x0117, "", ErrInvalidAttributes)
			}
			if seen[uid] || b.nativeUIDUsed(uid) {
				return failure(0x0111, "", errState)
			}
			seen[uid] = true
			box.AnnotationBoxes = append(box.AnnotationBoxes, AnnotationBox{SOPInstanceUID: uid})
		}
	}
	box.Presentation = p
	return nil
}

func (b *Binding) setAnnotationBox(req dimse.NormalizedSetRequest, ds *object.Object) (uint16, *dimse.NormalizedStatusFields, error) {
	if !b.supportsAnnotation() {
		return 0, nil, failure(0x0122, "", ErrNativeUnsupported)
	}
	if ds == nil || b.session == nil || len(b.session.boxes) == 0 {
		return 0, nil, errUnknownBox
	}
	last := b.session.boxes[len(b.session.boxes)-1]
	var target *AnnotationBox
	for i := range last.AnnotationBoxes {
		if last.AnnotationBoxes[i].SOPInstanceUID == req.RequestedSOPInstanceUID {
			target = &last.AnnotationBoxes[i]
		}
	}
	if target == nil {
		for _, f := range b.session.boxes[:len(b.session.boxes)-1] {
			for _, a := range f.AnnotationBoxes {
				if a.SOPInstanceUID == req.RequestedSOPInstanceUID {
					return 0, nil, failure(StatusProcessingFailure, "", errState)
				}
			}
		}
		return 0, nil, errUnknownBox
	}
	position, err := nativeUSValues(ds, TagAnnotationPosition, 1)
	if err != nil {
		return 0, nil, err
	}
	if position[0] < 1 || int(position[0]) > len(last.AnnotationBoxes) {
		return 0, nil, invalid(TagAnnotationPosition)
	}
	for _, other := range last.AnnotationBoxes {
		if other.SOPInstanceUID != target.SOPInstanceUID && other.Position == int(position[0]) {
			return 0, nil, invalid(TagAnnotationPosition)
		}
	}
	value, present, err := nativeText(ds, TagAnnotationText, b.session.presentation.SpecificCharacterSet)
	if err != nil {
		return 0, nil, err
	}
	if present {
		target.Text = value
	}
	target.Position = int(position[0])
	r := newReader(ds)
	r.used[TagAnnotationPosition] = true
	r.used[TagAnnotationText] = true
	if ignored := r.ignored(); len(ignored) > 0 {
		status, fields := warning(StatusAttributeListError, ignored)
		return status, fields, nil
	}
	return StatusSuccess, nil, nil
}

func (b *Binding) jobLUTs(boxes []*FilmBox) []PresentationLUTInstance {
	refs := map[string]bool{}
	for _, f := range boxes {
		if f.Presentation.PresentationLUTUID != "" {
			refs[f.Presentation.PresentationLUTUID] = true
		}
		for _, i := range f.ImageBoxes {
			if i.PresentationLUTUID != "" {
				refs[i.PresentationLUTUID] = true
			}
		}
	}
	keys := make([]string, 0, len(refs))
	for k := range refs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]PresentationLUTInstance, 0, len(keys))
	for _, uid := range keys {
		out = append(out, b.luts[uid].clone())
	}
	return out
}

func presentationLUTReference(uid string) core.Element {
	return sequenceOf(TagReferencedPresentationLUT, referenceItem(PresentationLUTSOPClassUID, uid))
}

// Native references are intentionally supported at Film Box/Image Box only.
// A Film Session reference remains unused, producing 0107H; required-native
// PrintFilm clients stop before N-ACTION instead of promising session scope.
