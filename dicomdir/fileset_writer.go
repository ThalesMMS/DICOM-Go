package dicomdir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const mediaStorageDirectoryStorageUID = "1.2.840.10008.1.3.10"

// Distinct candidates can describe the same root, including path aliases.
// Serialize publication across candidates so their final fingerprint checks
// cannot both pass before either replacement becomes visible.
var directoryPublications struct {
	sync.Mutex
	active  []os.FileInfo
	changed chan struct{}
}

func lockDirectoryPublication(ctx context.Context, root os.FileInfo) (func(), error) {
	for {
		directoryPublications.Lock()
		busy := false
		for _, active := range directoryPublications.active {
			busy = busy || os.SameFile(active, root)
		}
		if !busy {
			directoryPublications.active = append(directoryPublications.active, root)
			directoryPublications.Unlock()
			return func() {
				directoryPublications.Lock()
				for i, active := range directoryPublications.active {
					if os.SameFile(active, root) {
						directoryPublications.active = append(directoryPublications.active[:i], directoryPublications.active[i+1:]...)
						break
					}
				}
				if directoryPublications.changed != nil {
					close(directoryPublications.changed)
					directoryPublications.changed = nil
				}
				directoryPublications.Unlock()
			}, nil
		}
		if directoryPublications.changed == nil {
			directoryPublications.changed = make(chan struct{})
		}
		changed := directoryPublications.changed
		directoryPublications.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

var (
	tagFileSetID                                 = core.NewTag(0x0004, 0x1130)
	tagFileSetDescriptorFileID                   = core.NewTag(0x0004, 0x1141)
	tagDescriptorCharacterSet                    = core.NewTag(0x0004, 0x1142)
	tagOffsetLastRootDirectoryRecord             = core.NewTag(0x0004, 0x1202)
	tagFileSetConsistencyFlag                    = core.NewTag(0x0004, 0x1212)
	tagReferencedSOPClassUIDInFile               = core.NewTag(0x0004, 0x1510)
	tagReferencedSOPInstanceUIDInFile            = core.NewTag(0x0004, 0x1511)
	tagReferencedTransferSyntaxUIDInFile         = core.NewTag(0x0004, 0x1512)
	tagReferencedRelatedGeneralSOPClassUIDInFile = core.NewTag(0x0004, 0x151A)
	tagMediaStorageSOPClassUIDDirectory          = core.NewTag(0x0002, 0x0002)
	tagMediaStorageSOPInstanceUIDDirectory       = core.NewTag(0x0002, 0x0003)
	tagTransferSyntaxUIDDirectory                = core.NewTag(0x0002, 0x0010)
)

// WriteOptions bounds one two-pass DICOMDIR encoding.
type WriteOptions struct {
	// PublicationGuard runs after the staged index and source checks, before
	// the final root and destination checks and atomic replacement.
	// Returning an error preserves the existing DICOMDIR. It is not called by
	// WriteDICOMDIR, which does not publish a filesystem destination.
	// The callback must not call CommitDICOMDIR recursively.
	PublicationGuard func(context.Context, string) error
	// LegacyMRDR explicitly permits rewriting an imported retired MRDR graph.
	LegacyMRDR bool
	// MaxBytes is the inclusive bound on each encoded DICOMDIR pass and
	// the final output. Zero uses 64 MiB.
	MaxBytes int64
	// Profile, when set, validates every referenced instance against a PS3.11
	// Media Storage Application Profile before anything is written (a
	// *ProfileError lists the violations) and adds the directory keys the
	// profile requires. Empty writes the profile-neutral directory.
	Profile Profile
	// Icons adds an Icon Image Sequence to IMAGE and RT DOSE records. Profiles that
	// require icons (STD-XA1K) enable it and fix the size themselves.
	Icons bool
	// IconSize is the icon width and height, 1 to 128; zero uses 64.
	IconSize int
	// Decoders decodes compressed Pixel Data for icons; nil uses the
	// built-in decoders.
	Decoders pixeldata.Registry
	// BlankIconOnFailure writes a black icon, reported in
	// WriteReport.Diagnostics, when an image cannot be rendered; by default
	// the write fails with ErrIconUnavailable.
	BlankIconOnFailure bool
}

// WriteReport describes the final encoded DICOMDIR bytes. CommitDICOMDIR may
// return a populated report with ErrTransaction when atomic replacement
// succeeded but the final directory durability sync failed.
type WriteReport struct {
	BytesWritten      int64
	Records           int
	LogicalReferences int
	MRDRRecords       int
	SHA256            [32]byte
	// Icons counts the Icon Image Sequences written.
	Icons int
	// Diagnostics lists value-free notes such as blank icons.
	Diagnostics   []Diagnostic
	revision      uint64
	extraElements int
}

type directoryNode struct {
	typ        RecordType
	record     FileRecord
	extras     recordExtras
	children   []*directoryNode
	next       *directoryNode
	offset     uint32
	imported   core.DataSet
	mrdr       *directoryNode
	references uint32
	inactive   bool
}

// WriteDICOMDIR validates and writes a deterministic Basic Directory IOD.
// For atomic publication to the file-set root, use CommitDICOMDIR.
func WriteDICOMDIR(ctx context.Context, destination io.Writer, fileSet *FileSet, options WriteOptions) (WriteReport, error) {
	if fileSet == nil || destination == nil {
		return WriteReport{}, ErrInvalidOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return WriteReport{}, err
	}
	maxBytes, err := normalizeWriteOptions(options)
	if err != nil {
		return WriteReport{}, err
	}

	fileSet.mu.RLock()
	records := snapshotFileRecords(fileSet.records)
	uid := fileSet.uid
	id := fileSet.id
	descriptor := append(FileID(nil), fileSet.descriptor...)
	descriptorCharacterSet := fileSet.descriptorCharacterSet
	revision := fileSet.revision
	imported := fileSet.directory
	fileSet.mu.RUnlock()
	if imported != nil && !imported.matches(ctx) {
		if err := ctx.Err(); err != nil {
			return WriteReport{}, err
		}
		return WriteReport{}, ErrSourceChanged
	}
	if imported != nil && imported.hasMRDR() && !options.LegacyMRDR {
		return WriteReport{}, ErrLegacyMRDRRequiresOptIn
	}

	if err := fileSet.validateDescriptor(ctx); err != nil {
		return WriteReport{}, err
	}
	if err := plannedDirectoryKeyBytes(imported, records, fileSet.options.Limits.MaxTotalRecordKeyBytes); err != nil {
		return WriteReport{}, err
	}
	if _, err := plannedDirectoryRecordCount(imported, records, fileSet.options.Limits.MaxDirectoryRecords); err != nil {
		return WriteReport{}, err
	}
	if _, err := fileSet.validateRecordsSnapshot(ctx, records); err != nil {
		return WriteReport{}, err
	}
	if err := fileSet.revalidateSources(ctx, records); err != nil {
		return WriteReport{}, err
	}
	extras, diagnostics, _, err := fileSet.recordExtras(ctx, records, options)
	if err != nil {
		return WriteReport{}, err
	}
	roots, flat, err := buildDirectoryHierarchy(records)
	if err != nil {
		return WriteReport{}, err
	}
	if imported != nil {
		roots, flat, err = mergeImportedDirectory(imported, roots)
		if err != nil {
			return WriteReport{}, err
		}
	}
	if len(flat) > fileSet.options.Limits.MaxDirectoryRecords {
		return WriteReport{}, ErrResourceLimit
	}
	for _, node := range flat {
		if len(node.children) == 0 {
			node.extras = extras[fileIDKey(node.record.FileID)]
		}
	}
	extraElements, icons := 0, 0
	for _, node := range flat {
		switch node.typ {
		case RecordTypePatient:
			node.extras.patient = mergeLevelKeys(leafNodes(node), func(e recordExtras) []core.Element { return e.patient })
			extraElements += countDirectoryElements(node.extras.patient)
		case RecordTypeSeries:
			node.extras.series = mergeLevelKeys(leafNodes(node), func(e recordExtras) []core.Element { return e.series })
			extraElements += countDirectoryElements(node.extras.series)
		case RecordTypeStudy:
		default:
			extraElements += countDirectoryElements(node.extras.image)
			extraElements += countDirectoryElements(node.record.keys.Elements)
		}
		extraElements += countDirectoryElements(node.imported.Elements)
		for _, e := range directoryRecordDataSet(node, 0, 0).Elements {
			if e.Tag() == tagIconImageSequence {
				icons++
				break
			}
		}
	}
	encoded, err := encodeDirectoryTwoPass(ctx, roots, flat, uid, id, descriptor, descriptorCharacterSet, maxBytes, fileSet.options.Limits)
	if err != nil {
		return WriteReport{}, err
	}
	if err := fileSet.revalidateSources(ctx, records); err != nil {
		return WriteReport{}, err
	}
	if !fileSet.revisionMatches(revision) {
		return WriteReport{}, redactedFileSetError{class: ErrSourceChanged}
	}
	if imported != nil && !imported.matches(ctx) {
		if err := ctx.Err(); err != nil {
			return WriteReport{}, err
		}
		return WriteReport{}, ErrSourceChanged
	}
	written, err := writeAllContext(ctx, destination, encoded)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return WriteReport{}, ctxErr
		}
		if errors.Is(err, io.ErrShortWrite) {
			return WriteReport{}, errors.Join(ErrDICOMDIRWrite, io.ErrShortWrite)
		}
		return WriteReport{}, redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	logical, mrdr := 0, 0
	for _, node := range flat {
		if node.typ == recordTypeMRDR {
			mrdr++
		} else if standardFileRecordType(node.typ) {
			logical++
		}
	}
	if mrdr > 0 {
		diagnostics = append(diagnostics, Diagnostic{Code: DiagnosticUnsupported, Message: "retired MRDR compatibility graph emitted"})
	}
	return WriteReport{BytesWritten: written, Records: len(flat), LogicalReferences: logical, MRDRRecords: mrdr, SHA256: sha256.Sum256(encoded), Icons: icons, Diagnostics: diagnostics, revision: revision, extraElements: extraElements}, nil
}

// countDirectoryElements includes the contents of profile and icon sequences.
func countDirectoryElements(elements []core.Element) int {
	count := len(elements)
	for _, element := range elements {
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			for _, item := range sequence.Items {
				count += countDirectoryElements(item.Elements)
			}
		}
	}
	return count
}

func (f *FileSet) revisionMatches(revision uint64) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.revision == revision
}

func normalizeWriteOptions(options WriteOptions) (int64, error) {
	if options.MaxBytes < 0 || options.IconSize != 0 && !validIconSize(options.IconSize) {
		return 0, ErrInvalidOptions
	}
	if options.Profile != "" {
		if _, err := profileRulesFor(options.Profile); err != nil {
			return 0, err
		}
	}
	if options.MaxBytes == 0 {
		return 64 << 20, nil
	}
	return options.MaxBytes, nil
}

func writeAllContext(ctx context.Context, destination io.Writer, data []byte) (int64, error) {
	var written int64
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, err := destination.Write(data)
		written += int64(n)
		data = data[n:]
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func buildDirectoryHierarchy(records []sourceSnapshot) ([]*directoryNode, []*directoryNode, error) {
	records = sortedFileRecords(records)
	patients := make(map[string]*directoryNode)
	studies := make(map[string]*directoryNode)
	series := make(map[string]*directoryNode)
	root := make([]*directoryNode, 0)
	for _, stored := range records {
		r := stored.record
		if rootFileRecordType(r.RecordType) {
			root = append(root, &directoryNode{typ: r.RecordType, record: r})
			continue
		}
		patient := patients[r.PatientID]
		if patient == nil {
			patient = &directoryNode{typ: RecordTypePatient, record: r}
			patients[r.PatientID] = patient
			root = append(root, patient)
		} else if patient.record.PatientName != r.PatientName {
			return nil, nil, ErrInvalidRecord
		}
		study := studies[r.StudyInstanceUID]
		if study == nil {
			study = &directoryNode{typ: RecordTypeStudy, record: r}
			studies[r.StudyInstanceUID] = study
			patient.children = append(patient.children, study)
		} else if study.record.PatientID != r.PatientID || !sameStudyKeys(study.record, r) {
			return nil, nil, ErrInvalidRecord
		}
		seriesNode := series[r.SeriesInstanceUID]
		if seriesNode == nil {
			seriesNode = &directoryNode{typ: RecordTypeSeries, record: r}
			series[r.SeriesInstanceUID] = seriesNode
			study.children = append(study.children, seriesNode)
		} else if seriesNode.record.StudyInstanceUID != r.StudyInstanceUID || !sameSeriesKeys(seriesNode.record, r) {
			return nil, nil, ErrInvalidRecord
		}
		seriesNode.children = append(seriesNode.children, &directoryNode{typ: r.RecordType, record: r})
	}
	flat := make([]*directoryNode, 0, len(records)*4)
	var visit func([]*directoryNode)
	visit = func(nodes []*directoryNode) {
		for _, node := range nodes {
			flat = append(flat, node)
			visit(node.children)
		}
	}
	visit(root)
	groups := [][]*directoryNode{root}
	for _, node := range flat {
		if len(node.children) != 0 {
			groups = append(groups, node.children)
		}
	}
	for _, group := range groups {
		for i := 0; i+1 < len(group); i++ {
			group[i].next = group[i+1]
		}
	}
	return root, flat, nil
}

func sameStudyKeys(a, b FileRecord) bool {
	return a.StudyDate == b.StudyDate && a.StudyTime == b.StudyTime && a.StudyDescription == b.StudyDescription &&
		a.StudyID == b.StudyID && a.AccessionNumber == b.AccessionNumber
}

func sameSeriesKeys(a, b FileRecord) bool {
	return a.Modality == b.Modality && a.SeriesNumber == b.SeriesNumber
}

func encodeDirectoryTwoPass(ctx context.Context, roots, flat []*directoryNode, uid, id string, descriptor FileID, descriptorCharacterSet string, maxBytes int64, admission ...Limits) ([]byte, error) {
	passOne, err := encodeDirectory(ctx, roots, flat, uid, id, descriptor, descriptorCharacterSet, maxBytes)
	if err != nil {
		return nil, err
	}
	offsets, err := encodedDirectoryOffsets(passOne, len(flat))
	if err != nil {
		return nil, err
	}
	for i, offset := range offsets {
		flat[i].offset = offset
	}
	passTwo, err := encodeDirectory(ctx, roots, flat, uid, id, descriptor, descriptorCharacterSet, maxBytes)
	if err != nil {
		return nil, err
	}
	secondOffsets, err := encodedDirectoryOffsets(passTwo, len(flat))
	if err != nil || len(secondOffsets) != len(offsets) {
		return nil, redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	for i := range offsets {
		if offsets[i] != secondOffsets[i] {
			return nil, redactedFileSetError{class: ErrDICOMDIRWrite}
		}
	}
	if err := validateEncodedDirectory(passTwo, roots, flat, admission...); err != nil {
		return nil, err
	}
	return passTwo, nil
}

func encodeDirectory(ctx context.Context, roots, flat []*directoryNode, uid, id string, descriptor FileID, descriptorCharacterSet string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	elements := []core.Element{
		textElement(tagFileSetID, core.VRCS, id),
		ulElement(tagOffsetFirstRootDirectoryRecord, nodeOffset(firstNode(roots))),
		ulElement(tagOffsetLastRootDirectoryRecord, nodeOffset(lastNode(roots))),
		usElement(tagFileSetConsistencyFlag, 0),
	}
	if len(descriptor) != 0 {
		elements = append(elements, textValuesElement(tagFileSetDescriptorFileID, core.VRCS, []string(descriptor)))
		if descriptorCharacterSet != "" {
			elements = append(elements, textElement(tagDescriptorCharacterSet, core.VRCS, descriptorCharacterSet))
		}
	}
	items := make([]core.DataSet, len(flat))
	for i, node := range flat {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		items[i] = directoryRecordDataSet(node, nodeOffset(node.next), firstChildOffset(node))
	}
	elements = append(elements, core.Element{
		Header: core.ElementHeader{Tag: tagDirectoryRecordSequence, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: items},
	})
	dataset := object.FromElements(elements, std.Dictionary)
	meta := object.FromElements([]core.Element{
		uiElementValue(tagMediaStorageSOPClassUIDDirectory, mediaStorageDirectoryStorageUID),
		uiElementValue(tagMediaStorageSOPInstanceUIDDirectory, uid),
		uiElementValue(tagTransferSyntaxUIDDirectory, transfer.ExplicitVRLittleEndian.UID),
	}, std.Dictionary)
	file := &object.File{Meta: meta, Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
	var buffer limitedBuffer
	buffer.max = maxBytes
	if err := object.WriteFileWithOptions(&buffer, file, object.WriteFileOptions{OmitReconciledDataSetSOPUIDs: true}); err != nil {
		if errors.Is(err, ErrResourceLimit) {
			return nil, err
		}
		return nil, redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	return append([]byte(nil), buffer.Bytes()...), nil
}

type limitedBuffer struct {
	bytes.Buffer
	max int64
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	if int64(len(data)) > b.max-int64(b.Len()) {
		return 0, ErrResourceLimit
	}
	return b.Buffer.Write(data)
}

func firstNode(nodes []*directoryNode) *directoryNode {
	if len(nodes) == 0 {
		return nil
	}
	return nodes[0]
}

func lastNode(nodes []*directoryNode) *directoryNode {
	if len(nodes) == 0 {
		return nil
	}
	return nodes[len(nodes)-1]
}

func nodeOffset(node *directoryNode) uint32 {
	if node == nil {
		return 0
	}
	return node.offset
}

func firstChildOffset(node *directoryNode) uint32 {
	return nodeOffset(firstNode(node.children))
}

func directoryRecordDataSet(node *directoryNode, next, lower uint32) core.DataSet {
	elements := []core.Element{
		ulElement(tagOffsetNextDirectoryRecord, next), usElement(tagRecordInUseFlag, 0xFFFF),
		ulElement(tagOffsetLowerLevelRecord, lower), textElement(tagDirectoryRecordType, core.VRCS, node.typ.String()),
	}
	// Common selection strings and owned leaf keys are decoded Unicode. Each
	// directory record declares UTF-8 independently of the referenced file.
	if len(node.imported.Elements) > 0 {
		elements = append(elements, textElement(tagSpecificCharacterSetFS, core.VRCS, "ISO_IR 192"))
	} else if node.record.SpecificCharacterSet != "" {
		elements = append(elements, textValuesElement(tagSpecificCharacterSetFS, core.VRCS, strings.Split(node.record.SpecificCharacterSet, "\\")))
	}
	switch node.typ {
	case recordTypeMRDR:
		elements = append(elements, textValuesElement(tagReferencedFileID, core.VRCS, []string(node.record.FileID)), ulElement(tagNumberOfReferences, node.references))
	case RecordTypePatient:
		elements = append(elements, textElement(tagPatientNameFS, core.VRPN, node.record.PatientName), textElement(tagPatientIDFS, core.VRLO, node.record.PatientID))
		elements = append(elements, node.extras.patient...)
	case RecordTypeStudy:
		elements = append(elements,
			textElement(tagStudyDateFS, core.VRDA, node.record.StudyDate), textElement(tagStudyTimeFS, core.VRTM, node.record.StudyTime),
			textElement(tagStudyDescriptionFS, core.VRLO, node.record.StudyDescription), uiElementValue(tagStudyInstanceUIDFS, node.record.StudyInstanceUID),
			textElement(tagStudyIDFS, core.VRSH, node.record.StudyID), textElement(tagAccessionNumberFS, core.VRSH, node.record.AccessionNumber))
	case RecordTypeSeries:
		elements = append(elements, textElement(tagModalityFS, core.VRCS, node.record.Modality), uiElementValue(tagSeriesInstanceUIDFS, node.record.SeriesInstanceUID), textElement(tagSeriesNumberFS, core.VRIS, node.record.SeriesNumber))
		elements = append(elements, node.extras.series...)
	default:
		if node.mrdr == nil {
			elements = append(elements, textValuesElement(tagReferencedFileID, core.VRCS, []string(node.record.FileID)))
		} else {
			elements = append(elements, ulElement(tagMRDRDirectoryRecordOffset, nodeOffset(node.mrdr)))
		}
		elements = append(elements,
			uiElementValue(tagReferencedSOPClassUIDInFile, node.record.SOPClassUID), uiElementValue(tagReferencedSOPInstanceUIDInFile, node.record.SOPInstanceUID),
			uiElementValue(tagReferencedTransferSyntaxUIDInFile, node.record.TransferSyntaxUID))
		if node.record.InstanceNumber != "" && len(node.record.keys.Elements) == 0 {
			elements = append(elements, textElement(tagInstanceNumberFS, core.VRIS, node.record.InstanceNumber))
		}
		if len(node.record.RelatedGeneralSOPClassUIDs) != 0 {
			elements = append(elements, textValuesElement(tagReferencedRelatedGeneralSOPClassUIDInFile, core.VRUI, node.record.RelatedGeneralSOPClassUIDs))
		}
		elements = append(elements, node.extras.image...)
		elements = append(elements, node.record.keys.Elements...)
	}
	// Retain opaque and optional imported keys, then overlay defined keys.
	// Conditional schema keys are removed before extraction so stale values
	// cannot survive a Refresh when their source condition becomes false.
	reserved := map[core.Tag]bool{tagReferencedFileID: true, tagReferencedSOPClassUIDInFile: true, tagReferencedSOPInstanceUIDInFile: true,
		tagReferencedTransferSyntaxUIDInFile: true, tagReferencedRelatedGeneralSOPClassUIDInFile: true, tagMRDRDirectoryRecordOffset: true, tagNumberOfReferences: true}
	for _, rule := range directoryKeyRules(node.typ) {
		reserved[rule.tag] = true
	}
	merged := make(map[core.Tag]core.Element, len(elements)+len(node.imported.Elements))
	for _, e := range node.imported.Elements {
		if !reserved[e.Tag()] && !e.Tag().IsGroupLength() {
			merged[e.Tag()] = e
		}
	}
	for _, e := range elements {
		merged[e.Tag()] = e
	}
	elements = elements[:0]
	for _, e := range merged {
		elements = append(elements, e)
	}
	sort.Slice(elements, func(i, j int) bool { return elements[i].Tag().Less(elements[j].Tag()) })
	return core.DataSet{Elements: elements}
}

func textElement(tag core.Tag, vr core.VR, value string) core.Element {
	return textValuesElement(tag, vr, []string{value})
}

func textValuesElement(tag core.Tag, vr core.VR, values []string) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: vr},
		Value:  core.StringValue(append([]string(nil), values...)),
	}
}

func uiElementValue(tag core.Tag, value string) core.Element {
	raw := []byte(core.NormalizeUID(value))
	if len(raw)%2 != 0 {
		raw = append(raw, 0)
	}
	return core.NewRawElement(tag, core.VRUI, raw)
}

func ulElement(tag core.Tag, value uint32) core.Element {
	return object.ULElement(tag, binary.LittleEndian, value)
}

func usElement(tag core.Tag, value uint16) core.Element {
	return object.USElement(tag, binary.LittleEndian, value)
}

func encodedDirectoryOffsets(encoded []byte, want int) ([]uint32, error) {
	file, err := object.ReadFile(bytes.NewReader(encoded))
	if err != nil {
		return nil, redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	defer file.Close()
	items, ok := file.Dataset.GetSequence(tagDirectoryRecordSequence)
	if !ok || len(items) != want {
		return nil, redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	offsets := make([]uint32, len(items))
	seen := make(map[uint32]bool, len(items))
	for i, item := range items {
		offset, ok := item.ItemOffset()
		if !ok || offset <= 0 || offset > math.MaxUint32 || seen[uint32(offset)] {
			return nil, redactedFileSetError{class: ErrDICOMDIRWrite}
		}
		offsets[i] = uint32(offset)
		seen[offsets[i]] = true
	}
	return offsets, nil
}

func validateEncodedDirectory(encoded []byte, roots, flat []*directoryNode, admission ...Limits) error {
	file, err := object.ReadFile(bytes.NewReader(encoded))
	if err != nil {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	defer file.Close()
	if file.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID || file.Dataset.Has(tagSOPClassUIDFS) || file.Dataset.Has(tagSOPInstanceUIDFS) {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	metaClass, _ := file.Meta.GetUID(tagMediaStorageSOPClassUIDDirectory)
	metaInstance, _ := file.Meta.GetUID(tagMediaStorageSOPInstanceUIDDirectory)
	if validateDirectoryFileSetUID(file.Dataset, metaInstance) != nil {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	if metaClass != mediaStorageDirectoryStorageUID {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	first, okFirst := uint32Value(file.Dataset, tagOffsetFirstRootDirectoryRecord)
	last, okLast := uint32Value(file.Dataset, tagOffsetLastRootDirectoryRecord)
	if !okFirst || !okLast || first != nodeOffset(firstNode(roots)) || last != nodeOffset(lastNode(roots)) {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	items, ok := file.Dataset.GetSequence(tagDirectoryRecordSequence)
	if !ok || len(items) != len(flat) {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	byOffset := make(map[uint32]*object.Object, len(items))
	for _, item := range items {
		offset, ok := item.ItemOffset()
		if !ok {
			return redactedFileSetError{class: ErrDICOMDIRWrite}
		}
		byOffset[uint32(offset)] = item
	}
	legacy := false
	for _, node := range flat {
		if node.typ == recordTypeMRDR {
			legacy = true
		}
	}
	references, err := validateDirectoryDataSetLegacy(file.Dataset, legacy, admission...)
	if err != nil {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	// Check every relocated edge, including physical MRDR targets.
	for _, node := range flat {
		item := byOffset[node.offset]
		next, okNext := uint32Value(item, tagOffsetNextDirectoryRecord)
		lower, okLower := uint32Value(item, tagOffsetLowerLevelRecord)
		if !okNext || !okLower || next != nodeOffset(node.next) || lower != firstChildOffset(node) {
			return ErrDICOMDIRWrite
		}
		if node.mrdr != nil {
			target, ok := uint32Value(item, tagMRDRDirectoryRecordOffset)
			if !ok || target != nodeOffset(node.mrdr) {
				return ErrDICOMDIRWrite
			}
		}
	}
	expected := make([]FileRecord, 0)
	for _, node := range flat {
		if standardFileRecordType(node.typ) {
			expected = append(expected, node.record)
		}
	}
	if len(references) != len(expected) {
		return redactedFileSetError{class: ErrDICOMDIRWrite}
	}
	for i := range references {
		got, want := references[i], expected[i]
		if !equalFileID(got.fileID, want.FileID) || got.recordType != want.RecordType ||
			!encodedSelectionMatchesRecord(got.selection, want) || got.sopClassUID != want.SOPClassUID ||
			got.sopInstanceUID != want.SOPInstanceUID || got.transferSyntaxUID != want.TransferSyntaxUID ||
			!equalStrings(got.relatedGeneralSOPClassUIDs, want.RelatedGeneralSOPClassUIDs) || !directoryKeysEqual(got.selection.keys, want.keys) {
			return redactedFileSetError{class: ErrDICOMDIRWrite}
		}
	}
	return nil
}

func (f *FileSet) revalidateSources(ctx context.Context, records []sourceSnapshot) error {
	rootInfo, err := os.Lstat(f.root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(f.rootInfo, rootInfo) {
		return redactedFileSetError{class: ErrSourceChanged}
	}
	fileIDs := make([]FileID, len(records))
	for i := range records {
		fileIDs[i] = records[i].record.FileID
	}
	if sourceIndex, spellingErr := validateExactFileIDSpellings(ctx, f.root, fileIDs); spellingErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if sourceIndex >= 0 {
			return sourceValidationError{sourceIndex: sourceIndex}
		}
		return redactedFileSetError{class: ErrSourceChanged}
	}
	for sourceIndex, stored := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, _, err := OpenReferencedFile(f.root, Reference{FileID: []string(stored.record.FileID)})
		if err != nil {
			return sourceValidationError{sourceIndex: sourceIndex}
		}
		info, statErr := file.Stat()
		if statErr != nil || !os.SameFile(stored.info, info) || info.Size() != stored.size || !info.ModTime().Equal(stored.modTime) {
			file.Close()
			return sourceValidationError{sourceIndex: sourceIndex}
		}
		result, readErr := index.Read(ctx, index.ReaderAtSource("", file, info.Size()), f.metadataIndexOptions())
		closeErr := file.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if readErr != nil || closeErr != nil {
			return sourceValidationError{sourceIndex: sourceIndex}
		}
		current, currentErr := fileRecordFromIndex(stored.record.FileID, result, f.options.InventMissingKeys || len(stored.record.InventedKeys) > 0)
		adoptInventedKeys(&current, stored.record)
		if currentErr == nil {
			currentErr = f.readDirectoryKeys(ctx, &current)
		}
		if currentErr != nil || !sameReferencedIdentity(stored.record, current) || !directoryKeysEqual(stored.record.keys, current.keys) {
			return sourceValidationError{sourceIndex: sourceIndex}
		}
	}
	return nil
}

func sameReferencedIdentity(a, b FileRecord) bool {
	return a.RecordType == b.RecordType && a.SpecificCharacterSet == b.SpecificCharacterSet &&
		encodedSelectionMatchesRecord(a, b) && a.SOPClassUID == b.SOPClassUID &&
		a.SOPInstanceUID == b.SOPInstanceUID && a.TransferSyntaxUID == b.TransferSyntaxUID &&
		equalStrings(a.RelatedGeneralSOPClassUIDs, b.RelatedGeneralSOPClassUIDs)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CommitDICOMDIR transactionally publishes DICOMDIR in the FileSet root.
func CommitDICOMDIR(ctx context.Context, fileSet *FileSet, options WriteOptions) (report WriteReport, err error) {
	if fileSet == nil {
		return WriteReport{}, ErrInvalidOptions
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return WriteReport{}, err
	}
	maxBytes, optionErr := normalizeWriteOptions(options)
	if optionErr != nil {
		return WriteReport{}, optionErr
	}
	rootInfo, rootErr := os.Lstat(fileSet.root)
	if rootErr != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(fileSet.rootInfo, rootInfo) {
		return WriteReport{}, redactedFileSetError{class: ErrSourceChanged}
	}
	fileSet.mu.RLock()
	imported := fileSet.directory
	fileSet.mu.RUnlock()
	if imported != nil && !imported.matches(ctx) {
		if err := ctx.Err(); err != nil {
			return WriteReport{}, err
		}
		return WriteReport{}, ErrSourceChanged
	}
	destination := filepath.Join(fileSet.root, "DICOMDIR")
	var previous fsSnapshot
	if info, statErr := os.Lstat(destination); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return WriteReport{}, redactedFileSetError{class: ErrTransaction}
		}
		previous = fsSnapshot{exists: true, info: info}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return WriteReport{}, redactedFileSetError{class: ErrTransaction}
	}
	temp, createErr := os.CreateTemp(fileSet.root, ".DICOMDIR.tmp-")
	if createErr != nil {
		return WriteReport{}, redactedFileSetError{class: ErrTransaction}
	}
	tempPath := temp.Name()
	defer func() {
		root, err := os.Lstat(fileSet.root)
		if err == nil && os.SameFile(fileSet.rootInfo, root) {
			_ = os.Remove(tempPath)
		}
	}()
	if chmodErr := temp.Chmod(0o600); chmodErr != nil {
		temp.Close()
		return WriteReport{}, redactedFileSetError{class: ErrTransaction}
	}
	report, err = WriteDICOMDIR(ctx, temp, fileSet, options)
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return WriteReport{}, err
	}
	fileSet.mu.RLock()
	if fileSet.revision != report.revision {
		fileSet.mu.RUnlock()
		return WriteReport{}, redactedFileSetError{class: ErrSourceChanged}
	}
	records := snapshotFileRecords(fileSet.records)
	fileSetOptions := fileSet.options
	fileSet.mu.RUnlock()
	readOptions, limitErr := directoryReadOptions(maxBytes, report.Records, report.extraElements)
	if limitErr != nil {
		return WriteReport{}, limitErr
	}
	if _, parseErr := parseFileSet(ctx, tempPath, readOptions, fileSetOptions); parseErr != nil {
		if errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded) || errors.Is(parseErr, ErrResourceLimit) {
			return WriteReport{}, parseErr
		}
		return WriteReport{}, redactedFileSetError{class: ErrTransaction}
	}
	if err := fileSet.revalidateSources(ctx, records); err != nil {
		return WriteReport{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteReport{}, err
	}
	unlock, err := lockDirectoryPublication(ctx, fileSet.rootInfo)
	if err != nil {
		return WriteReport{}, err
	}
	defer unlock()
	if options.PublicationGuard != nil {
		if err := options.PublicationGuard(ctx, destination); err != nil {
			return WriteReport{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return WriteReport{}, err
	}
	fileSet.mu.RLock()
	defer fileSet.mu.RUnlock()
	if fileSet.revision != report.revision {
		return WriteReport{}, redactedFileSetError{class: ErrSourceChanged}
	}
	currentRoot, rootErr := os.Lstat(fileSet.root)
	if rootErr != nil || !os.SameFile(fileSet.rootInfo, currentRoot) || currentRoot.Mode()&os.ModeSymlink != 0 {
		return WriteReport{}, ErrSourceChanged
	}
	if imported != nil && !imported.matches(ctx) {
		if err := ctx.Err(); err != nil {
			return WriteReport{}, err
		}
		return WriteReport{}, ErrSourceChanged
	}
	if !sameDestinationSnapshot(destination, previous) {
		return WriteReport{}, redactedFileSetError{class: ErrTransaction}
	}
	if err := replaceFileAtomically(tempPath, destination); err != nil {
		return WriteReport{}, redactedFileSetError{class: ErrTransaction}
	}
	if err := syncFileSetDirectory(fileSet.root); err != nil {
		// The atomic replacement has already committed; preserve the report so
		// callers can distinguish a post-publication durability failure from a
		// pre-publication rejection.
		return report, redactedFileSetError{class: ErrTransaction}
	}
	return report, nil
}

type fsSnapshot struct {
	exists bool
	info   os.FileInfo
}

func sameDestinationSnapshot(path string, snapshot fsSnapshot) bool {
	info, err := os.Lstat(path)
	if !snapshot.exists {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(snapshot.info, info)
}

// ParseFileSet strictly imports a DICOMDIR authored within the supported IMAGE
// hierarchy and revalidates every active reference against its source file.
func ParseFileSet(path string) (*FileSet, error) {
	return ParseFileSetWithOptions(context.Background(), path, Options{})
}

func directoryReadOptions(maxBytes int64, maxRecords, extraElements int) (object.ReadFileOptions, error) {
	const elementsPerRecord = 16
	if maxBytes <= 0 || maxRecords < 0 || extraElements < 0 || extraElements > math.MaxInt-32 || maxRecords > (math.MaxInt-32-extraElements)/elementsPerRecord {
		return object.ReadFileOptions{}, ErrResourceLimit
	}
	return object.ReadFileOptions{
		MaxElementBytes:  16 << 20,
		MaxTotalBytes:    maxBytes,
		MaxSequenceDepth: 64,
		MaxElements:      maxRecords*elementsPerRecord + 32 + extraElements,
		MaxFragments:     200_000,
		SkipPixelData:    true,
	}, nil
}

func parseFileSet(ctx context.Context, path string, readOptions object.ReadFileOptions, sourceOptions Options) (*FileSet, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot, err := readDirectorySnapshot(ctx, path, sourceOptions, sourceOptions.LegacyMRDR, readOptions.MaxTotalBytes)
	if err != nil {
		return nil, err
	}
	set, _, err := prepareImportedFileSet(ctx, snapshot, nil, FileSetUpdateOptions{Mode: FileSetAppend, SourceOptions: sourceOptions, LegacyMRDR: sourceOptions.LegacyMRDR, MaxBytes: readOptions.MaxTotalBytes})
	return set, err
}

func isParserResourceLimit(err error) bool {
	for _, target := range []error{
		parser.ErrMaxDepthExceeded, parser.ErrMaxElementBytesExceeded, parser.ErrMaxElementsExceeded,
		parser.ErrMaxFragmentsExceeded, parser.ErrMaxTotalBytesExceeded,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

func encodedSelectionMatchesRecord(encoded, record FileRecord) bool {
	return encoded.PatientName == record.PatientName && encoded.PatientID == record.PatientID &&
		encoded.StudyDate == record.StudyDate && encoded.StudyTime == record.StudyTime &&
		encoded.StudyDescription == record.StudyDescription && encoded.StudyInstanceUID == record.StudyInstanceUID &&
		encoded.StudyID == record.StudyID && encoded.AccessionNumber == record.AccessionNumber &&
		encoded.Modality == record.Modality && encoded.SeriesInstanceUID == record.SeriesInstanceUID &&
		encoded.SeriesNumber == record.SeriesNumber && encoded.InstanceNumber == record.InstanceNumber
}

func (f *FileSet) sortedRecordsSnapshot() []sourceSnapshot {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return sortedFileRecords(f.records)
}

func directoryRecordCount(records []sourceSnapshot) int {
	patients, studies, series := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, stored := range records {
		if rootFileRecordType(stored.record.RecordType) {
			continue
		}
		patients[stored.record.PatientID] = true
		studies[stored.record.StudyInstanceUID] = true
		series[stored.record.SeriesInstanceUID] = true
	}
	return len(patients) + len(studies) + len(series) + len(records)
}
