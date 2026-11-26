package dicomdir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Profile names a PS3.11 Media Storage Application Profile. WriteOptions
// .Profile validates the file set against it and adds the directory keys and
// icons it requires.
type Profile string

const (
	// ProfileGeneralPurpose is STD-GEN-CD (also STD-GEN-DVD-RAM and
	// STD-GEN-BD): any image storage SOP class in Explicit VR Little Endian.
	ProfileGeneralPurpose Profile = "STD-GEN-CD"
	// ProfileGeneralDVDJPEG is STD-GEN-DVD-JPEG: Explicit VR Little Endian or
	// JPEG Lossless SV1, Baseline and Extended.
	ProfileGeneralDVDJPEG Profile = "STD-GEN-DVD-JPEG"
	// ProfileGeneralDVDJ2K is STD-GEN-DVD-J2K: Explicit VR Little Endian or
	// JPEG 2000 (lossless only or lossy).
	ProfileGeneralDVDJ2K Profile = "STD-GEN-DVD-J2K"
	// ProfileGeneralUSBJPEG is STD-GEN-USB-JPEG (and the MMC, CF and SD
	// variants), with the transfer syntaxes of STD-GEN-DVD-JPEG.
	ProfileGeneralUSBJPEG Profile = "STD-GEN-USB-JPEG"
	// ProfileGeneralUSBJ2K is STD-GEN-USB-J2K, with the transfer syntaxes of
	// STD-GEN-DVD-J2K.
	ProfileGeneralUSBJ2K Profile = "STD-GEN-USB-J2K"
	// ProfileXA1KCD is STD-XA1K-CD: X-Ray Angiographic images in JPEG
	// Lossless SV1 up to 1024x1024 with 8, 10 or 12 bits stored, 8-bit
	// MONOCHROME2 Secondary Capture in Explicit VR Little Endian, and a
	// 128x128 icon in every IMAGE record.
	ProfileXA1KCD Profile = "STD-XA1K-CD"
	// ProfileXA1KDVD is STD-XA1K-DVD: STD-XA1K-CD that also admits JPEG
	// Baseline and Extended for X-Ray Angiographic images.
	ProfileXA1KDVD Profile = "STD-XA1K-DVD"
)

// Profile diagnostics. They name the rule, never a value or a path.
const (
	DiagnosticProfileSOPClass       DiagnosticCode = "profile_sop_class"
	DiagnosticProfileTransferSyntax DiagnosticCode = "profile_transfer_syntax"
	DiagnosticProfileImage          DiagnosticCode = "profile_image_constraint"
	DiagnosticProfileMissingKey     DiagnosticCode = "profile_missing_key"
	DiagnosticIconUnavailable       DiagnosticCode = "icon_unavailable"
)

var (
	// ErrProfileViolation reports a file set that does not satisfy the
	// requested application profile; errors.As yields a *ProfileError.
	ErrProfileViolation = errors.New("dicomdir: file set violates the application profile")
	// ErrUnknownProfile reports a Profile this package does not define.
	ErrUnknownProfile = errors.New("dicomdir: unknown application profile")
	// ErrIconUnavailable reports an icon that could not be rendered.
	ErrIconUnavailable = errors.New("dicomdir: icon image could not be rendered")
)

// ProfileError lists every profile violation of a file set.
type ProfileError struct {
	Profile     Profile
	Diagnostics []Diagnostic
}

func (e *ProfileError) Error() string {
	return fmt.Sprintf("%s: %s (%d finding(s))", ErrProfileViolation, e.Profile, len(e.Diagnostics))
}

func (e *ProfileError) Unwrap() error { return ErrProfileViolation }

const (
	sopClassXA = "1.2.840.10008.5.1.4.1.1.12.1"
	sopClassSC = "1.2.840.10008.5.1.4.1.1.7"
)

type profileRules struct {
	// transferSyntax reports whether a SOP class may use a transfer syntax.
	transferSyntax func(sopClass, syntax string) bool
	// sopClasses, when set, lists the only admitted SOP classes.
	sopClasses map[string]bool
	// iconSize, when non-zero, fixes the icon size and makes icons required.
	iconSize int
	// generalKeys makes Patient ID, Study Date, Study Time, Study ID, Series
	// Number and Instance Number Type 1 (PS3.11 D.3.3.1).
	generalKeys bool
	// xaKeys adds the STD-XA1K directory keys (PS3.11 Table B.3-2).
	xaKeys bool
	// compressionKeys adds the keys of the general purpose DVD, BD and USB
	// profiles with compression (PS3.11 Tables H.3-2 and J.3-2).
	compressionKeys bool
	// xaImages applies the STD-XA1K image constraints (Tables B.3-3, B.3-4).
	xaImages bool
}

func syntaxSet(uids ...string) map[string]bool {
	out := make(map[string]bool, len(uids))
	for _, uid := range uids {
		out[uid] = true
	}
	return out
}

var (
	generalJPEGSyntaxes = syntaxSet(transfer.ExplicitVRLittleEndian.UID, transfer.JPEGLosslessSV1.UID, transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID)
	generalJ2KSyntaxes  = syntaxSet(transfer.ExplicitVRLittleEndian.UID, transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID)
)

func profileRulesFor(profile Profile) (profileRules, error) {
	anyOf := func(set map[string]bool) func(string, string) bool {
		return func(_, syntax string) bool { return set[syntax] }
	}
	switch profile {
	case ProfileGeneralPurpose:
		return profileRules{transferSyntax: anyOf(syntaxSet(transfer.ExplicitVRLittleEndian.UID)), generalKeys: true}, nil
	case ProfileGeneralDVDJPEG, ProfileGeneralUSBJPEG:
		return profileRules{transferSyntax: anyOf(generalJPEGSyntaxes), generalKeys: true, compressionKeys: true}, nil
	case ProfileGeneralDVDJ2K, ProfileGeneralUSBJ2K:
		return profileRules{transferSyntax: anyOf(generalJ2KSyntaxes), generalKeys: true, compressionKeys: true}, nil
	case ProfileXA1KCD, ProfileXA1KDVD:
		dvd := profile == ProfileXA1KDVD
		return profileRules{
			sopClasses: syntaxSet(sopClassXA, sopClassSC),
			transferSyntax: func(sopClass, syntax string) bool {
				if sopClass == sopClassSC {
					return syntax == transfer.ExplicitVRLittleEndian.UID
				}
				return syntax == transfer.JPEGLosslessSV1.UID ||
					dvd && (syntax == transfer.JPEGBaseline.UID || syntax == transfer.JPEGExtended.UID)
			},
			iconSize: 128,
			xaKeys:   true,
			xaImages: true,
		}, nil
	}
	return profileRules{}, fmt.Errorf("%w: %q", ErrUnknownProfile, string(profile))
}

var (
	tagImageTypeFS             = core.NewTag(0x0008, 0x0008)
	tagInstitutionNameFS       = core.NewTag(0x0008, 0x0080)
	tagInstitutionAddressFS    = core.NewTag(0x0008, 0x0081)
	tagReferencedImageSequence = core.NewTag(0x0008, 0x1140)
	tagPerformingPhysicianName = core.NewTag(0x0008, 0x1050)
	tagPatientBirthDateFS      = core.NewTag(0x0010, 0x0030)
	tagPatientSexFS            = core.NewTag(0x0010, 0x0040)
	tagSamplesPerPixelFS       = core.NewTag(0x0028, 0x0002)
	tagPhotometricFS           = core.NewTag(0x0028, 0x0004)
	tagRowsFS                  = core.NewTag(0x0028, 0x0010)
	tagColumnsFS               = core.NewTag(0x0028, 0x0011)
	tagBitsAllocatedFS         = core.NewTag(0x0028, 0x0100)
	tagBitsStoredFS            = core.NewTag(0x0028, 0x0101)
	tagHighBitFS               = core.NewTag(0x0028, 0x0102)
	tagPixelRepresentationFS   = core.NewTag(0x0028, 0x0103)
	tagLossyCompressionRatioFS = core.NewTag(0x0028, 0x2112)
	tagCalibrationImageFS      = core.NewTag(0x0050, 0x0004)
	tagIconImageSequence       = core.NewTag(0x0088, 0x0200)

	tagAcquisitionDateTimeFS              = core.NewTag(0x0008, 0x002A)
	tagAcquisitionTimeSynchronized        = core.NewTag(0x0018, 0x1800)
	tagImagePositionPatientFS             = core.NewTag(0x0020, 0x0032)
	tagImageOrientationPatientFS          = core.NewTag(0x0020, 0x0037)
	tagFrameOfReferenceUIDFS              = core.NewTag(0x0020, 0x0052)
	tagSynchronizationFrameOfReferenceUID = core.NewTag(0x0020, 0x0200)
	tagPlanePositionSequence              = core.NewTag(0x0020, 0x9113)
	tagPlaneOrientationSequence           = core.NewTag(0x0020, 0x9116)
	tagPixelSpacingFS                     = core.NewTag(0x0028, 0x0030)
	tagPixelMeasuresSequence              = core.NewTag(0x0028, 0x9110)
)

// recordExtras holds the profile keys and the icon of one referenced
// instance, copied from the instance at write time.
type recordExtras struct {
	patient []core.Element
	series  []core.Element
	image   []core.Element
}

// profileFinding is one violation found while inspecting an instance.
type profileFinding struct {
	code    DiagnosticCode
	message string
}

// inspectProfile checks one instance against the rules and returns the
// directory keys the profile adds for it.
func inspectProfile(rules profileRules, record FileRecord, dataset *object.Object) (recordExtras, []profileFinding) {
	var extras recordExtras
	var findings []profileFinding
	fail := func(code DiagnosticCode, message string) {
		findings = append(findings, profileFinding{code: code, message: message})
	}
	syntax := core.NormalizeUID(record.TransferSyntaxUID)
	if rules.sopClasses != nil && !rules.sopClasses[record.SOPClassUID] {
		fail(DiagnosticProfileSOPClass, "SOP class is not part of the profile")
	} else if rules.transferSyntax != nil && !rules.transferSyntax(record.SOPClassUID, syntax) {
		fail(DiagnosticProfileTransferSyntax, "transfer syntax is not allowed for this SOP class by the profile")
	}
	if rules.generalKeys {
		for _, key := range []struct {
			name  string
			value string
		}{
			{"Patient ID", record.PatientID}, {"Study Date", record.StudyDate}, {"Study Time", record.StudyTime},
			{"Study ID", record.StudyID}, {"Series Number", record.SeriesNumber}, {"Instance Number", record.InstanceNumber},
		} {
			if strings.TrimSpace(key.value) == "" {
				fail(DiagnosticProfileMissingKey, key.name+" is empty but Type 1 in the profile")
			}
		}
	}
	if rules.xaImages {
		findings = append(findings, xaImageFindings(record, dataset)...)
	}

	// Image Type and Referenced Image Sequence are copied when the instance
	// has them (PS3.11 Tables D.3-2 and B.3-2).
	if element, ok := dataset.Get(tagImageTypeFS); ok {
		extras.image = append(extras.image, element)
	} else if rules.xaKeys && record.SOPClassUID == sopClassXA {
		fail(DiagnosticProfileMissingKey, "Image Type is required for X-Ray Angiographic images")
	}
	if element, ok := dataset.Get(tagReferencedImageSequence); ok {
		// Copied as-is (PS3.11 Tables D.3-2 and H.3-2).
		extras.image = append(extras.image, element)
	}
	if rules.xaKeys {
		extras.patient = append(extras.patient, copiedOrEmpty(dataset, tagPatientBirthDateFS, core.VRDA), copiedOrEmpty(dataset, tagPatientSexFS, core.VRCS))
		extras.series = append(extras.series,
			copiedOrEmpty(dataset, tagInstitutionNameFS, core.VRLO),
			copiedOrEmpty(dataset, tagInstitutionAddressFS, core.VRST),
			copiedOrEmpty(dataset, tagPerformingPhysicianName, core.VRPN))
		extras.image = append(extras.image, copiedOrEmpty(dataset, tagCalibrationImageFS, core.VRCS))
		if element, ok := dataset.Get(tagLossyCompressionRatioFS); ok {
			extras.image = append(extras.image, element)
		}
	}
	if rules.compressionKeys {
		for _, tag := range []core.Tag{tagPatientBirthDateFS, tagPatientSexFS} {
			if element, ok := nonEmpty(dataset, tag); ok {
				extras.patient = append(extras.patient, element)
			}
		}
		for _, tag := range []core.Tag{tagInstitutionNameFS, tagInstitutionAddressFS, tagPerformingPhysicianName} {
			if element, ok := nonEmpty(dataset, tag); ok {
				extras.series = append(extras.series, element)
			}
		}
		for _, tag := range []core.Tag{tagCalibrationImageFS, tagLossyCompressionRatioFS} {
			if element, ok := nonEmpty(dataset, tag); ok {
				extras.image = append(extras.image, element)
			}
		}
		for _, tag := range []core.Tag{tagRowsFS, tagColumnsFS} {
			if element, ok := dataset.Get(tag); ok {
				extras.image = append(extras.image, element)
			} else {
				fail(DiagnosticProfileMissingKey, "Rows and Columns are Type 1 in the profile")
			}
		}
		for _, tag := range []core.Tag{tagFrameOfReferenceUIDFS, tagSynchronizationFrameOfReferenceUID, tagNumberOfFramesFS, tagAcquisitionTimeSynchronized, tagAcquisitionDateTimeFS} {
			if element, ok := dataset.Get(tag); ok {
				extras.image = append(extras.image, element)
			}
		}
		for _, key := range []struct{ functionalGroup, tag core.Tag }{
			{tagPlanePositionSequence, tagImagePositionPatientFS},
			{tagPlaneOrientationSequence, tagImageOrientationPatientFS},
			{tagPixelMeasuresSequence, tagPixelSpacingFS},
		} {
			if element, ok := topLevelOrShared(dataset, key.functionalGroup, key.tag); ok {
				extras.image = append(extras.image, element)
			}
		}
	}
	return extras, findings
}

// nonEmpty returns a copy of an attribute that is present with a value.
func nonEmpty(dataset *object.Object, tag core.Tag) (core.Element, bool) {
	element, ok := dataset.Get(tag)
	if !ok {
		return core.Element{}, false
	}
	if raw, isRaw := element.RawBytes(); isRaw {
		return element, strings.Trim(string(raw), " \x00") != ""
	}
	return element, strings.TrimSpace(strings.Join(element.StringValues(), "")) != ""
}

// topLevelOrShared returns tag from the data set, or from the functional
// group macro of the Shared Functional Groups Sequence of an enhanced
// multi-frame object (PS3.11 Table H.3-2).
func topLevelOrShared(dataset *object.Object, functionalGroup, tag core.Tag) (core.Element, bool) {
	if element, ok := dataset.Get(tag); ok {
		return element, true
	}
	// Frame index -1 consults the Shared Functional Groups only.
	macro, _, _ := dataset.FunctionalGroupItem(-1, functionalGroup)
	if macro == nil {
		return core.Element{}, false
	}
	if element, ok := macro.Get(tag); ok {
		return element, true
	}
	return core.Element{}, false
}

// xaImageFindings applies PS3.11 Tables B.3-3 and B.3-4.
func xaImageFindings(record FileRecord, dataset *object.Object) []profileFinding {
	var findings []profileFinding
	fail := func(message string) {
		findings = append(findings, profileFinding{code: DiagnosticProfileImage, message: message})
	}
	rows, okRows := usValue(dataset, tagRowsFS)
	columns, okColumns := usValue(dataset, tagColumnsFS)
	if !okRows || !okColumns || rows == 0 || columns == 0 || rows > 1024 || columns > 1024 {
		fail("Rows and Columns must be 1 to 1024")
	}
	stored, _ := usValue(dataset, tagBitsStoredFS)
	switch record.SOPClassUID {
	case sopClassXA:
		if modality, _ := dataset.GetString(tagModalityFS); strings.TrimSpace(modality) != "XA" {
			fail("Modality must be XA")
		}
		if stored != 8 && stored != 10 && stored != 12 {
			fail("X-Ray Angiographic Bits Stored must be 8, 10 or 12")
		}
	case sopClassSC:
		samples, _ := usValue(dataset, tagSamplesPerPixelFS)
		allocated, _ := usValue(dataset, tagBitsAllocatedFS)
		high, _ := usValue(dataset, tagHighBitFS)
		representation, _ := usValue(dataset, tagPixelRepresentationFS)
		photometric, _ := dataset.GetString(tagPhotometricFS)
		if samples != 1 || strings.TrimSpace(photometric) != "MONOCHROME2" || allocated != 8 || stored != 8 || high != 7 || representation != 0 {
			fail("Secondary Capture must be 8-bit unsigned MONOCHROME2")
		}
		for _, element := range dataset.Elements() {
			if group := element.Tag().Group; group >= 0x6000 && group <= 0x60FF && group%2 == 0 {
				fail("Secondary Capture must not carry overlay groups")
				break
			}
		}
	}
	return findings
}

func usValue(dataset *object.Object, tag core.Tag) (int, bool) {
	element, ok := dataset.Get(tag)
	if !ok || element.VR() != core.VRUS {
		return 0, false
	}
	raw, ok := element.RawBytes()
	if !ok || len(raw) < 2 {
		return 0, false
	}
	return int(dataset.ValueByteOrder().Uint16(raw)), true
}

func cloneElement(element core.Element) core.Element {
	if raw, ok := element.RawBytes(); ok {
		return core.NewRawElement(element.Tag(), element.VR(), core.CloneBytes(raw))
	}
	return element
}

func copiedOrEmpty(dataset *object.Object, tag core.Tag, vr core.VR) core.Element {
	if element, ok := dataset.Get(tag); ok {
		return element
	}
	return core.NewRawElement(tag, vr, nil)
}

// readReferencedDataSet reads one referenced instance. Values above 64 KiB
// stay deferred, and unreadable once it returns, unless pixels are needed
// for an icon.
func (f *FileSet) readReferencedDataSet(record FileRecord, withPixels bool) (*object.File, error) {
	source, _, err := OpenReferencedFile(f.root, Reference{FileID: []string(record.FileID)})
	if err != nil {
		return nil, err
	}
	options := object.ReadFileOptions{InlineValueBytesThreshold: 64 << 10}
	if withPixels {
		options = object.ReadFileOptions{}
	}
	file, err := object.ReadFileWithOptions(io.ReadSeeker(source), options)
	closeErr := source.Close()
	if err != nil {
		return nil, err
	}
	// Deferred values (Pixel Data when withPixels is false) are not read
	// after this point; every key the profile copies is inline.
	return file, closeErr
}

// ValidateProfile checks the file set against an application profile
// without writing a directory. Icons are not rendered.
func (f *FileSet) ValidateProfile(ctx context.Context, profile Profile) (ValidationReport, error) {
	rules, err := profileRulesFor(profile)
	if err != nil {
		return ValidationReport{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	records := f.Records()
	report := ValidationReport{Valid: true, Statistics: f.Statistics()}
	for i, record := range records {
		if err := ctx.Err(); err != nil {
			return ValidationReport{}, err
		}
		file, err := f.readReferencedDataSet(record, false)
		if err != nil {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: DiagnosticSourceChanged, SourceIndex: i, Message: "referenced file could not be read"})
			continue
		}
		_, findings := inspectProfile(rules, record, file.Dataset)
		for _, finding := range findings {
			report.Valid = false
			report.Diagnostics = append(report.Diagnostics, Diagnostic{Code: finding.code, SourceIndex: i, Message: finding.message})
		}
	}
	return report, nil
}

func fileIDKey(id FileID) string { return strings.Join([]string(id), `\`) }

// recordExtras reads every referenced instance once when a profile or icons
// are requested, validating the profile and rendering the icons before any
// byte is written.
func (f *FileSet) recordExtras(ctx context.Context, records []sourceSnapshot, options WriteOptions) (map[string]recordExtras, []Diagnostic, int, error) {
	var rules profileRules
	if options.Profile != "" {
		var err error
		if rules, err = profileRulesFor(options.Profile); err != nil {
			return nil, nil, 0, err
		}
	}
	icons, size := options.Icons, options.IconSize
	if rules.iconSize != 0 {
		icons, size = true, rules.iconSize
	}
	if size == 0 {
		size = defaultIconSize
	}
	if options.Profile == "" && !icons {
		return nil, nil, 0, nil
	}
	out := make(map[string]recordExtras, len(records))
	var findings, diagnostics []Diagnostic
	count := 0
	f.mu.RLock()
	imported := f.directory
	f.mu.RUnlock()
	used, budgetErr := plannedDirectoryKeyUsage(imported, records, f.options.Limits.MaxTotalRecordKeyBytes)
	if budgetErr != nil {
		return nil, nil, 0, budgetErr
	}
	aliases := map[string]int{}
	if imported != nil {
		for _, node := range imported.flat {
			if standardFileRecordType(node.typ) {
				aliases[node.record.FileID.String()]++
			}
		}
	}
	for i, stored := range records {
		if err := ctx.Err(); err != nil {
			return nil, nil, 0, err
		}
		supportsIcon := stored.record.RecordType == RecordTypeImage || stored.record.RecordType == RecordTypeRTDose
		file, err := f.readReferencedDataSet(stored.record, icons && supportsIcon)
		if err != nil {
			return nil, nil, 0, sourceValidationError{sourceIndex: i}
		}
		var extra recordExtras
		if options.Profile != "" {
			var found []profileFinding
			extra, found = inspectProfile(rules, stored.record, file.Dataset)
			for _, finding := range found {
				findings = append(findings, Diagnostic{Code: finding.code, SourceIndex: i, Message: finding.message})
			}
		}
		n := aliases[stored.record.FileID.String()]
		if n == 0 {
			n = 1
		}
		remaining := (f.options.Limits.MaxTotalRecordKeyBytes - used) / int64(n)
		if remaining <= 0 {
			return nil, nil, 0, ErrResourceLimit
		}
		keyLimits := f.options.Limits
		if remaining < keyLimits.MaxRecordKeyBytes {
			keyLimits.MaxRecordKeyBytes = remaining
		}
		budget := &directoryKeyBudget{ctx: ctx, limits: keyLimits}
		canonical := func(elements []core.Element) ([]core.Element, error) {
			for j, e := range elements {
				if file.Dataset.Has(e.Tag()) {
					owned, err := ownDirectoryElement(file.Dataset, e.Tag(), budget, 0)
					if err != nil {
						return nil, err
					}
					elements[j] = owned
				}
			}
			return elements, nil
		}
		extra.patient, err = canonical(extra.patient)
		if err != nil {
			return nil, nil, 0, err
		}
		extra.series, err = canonical(extra.series)
		if err != nil {
			return nil, nil, 0, err
		}
		extra.image, err = canonical(extra.image)
		if err != nil {
			return nil, nil, 0, err
		}
		if icons && supportsIcon && len(findings) == 0 {
			// Reserve the bounded icon payload/container allowance before rendering.
			if err := budget.charge(int64(size*size)+512, 0); err != nil {
				return nil, nil, 0, err
			}
			icon, err := renderIcon(ctx, file, size, options.Decoders)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, nil, 0, ctxErr
				}
				if !options.BlankIconOnFailure {
					return nil, nil, 0, fmt.Errorf("source %d: %w", i, err)
				}
				icon = iconSequence(make([]byte, size*size), size)
				diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticIconUnavailable, SourceIndex: i, Message: "icon replaced by a black image"})
			}
			extra.image = append(extra.image, icon)
			count++
		}
		used += budget.bytes * int64(n)
		out[fileIDKey(stored.record.FileID)] = extra
	}
	if len(findings) != 0 {
		return nil, nil, 0, &ProfileError{Profile: options.Profile, Diagnostics: findings}
	}
	return out, diagnostics, count, nil
}

// mergeLevelKeys gives a PATIENT or SERIES record the keys of its
// subordinate instances: for each attribute the first non-empty value, else
// the first (empty) one, since the profiles require the attribute when "any
// object referenced by subordinate records" has a value.
func mergeLevelKeys(leaves []*directoryNode, level func(recordExtras) []core.Element) []core.Element {
	var order []core.Tag
	chosen := map[core.Tag]core.Element{}
	for _, leaf := range leaves {
		for _, element := range level(leaf.extras) {
			tag := element.Tag()
			current, seen := chosen[tag]
			if !seen {
				order = append(order, tag)
				chosen[tag] = element
				continue
			}
			if raw, ok := current.RawBytes(); ok && len(raw) == 0 {
				chosen[tag] = element
			}
		}
	}
	out := make([]core.Element, len(order))
	for i, tag := range order {
		out[i] = chosen[tag]
	}
	return out
}

func leafNodes(node *directoryNode) []*directoryNode {
	if len(node.children) == 0 {
		return []*directoryNode{node}
	}
	var out []*directoryNode
	for _, child := range node.children {
		out = append(out, leafNodes(child)...)
	}
	return out
}
