package printmgmt

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

// MaxImageBoxesCeiling is the largest Limits.MaxImageBoxes NewService
// accepts. It bounds the image boxes (and SOP Instance UIDs) that one Film Box
// N-CREATE can allocate, whatever columns and rows the peer requests.
const MaxImageBoxesCeiling = 4096

// Limits bound one association's film session. Zero fields select defaults.
type Limits struct {
	MaxFilmBoxes    int           // per film session (default 16)
	MaxImageBoxes   int           // per film box (default 64, at most MaxImageBoxesCeiling)
	MaxDimension    int           // rows or columns of an image (default 8192)
	MaxImageBytes   int64         // one image (default 64 MiB)
	MaxSessionBytes int64         // all images of the session (default 512 MiB)
	MaxCopies       int           // Number of Copies (default 10)
	PrintTimeout    time.Duration // one Backend.Print call (default 30 s)
}

// ServiceOptions configure a Service.
type ServiceOptions struct {
	Limits Limits
	// Physical opts into explicit backend-supported physical presentation.
	Physical *PhysicalServiceOptions
	Native   *NativeServiceOptions
	Clock    func() time.Time
	// UIDGenerator assigns SOP Instance UIDs; nil uses random 2.25 UIDs.
	UIDGenerator func() (string, error)
}

// Service is a Print Management SCP. It holds no association state; Bind
// creates the state of one association.
type Service struct {
	backend  Backend
	limits   Limits
	clock    func() time.Time
	uids     func() (string, error)
	physical *PhysicalServiceOptions
	native   *NativeServiceOptions
}

// NewService returns an SCP that prints through backend.
func NewService(backend Backend, options ServiceOptions) (*Service, error) {
	if backend == nil {
		return nil, ErrInvalidAttributes
	}
	native, nativeErr := normalizeNativeOptions(backend, options.Native)
	if nativeErr != nil {
		return nil, nativeErr
	}
	var physical *PhysicalServiceOptions
	if options.Physical != nil {
		copy := *options.Physical
		capable, ok := backend.(PhysicalBackend)
		if !ok || !capable.HonorsPhysicalImageBoxPlans() || copy.Area == nil || copy.Capabilities.Validate() != nil {
			return nil, ErrPhysicalUnsupported
		}
		if copy.DefaultBehavior != "" && copy.DefaultBehavior != Decimate && copy.DefaultBehavior != Crop && copy.DefaultBehavior != Fail {
			return nil, ErrInvalidAttributes
		}
		physical = &copy
	}
	l := options.Limits
	if l.MaxFilmBoxes < 0 || l.MaxImageBoxes < 0 || l.MaxDimension < 0 || l.MaxImageBytes < 0 || l.MaxSessionBytes < 0 || l.MaxCopies < 0 || l.PrintTimeout < 0 || l.MaxImageBoxes > MaxImageBoxesCeiling {
		return nil, ErrInvalidAttributes
	}
	defaults := Limits{16, 64, 8192, 64 << 20, 512 << 20, 10, 30 * time.Second}
	if l.MaxFilmBoxes == 0 {
		l.MaxFilmBoxes = defaults.MaxFilmBoxes
	}
	if l.MaxImageBoxes == 0 {
		l.MaxImageBoxes = defaults.MaxImageBoxes
	}
	if l.MaxDimension == 0 {
		l.MaxDimension = defaults.MaxDimension
	}
	if l.MaxImageBytes == 0 {
		l.MaxImageBytes = defaults.MaxImageBytes
	}
	if l.MaxSessionBytes == 0 {
		l.MaxSessionBytes = defaults.MaxSessionBytes
	}
	if l.MaxCopies == 0 {
		l.MaxCopies = defaults.MaxCopies
	}
	if l.PrintTimeout == 0 {
		l.PrintTimeout = defaults.PrintTimeout
	}
	s := &Service{backend: backend, limits: l, clock: options.Clock, uids: options.UIDGenerator, physical: physical, native: native}
	if s.clock == nil {
		s.clock = time.Now
	}
	if s.uids == nil {
		s.uids = core.NewUID
	}
	return s, nil
}

// Binding is the Print Management state of one association: at most one
// Film Session (PS3.4 H.4.1.2.1.3). Close deletes it, as releasing or
// aborting the association does (H.5); jobs already handed to the Backend are
// unaffected.
type Binding struct {
	service        *Service
	calling        string
	mu             sync.Mutex
	session        *filmSession
	closed         bool
	nativeContexts map[string]bool
	luts           map[string]PresentationLUTInstance
}

type filmSession struct {
	uid          string
	presentation FilmSessionPresentation
	boxes        []*FilmBox
}

// Bind creates the state for one association, which is borrowed only for
// its calling AE Title.
func (s *Service) Bind(assoc *ul.Association) *Binding {
	b := &Binding{service: s, nativeContexts: map[string]bool{}}
	if assoc != nil {
		b.calling = strings.TrimSpace(assoc.CallingAETitle)
		for _, pc := range assoc.AcceptedContexts {
			if pc.AbstractSyntaxUID == PresentationLUTSOPClassUID || pc.AbstractSyntaxUID == AnnotationBoxSOPClassUID {
				b.nativeContexts[pc.AbstractSyntaxUID] = true
			}
		}
	}
	return b
}

// Close deletes the film session hierarchy of the association.
func (b *Binding) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.session = nil
	b.luts = nil
}

// metaContext is the Meta SOP Class of the request's presentation context, or
// "" for a Printer context.
func metaContext(ctx context.Context) string {
	info, ok := dimse.NormalizedRequestInfoFromContext(ctx)
	if !ok {
		return ""
	}
	return info.PresentationContext.AbstractSyntaxUID
}

func imageBoxClass(meta string) string {
	if meta == ColorMetaSOPClassUID {
		return ColorImageBoxUID
	}
	return GrayscaleImageBoxUID
}

// allowedCommand is the operation matrix: which command SOP Class may travel
// on which negotiated abstract syntax.
func allowedCommand(abstract, command string) bool {
	switch abstract {
	case GrayscaleMetaSOPClassUID:
		return command == FilmSessionSOPClassUID || command == FilmBoxSOPClassUID || command == GrayscaleImageBoxUID || command == PrinterSOPClassUID
	case ColorMetaSOPClassUID:
		return command == FilmSessionSOPClassUID || command == FilmBoxSOPClassUID || command == ColorImageBoxUID || command == PrinterSOPClassUID
	case PrinterSOPClassUID:
		return command == PrinterSOPClassUID
	case PresentationLUTSOPClassUID, AnnotationBoxSOPClassUID:
		return command == abstract
	}
	return false
}

func failure(status uint16, comment string, err error, tags ...core.Tag) error {
	return &StatusError{Status: status, Tags: tags, ErrorComment: comment, Err: err}
}

var (
	errState      = errors.New("printmgmt: operation not allowed in the current state")
	errUnknownBox = errors.New("printmgmt: unknown instance")
)

func statusOf(err error) uint16 {
	var statusErr *StatusError
	switch {
	case err == nil:
		return StatusSuccess
	case errors.As(err, &statusErr):
		return statusErr.Status
	case errors.Is(err, errUnknownBox):
		return StatusNoSuchSOPInstance
	default:
		return StatusProcessingFailure
	}
}

func fieldsOf(err error) *dimse.NormalizedStatusFields {
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.ErrorComment == "" && len(statusErr.Tags) == 0 {
		return nil
	}
	return &dimse.NormalizedStatusFields{ErrorComment: statusErr.ErrorComment, AttributeIdentifierList: append([]core.Tag(nil), statusErr.Tags...)}
}

// warning returns a warning status (no error) with the attributes involved.
func warning(status uint16, tags []core.Tag) (uint16, *dimse.NormalizedStatusFields) {
	return status, &dimse.NormalizedStatusFields{AttributeIdentifierList: tags}
}

// NormalizedOptions returns the handlers for the association.
func (b *Binding) NormalizedOptions() dimse.NormalizedSCPOptions {
	return dimse.NormalizedSCPOptions{
		MaxDataSetBytes: b.service.limits.MaxImageBytes + 1<<20,
		PresentationContextPolicy: func(pc ul.AcceptedContext, command string) error {
			if !allowedCommand(pc.AbstractSyntaxUID, command) {
				return &dimse.NormalizedPresentationContextError{PresentationContextID: pc.ID, AbstractSyntaxUID: pc.AbstractSyntaxUID, CommandSOPClassUID: command}
			}
			return nil
		},
		CreateHandler: b.create,
		SetHandler:    b.set,
		ActionHandler: b.action,
		DeleteHandler: b.delete,
		GetHandler:    b.get,
	}
}

func (b *Binding) create(ctx context.Context, request dimse.NormalizedCreateRequest, attributes *object.Object) (dimse.NormalizedCreateSCPResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var (
		result dimse.NormalizedCreateSCPResult
		err    error
	)
	switch request.AffectedSOPClassUID {
	case FilmSessionSOPClassUID:
		result, err = b.createSession(request, attributes)
	case FilmBoxSOPClassUID:
		result, err = b.createFilmBox(ctx, request, attributes)
	case PresentationLUTSOPClassUID:
		result, err = b.createPresentationLUT(request, attributes)
	default:
		err = failure(dimse.StatusUnrecognizedOperation, "", errState)
	}
	if err != nil {
		return dimse.NormalizedCreateSCPResult{Response: dimse.NormalizedCreateResponse{Status: statusOf(err), StatusFields: fieldsOf(err)}}, err
	}
	return result, nil
}

func (b *Binding) newUID(requested string) (string, error) {
	if uid := strings.TrimSpace(requested); uid != "" {
		if !core.IsValidUID(uid) {
			return "", failure(0x0117, "", ErrInvalidAttributes)
		}
		return uid, nil
	}
	return b.service.uids()
}

func (b *Binding) createSession(request dimse.NormalizedCreateRequest, attributes *object.Object) (dimse.NormalizedCreateSCPResult, error) {
	if b.closed || b.session != nil {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusProcessingFailure, "Only one Film Session per association", errState)
	}
	presentation, memory, ignored, err := parseFilmSession(attributes, FilmSessionPresentation{NumberOfCopies: 1, PrintPriority: "MED", MediumType: "PAPER", FilmDestination: "MAGAZINE"}, b.service.limits.MaxCopies)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	presentation.SpecificCharacterSet = captureNativeCharset(attributes, "")
	uid, err := b.newUID(request.AffectedSOPInstanceUID)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	if b.service.native != nil {
		if !core.IsValidUID(uid) {
			return dimse.NormalizedCreateSCPResult{}, failure(0x0117, "", ErrInvalidAttributes)
		}
		if b.nativeUIDUsed(uid) {
			return dimse.NormalizedCreateSCPResult{}, failure(0x0111, "", errState)
		}
	}
	b.session = &filmSession{uid: uid, presentation: presentation}
	response := dimse.NormalizedCreateResponse{Status: StatusSuccess, AffectedSOPInstanceUID: uid}
	switch {
	case memory:
		response.Status, response.StatusFields = warning(StatusMemoryAllocationNotSupport, []core.Tag{TagMemoryAllocation})
	case len(ignored) > 0:
		response.Status, response.StatusFields = warning(StatusAttributeListError, ignored)
	}
	return dimse.NormalizedCreateSCPResult{Response: response}, nil
}

func (b *Binding) createFilmBox(ctx context.Context, request dimse.NormalizedCreateRequest, attributes *object.Object) (dimse.NormalizedCreateSCPResult, error) {
	meta := metaContext(ctx)
	if meta != GrayscaleMetaSOPClassUID && meta != ColorMetaSOPClassUID {
		return dimse.NormalizedCreateSCPResult{}, failure(dimse.StatusUnrecognizedOperation, "", errState)
	}
	if b.session == nil {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusProcessingFailure, "No Film Session", errState)
	}
	if len(b.session.boxes) >= b.service.limits.MaxFilmBoxes {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusResourceLimitation, "", ErrInvalidAttributes)
	}
	if attributes == nil {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusMissingAttribute, "", ErrInvalidAttributes, TagImageDisplayFormat, TagReferencedFilmSessionSequence)
	}
	r := newReader(attributes)
	format, ok, err := r.text(TagImageDisplayFormat)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	if !ok {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusMissingAttribute, "", ErrInvalidAttributes, TagImageDisplayFormat)
	}
	columns, rows, err := parseDisplayFormat(format, b.service.limits.MaxImageBoxes)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	r.used[TagReferencedFilmSessionSequence] = true
	references, present := attributes.GetSequence(TagReferencedFilmSessionSequence)
	if !present {
		return dimse.NormalizedCreateSCPResult{}, failure(StatusMissingAttribute, "", ErrInvalidAttributes, TagReferencedFilmSessionSequence)
	}
	if len(references) != 1 {
		return dimse.NormalizedCreateSCPResult{}, invalid(TagReferencedFilmSessionSequence)
	}
	class, _ := references[0].GetString(TagReferencedSOPClassUID)
	instance, _ := references[0].GetString(TagReferencedSOPInstanceUID)
	if strings.TrimRight(class, "\x00 ") != FilmSessionSOPClassUID || strings.TrimRight(instance, "\x00 ") != b.session.uid {
		return dimse.NormalizedCreateSCPResult{}, invalid(TagReferencedFilmSessionSequence)
	}
	color := meta == ColorMetaSOPClassUID
	presentation, err := parseFilmBoxPresentationNative(r, FilmBoxPresentation{FilmOrientation: "PORTRAIT", FilmSizeID: "8INX10IN", MagnificationType: "REPLICATE", Trim: "NO"}, color, b.supportsLUT())
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	uid, err := b.newUID(request.AffectedSOPInstanceUID)
	if err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	box := &FilmBox{SOPInstanceUID: uid, Color: color, Columns: columns, Rows: rows, Presentation: presentation}
	if b.service.native != nil {
		if !core.IsValidUID(uid) {
			return dimse.NormalizedCreateSCPResult{}, failure(0x0117, "", ErrInvalidAttributes)
		}
		if b.nativeUIDUsed(uid) {
			return dimse.NormalizedCreateSCPResult{}, failure(0x0111, "", errState)
		}
	}
	if err := b.configureNativeFilm(r, box, true); err != nil {
		return dimse.NormalizedCreateSCPResult{}, err
	}
	class = imageBoxClass(meta)
	items := make([]core.DataSet, 0, columns*rows)
	seenUIDs := map[string]bool{uid: true}
	for _, a := range box.AnnotationBoxes {
		seenUIDs[a.SOPInstanceUID] = true
	}
	for position := 1; position <= columns*rows; position++ {
		boxUID, err := b.service.uids()
		if err != nil {
			return dimse.NormalizedCreateSCPResult{}, err
		}
		if b.service.native != nil {
			if !core.IsValidUID(boxUID) {
				return dimse.NormalizedCreateSCPResult{}, failure(0x0117, "", ErrInvalidAttributes)
			}
			if seenUIDs[boxUID] || b.nativeUIDUsed(boxUID) {
				return dimse.NormalizedCreateSCPResult{}, failure(0x0111, "", errState)
			}
			seenUIDs[boxUID] = true
		}
		box.ImageBoxes = append(box.ImageBoxes, ImageBox{SOPClassUID: class, SOPInstanceUID: boxUID, Position: position})
		items = append(items, referenceItem(class, boxUID))
	}
	b.session.boxes = append(b.session.boxes, box)
	reply := object.FromElements(append(attributes.Elements(), sequenceOf(TagReferencedImageBoxSequence, items...)), std.Dictionary)
	if b.supportsAnnotation() {
		refs := make([]core.DataSet, len(box.AnnotationBoxes))
		for i, a := range box.AnnotationBoxes {
			refs[i] = referenceItem(AnnotationBoxSOPClassUID, a.SOPInstanceUID)
		}
		reply.Put(sequenceOf(TagReferencedAnnotationBoxSequence, refs...))
	}
	response := dimse.NormalizedCreateResponse{Status: StatusSuccess, AffectedSOPInstanceUID: uid}
	if ignored := r.ignored(); len(ignored) > 0 {
		response.Status, response.StatusFields = warning(StatusAttributeListError, ignored)
	}
	return dimse.NormalizedCreateSCPResult{Response: response, DataSet: reply}, nil
}

func (b *Binding) lastBox(uid string) (*FilmBox, error) {
	if b.session == nil || len(b.session.boxes) == 0 {
		return nil, errUnknownBox
	}
	last := b.session.boxes[len(b.session.boxes)-1]
	if last.SOPInstanceUID == uid {
		return last, nil
	}
	for _, box := range b.session.boxes {
		if box.SOPInstanceUID == uid {
			// PS3.4 H.4.2.2.2/3/4: only the last created Film Box.
			return nil, failure(StatusProcessingFailure, "Only the last created Film Box can be addressed", errState)
		}
	}
	return nil, errUnknownBox
}

func (b *Binding) set(ctx context.Context, request dimse.NormalizedSetRequest, modifications *object.Object) (dimse.NormalizedSetSCPResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	status, fields, err := b.setLocked(ctx, request, modifications)
	if err != nil {
		return dimse.NormalizedSetSCPResult{Response: dimse.NormalizedSetResponse{Status: statusOf(err), StatusFields: fieldsOf(err)}}, err
	}
	return dimse.NormalizedSetSCPResult{Response: dimse.NormalizedSetResponse{Status: status, StatusFields: fields}}, nil
}

func (b *Binding) setLocked(ctx context.Context, request dimse.NormalizedSetRequest, modifications *object.Object) (uint16, *dimse.NormalizedStatusFields, error) {
	if request.RequestedSOPClassUID == PresentationLUTSOPClassUID {
		return 0, nil, failure(dimse.StatusUnrecognizedOperation, "", errState)
	}
	if b.session == nil {
		return 0, nil, errUnknownBox
	}
	switch request.RequestedSOPClassUID {
	case FilmSessionSOPClassUID:
		if request.RequestedSOPInstanceUID != b.session.uid {
			return 0, nil, errUnknownBox
		}
		presentation, memory, ignored, err := parseFilmSession(modifications, b.session.presentation, b.service.limits.MaxCopies)
		if err != nil {
			return 0, nil, err
		}
		presentation.SpecificCharacterSet = captureNativeCharset(modifications, b.session.presentation.SpecificCharacterSet)
		if presentation.SpecificCharacterSet != b.session.presentation.SpecificCharacterSet {
			for _, box := range b.session.boxes {
				if len(box.AnnotationBoxes) > 0 {
					return 0, nil, invalid(TagSpecificCharacterSet)
				}
			}
		}
		b.session.presentation = presentation
		if memory {
			status, fields := warning(StatusMemoryAllocationNotSupport, []core.Tag{TagMemoryAllocation})
			return status, fields, nil
		}
		if len(ignored) > 0 {
			status, fields := warning(StatusAttributeListError, ignored)
			return status, fields, nil
		}
		return StatusSuccess, nil, nil
	case FilmBoxSOPClassUID:
		box, err := b.lastBox(request.RequestedSOPInstanceUID)
		if err != nil {
			return 0, nil, err
		}
		r := newReader(modifications)
		presentation, err := parseFilmBoxPresentationNative(r, box.Presentation, box.Color, b.supportsLUT())
		if err != nil {
			return 0, nil, err
		}
		candidate := *box
		candidate.Presentation = presentation
		if err := b.configureNativeFilm(r, &candidate, false); err != nil {
			return 0, nil, err
		}
		presentation = candidate.Presentation
		var plans []PhysicalPlan
		if b.service.physical != nil {
			plans = make([]PhysicalPlan, len(box.ImageBoxes))
			for i, imageBox := range box.ImageBoxes {
				if err := ctx.Err(); err != nil {
					return 0, nil, err
				}
				if !imageBox.Presentation.Specified() {
					continue
				}
				plan, _, planErr := b.resolvePhysicalPlan(ctx, presentation, box.Columns, box.Rows, imageBox.Position, imageBox.Image, imageBox.Presentation)
				if planErr != nil {
					return 0, nil, planErr
				}
				plans[i] = plan
			}
		}
		box.Presentation = presentation
		if plans != nil {
			for i := range plans {
				box.ImageBoxes[i].Plan = plans[i]
			}
		}

		if ignored := r.ignored(); len(ignored) > 0 {
			status, fields := warning(StatusAttributeListError, ignored)
			return status, fields, nil
		}
		return StatusSuccess, nil, nil
	case GrayscaleImageBoxUID, ColorImageBoxUID:
		return b.setImageBox(ctx, request, modifications)
	case AnnotationBoxSOPClassUID:
		return b.setAnnotationBox(request, modifications)
	}
	return 0, nil, failure(dimse.StatusUnrecognizedOperation, "", errState)
}

func (b *Binding) setImageBox(ctx context.Context, request dimse.NormalizedSetRequest, modifications *object.Object) (uint16, *dimse.NormalizedStatusFields, error) {
	if len(b.session.boxes) == 0 {
		return 0, nil, errUnknownBox
	}
	last := b.session.boxes[len(b.session.boxes)-1]
	var target *ImageBox
	for i := range last.ImageBoxes {
		if last.ImageBoxes[i].SOPInstanceUID == request.RequestedSOPInstanceUID {
			target = &last.ImageBoxes[i]
		}
	}
	if target == nil {
		for _, box := range b.session.boxes[:len(b.session.boxes)-1] {
			for _, image := range box.ImageBoxes {
				if image.SOPInstanceUID == request.RequestedSOPInstanceUID {
					return 0, nil, failure(StatusProcessingFailure, "Only Image Boxes of the last created Film Box can be set", errState)
				}
			}
		}
		return 0, nil, errUnknownBox
	}
	if request.RequestedSOPClassUID != target.SOPClassUID || imageBoxClass(metaContext(ctx)) != target.SOPClassUID {
		return 0, nil, failure(0x0119, "", errState)
	}
	if modifications == nil {
		return 0, nil, failure(StatusInvalidAttributeValue, "", ErrInvalidAttributes, TagImageBoxPosition)
	}
	r := newReader(modifications)
	position, ok, err := r.number(TagImageBoxPosition)
	if err != nil || !ok || position != target.Position {
		return 0, nil, invalid(TagImageBoxPosition)
	}
	polarity, err := r.enum(TagPolarity, "NORMAL", "REVERSE")
	if err != nil {
		return 0, nil, err
	}
	sequenceTag := TagBasicGrayscaleImageSequence
	if last.Color {
		sequenceTag = TagBasicColorImageSequence
	}
	r.used[sequenceTag] = true
	ref, refErr := b.imageLUTReference(r, target.PresentationLUTUID)
	if refErr != nil {
		return 0, nil, refErr
	}
	// Legacy zero options still report unused optional attributes with 0107H.
	presentation, parseErr := parseImagePresentation(newReader(modifications), target.Presentation)
	if parseErr != nil {
		return 0, nil, parseErr
	}
	physicalPlan := PhysicalPlan{}
	physicalWarning := uint16(0)
	if b.service.physical != nil {
		for _, tag := range []core.Tag{TagRequestedImageSize, TagRequestedDecimateCrop, TagMagnificationType} {
			r.used[tag] = true
		}
	}
	element, present := modifications.Get(sequenceTag)
	if !present {
		return 0, nil, failure(StatusInvalidAttributeValue, "", ErrInvalidAttributes, sequenceTag)
	}
	var image *Image
	if sequence, isSequence := element.Value.(core.SequenceValue); !isSequence {
		return 0, nil, invalid(sequenceTag)
	} else if len(sequence.Items) > 1 {
		return 0, nil, invalid(sequenceTag)
	} else if len(sequence.Items) == 1 {
		items, _ := modifications.GetSequence(sequenceTag)
		image, err = parseImage(items[0], last.Color, b.service.limits.MaxDimension, b.service.limits.MaxImageBytes)
		if err != nil {
			return 0, nil, err
		}
		if b.sessionBytes()-imageBytes(target.Image)+imageBytes(image) > b.service.limits.MaxSessionBytes {
			return 0, nil, failure(StatusInsufficientPrinterMemory, "", ErrInvalidAttributes, sequenceTag)
		}
	}
	if presentation.Specified() && b.service.physical != nil {
		physicalPlan, physicalWarning, err = b.resolvePhysicalPlan(ctx, last.Presentation, last.Columns, last.Rows, target.Position, image, presentation)
		if err != nil {
			return 0, nil, err
		}
	}
	if err := b.validateLUTImage(last.Presentation, ref, image); err != nil {
		return 0, nil, err
	}
	target.Image = image
	target.PresentationLUTUID = ref
	target.Presentation = presentation
	target.Plan = physicalPlan
	if polarity != "" {
		target.Polarity = polarity
	}
	if ignored := r.ignored(); len(ignored) > 0 {
		status, fields := warning(StatusAttributeListError, ignored)
		return status, fields, nil
	}
	if physicalWarning != 0 {
		return physicalWarning, nil, nil
	}
	return StatusSuccess, nil, nil
}

func (b *Binding) resolvePhysicalPlan(ctx context.Context, film FilmBoxPresentation, columns, rows, position int, image *Image, p ImageBoxPresentation) (PhysicalPlan, uint16, error) {
	profile := b.service.physical
	if p.RequestedWidthMM != 0 && profile.Capabilities.RequestedSize != SupportSupported {
		return PhysicalPlan{}, 0, invalid(TagRequestedImageSize)
	}
	if p.DecimateCropBehavior != "" && profile.Capabilities.Behavior(p.DecimateCropBehavior) != SupportSupported {
		return PhysicalPlan{}, 0, invalid(TagRequestedDecimateCrop)
	}
	if image == nil {
		return PhysicalPlan{}, 0, nil
	}
	if err := ctx.Err(); err != nil {
		return PhysicalPlan{}, 0, err
	}
	area, err := profile.Area(ctx, film, columns, rows, position)
	if err != nil {
		return PhysicalPlan{}, 0, failure(StatusProcessingFailure, "", ErrPhysicalGeometry, TagRequestedImageSize, TagRequestedDecimateCrop)
	}
	plan, err := PlanImageBox(image.Rows, image.Columns, image.PixelAspectRatio, p, area, film.MagnificationType, profile.DefaultBehavior)
	if err != nil {
		return PhysicalPlan{}, 0, err
	}
	if plan.Outcome != "" && profile.Capabilities.Behavior(plan.Outcome) != SupportSupported {
		return PhysicalPlan{}, 0, invalid(TagRequestedDecimateCrop)
	}
	status := uint16(0)
	switch plan.Outcome {
	case Crop:
		status = 0xB609
	case Decimate:
		status = 0xB60A
	}
	return plan, status, nil
}

func imageBytes(image *Image) int64 {
	if image == nil {
		return 0
	}
	return int64(len(image.PixelData))
}

func (b *Binding) sessionBytes() int64 {
	var total int64
	for _, box := range b.session.boxes {
		for _, image := range box.ImageBoxes {
			total += imageBytes(image.Image)
		}
	}
	return total
}

func (b *Binding) action(ctx context.Context, request dimse.NormalizedActionRequest, _ *object.Object) (dimse.NormalizedActionSCPResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	status, err := b.actionLocked(ctx, request)
	id := request.ActionTypeID
	response := dimse.NormalizedActionResponse{Status: status, ActionTypeIDOrNil: &id}
	if err != nil {
		response.Status, response.StatusFields = statusOf(err), fieldsOf(err)
	}
	return dimse.NormalizedActionSCPResult{Response: response}, err
}

func (b *Binding) actionLocked(ctx context.Context, request dimse.NormalizedActionRequest) (uint16, error) {
	if request.ActionTypeID != PrintActionTypeID {
		return 0, failure(dimse.StatusNoSuchActionType, "", errState)
	}
	if b.session == nil {
		return 0, errUnknownBox
	}
	var boxes []*FilmBox
	whole := false
	switch request.RequestedSOPClassUID {
	case FilmSessionSOPClassUID:
		if request.RequestedSOPInstanceUID != b.session.uid {
			return 0, errUnknownBox
		}
		if len(b.session.boxes) == 0 {
			return 0, failure(StatusNoFilmBoxInSession, "", errState)
		}
		boxes, whole = b.session.boxes, true
	case FilmBoxSOPClassUID:
		box, err := b.lastBox(request.RequestedSOPInstanceUID)
		if err != nil {
			return 0, err
		}
		boxes = []*FilmBox{box}
	default:
		return 0, failure(dimse.StatusUnrecognizedOperation, "", errState)
	}
	jobID, err := b.service.uids()
	if err != nil {
		return 0, err
	}
	job := Job{ID: jobID, FilmSessionUID: b.session.uid, CallingAETitle: b.calling, Session: b.session.presentation, WholeSession: whole, Copies: b.session.presentation.NumberOfCopies, RequestedAt: b.service.clock()}
	for _, box := range boxes {
		for _, image := range box.ImageBoxes {
			if err := b.validateLUTImage(box.Presentation, image.PresentationLUTUID, image.Image); err != nil {
				return 0, err
			}
		}
	}
	job.PresentationLUTs = b.jobLUTs(boxes)
	for _, box := range boxes {
		copy := box.clone()
		for _, image := range copy.ImageBoxes {
			if image.Image == nil {
				job.EmptyImageBoxes++
			} else {
				job.ImageBoxesPrinted++
			}
		}
		job.FilmBoxes = append(job.FilmBoxes, copy)
	}
	printCtx, cancel := context.WithTimeout(ctx, b.service.limits.PrintTimeout)
	defer cancel()
	// One call per N-ACTION: a refused or uncertain job is reported, never
	// resubmitted, so a film is not printed twice.
	if err := b.service.backend.Print(printCtx, job); err != nil {
		switch {
		case errors.Is(err, ErrQueueFull) && whole:
			return 0, failure(StatusSessionQueueFull, "", err)
		case errors.Is(err, ErrQueueFull):
			return 0, failure(StatusFilmBoxQueueFull, "", err)
		default:
			return 0, failure(StatusProcessingFailure, "Print backend failure", err)
		}
	}
	for _, box := range boxes {
		box.Printed = true
	}
	if job.ImageBoxesPrinted == 0 {
		if whole {
			return StatusEmptySessionPage, nil
		}
		return StatusEmptyFilmBoxPage, nil
	}
	return StatusSuccess, nil
}

func (b *Binding) delete(_ context.Context, request dimse.NormalizedDeleteRequest) (dimse.NormalizedDeleteSCPResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.deleteLocked(request)
	return dimse.NormalizedDeleteSCPResult{Response: dimse.NormalizedDeleteResponse{Status: statusOf(err), StatusFields: fieldsOf(err)}}, err
}

func (b *Binding) deleteLocked(request dimse.NormalizedDeleteRequest) error {
	if request.RequestedSOPClassUID == PresentationLUTSOPClassUID {
		return b.deletePresentationLUT(request.RequestedSOPInstanceUID)
	}
	if b.session == nil {
		return errUnknownBox
	}
	switch request.RequestedSOPClassUID {
	case FilmSessionSOPClassUID:
		if request.RequestedSOPInstanceUID != b.session.uid {
			return errUnknownBox
		}
		b.session = nil
		return nil
	case FilmBoxSOPClassUID:
		if _, err := b.lastBox(request.RequestedSOPInstanceUID); err != nil {
			return err
		}
		b.session.boxes = b.session.boxes[:len(b.session.boxes)-1]
		return nil
	}
	return failure(dimse.StatusUnrecognizedOperation, "", errState)
}

func (b *Binding) get(ctx context.Context, request dimse.NormalizedGetRequest) (dimse.NormalizedGetSCPResult, error) {
	if request.RequestedSOPClassUID != PrinterSOPClassUID || request.RequestedSOPInstanceUID != PrinterSOPInstanceUID {
		err := errUnknownBox
		if request.RequestedSOPClassUID != PrinterSOPClassUID {
			err = failure(dimse.StatusUnrecognizedOperation, "", errState)
		}
		return dimse.NormalizedGetSCPResult{Response: dimse.NormalizedGetResponse{Status: statusOf(err)}}, err
	}
	status, err := b.service.backend.PrinterStatus(ctx)
	if err != nil {
		return dimse.NormalizedGetSCPResult{Response: dimse.NormalizedGetResponse{Status: StatusProcessingFailure}}, err
	}
	all := map[core.Tag]core.Element{
		TagPrinterStatus:         text(TagPrinterStatus, core.VRCS, status.Status),
		TagPrinterStatusInfo:     text(TagPrinterStatusInfo, core.VRCS, status.Info),
		TagPrinterName:           text(TagPrinterName, core.VRLO, status.Name),
		TagManufacturer:          text(TagManufacturer, core.VRLO, status.Manufacturer),
		TagManufacturerModelName: text(TagManufacturerModelName, core.VRLO, status.ManufacturerModelName),
	}
	var elements []core.Element
	missing := false
	if len(request.AttributeIdentifierList) == 0 {
		for _, element := range all {
			elements = append(elements, element)
		}
	} else {
		for _, tag := range request.AttributeIdentifierList {
			element, ok := all[tag]
			if !ok {
				missing = true
				continue
			}
			elements = append(elements, element)
		}
	}
	code := StatusSuccess
	if missing {
		code = StatusAttributeListError
	}
	return dimse.NormalizedGetSCPResult{Response: dimse.NormalizedGetResponse{Status: code}, DataSet: object.FromElements(elements, std.Dictionary)}, nil
}
