package dicomdir

import (
	"context"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

type directorySnapshot struct {
	path                   string
	info                   fs.FileInfo
	hash                   [32]byte
	roots                  []*directoryNode
	flat                   []*directoryNode
	references             []encodedReference
	uid, id                string
	descriptor             FileID
	descriptorCharacterSet string
	keyBytes               int64
	legacy                 bool
}

func captureDirectoryRecordKeys(src *object.Object, typ RecordType, limits Limits, contexts ...context.Context) (core.DataSet, int64, error) {
	ctx := context.Background()
	if len(contexts) > 0 && contexts[0] != nil {
		ctx = contexts[0]
	}
	if err := normalizeFileSetLimits(&limits); err != nil {
		return core.DataSet{}, 0, err
	}
	b := &directoryKeyBudget{ctx: ctx, limits: limits}
	keys := core.DataSet{}
	for _, rule := range directoryKeyRules(typ) {
		if !src.Has(rule.tag) {
			continue
		}
		e, err := ownDirectoryElement(src, rule.tag, b, 0)
		if err != nil {
			return core.DataSet{}, 0, err
		}
		keys.Elements = append(keys.Elements, e)
	}
	return keys, b.bytes, nil
}

func hashDirectoryHandle(ctx context.Context, handle io.ReadSeeker, maxBytes int64) ([32]byte, error) {
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return [32]byte{}, ErrInvalidRecord
	}
	hash := sha256.New()
	buffer := make([]byte, 32<<10)
	total := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return [32]byte{}, err
		}
		n, err := handle.Read(buffer)
		if n > 0 {
			if int64(n) > maxBytes-total {
				return [32]byte{}, ErrResourceLimit
			}
			total += int64(n)
			_, _ = hash.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return [32]byte{}, ErrInvalidRecord
		}
		if n == 0 {
			return [32]byte{}, io.ErrNoProgress
		}
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func readDirectorySnapshot(ctx context.Context, path string, options Options, legacy bool, maxBytes int64) (*directorySnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := normalizeFileSetLimits(&options.Limits); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrInvalidRecord
	}
	handle, _, err := OpenReferencedFile(filepath.Dir(abs), Reference{FileID: []string{filepath.Base(abs)}})
	if err != nil {
		return nil, ErrInvalidRecord
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, ErrResourceLimit
	}
	digest, err := hashDirectoryHandle(ctx, handle, maxBytes)
	if err != nil {
		return nil, err
	}
	// Bound the source by the admitted physical extent. The parser's stream
	// budget refuses a new tag at the limit, so use the section's EOF to
	// finish reading an exact-size, uncompressed directory.
	section := io.NewSectionReader(handle, 0, info.Size())
	header, dataset, err := readSelectedDirectoryMetadata(ctx, section, "", options.Limits, 0, true)
	if err != nil {
		if isParserResourceLimit(err) {
			return nil, ErrResourceLimit
		}
		return nil, err
	}
	class, cok := requiredUID(header.Meta, tagMediaStorageSOPClassUIDDirectory)
	instance, iok := requiredUID(header.Meta, tagMediaStorageSOPInstanceUIDDirectory)
	if !cok || !iok || class != mediaStorageDirectoryStorageUID {
		return nil, ErrInvalidRecord
	}
	if dataset.Has(tagSOPClassUIDFS) || dataset.Has(tagSOPInstanceUIDFS) || dataset.Has(core.TagPixelData) {
		return nil, ErrInvalidRecord
	}
	id, ok := typeTwoStringWithVR(dataset, tagFileSetID, core.VRCS)
	if !ok {
		return nil, ErrInvalidRecord
	}
	if err := validateDirectoryFileSetUID(dataset, instance); err != nil {
		return nil, err
	}
	references, err := validateDirectoryDataSetLegacyContext(ctx, dataset, legacy, options.Limits)
	if err != nil {
		return nil, err
	}
	snapshot := &directorySnapshot{path: abs, info: info, hash: digest, references: references, uid: instance, id: id, legacy: legacy}
	for _, ref := range references {
		snapshot.keyBytes += ref.selection.keyBytes
	}
	if dataset.Has(tagFileSetDescriptorFileID) {
		ids, ok := stringsWithVR(dataset, tagFileSetDescriptorFileID, core.VRCS)
		if !ok {
			return nil, ErrInvalidRecord
		}
		if len(ids) > 0 {
			snapshot.descriptor, err = NewFileID(ids...)
			if err != nil {
				return nil, err
			}
		}
	}
	if dataset.Has(tagDescriptorCharacterSet) {
		snapshot.descriptorCharacterSet, ok = requiredStringWithVR(dataset, tagDescriptorCharacterSet, core.VRCS)
		if !ok || len(snapshot.descriptor) == 0 {
			return nil, ErrInvalidRecord
		}
	}
	items, _ := dataset.GetSequence(tagDirectoryRecordSequence)
	byOffset := make(map[uint32]*directoryNode, len(items))
	rawByOffset := make(map[uint32]*object.Object, len(items))
	refByOffset := make(map[uint32]encodedReference, len(references))
	for _, ref := range references {
		refByOffset[ref.offset] = ref
	}
	for _, item := range items {
		offset, _ := item.ItemOffset()
		typ, _ := item.GetString(tagDirectoryRecordType)
		rawByOffset[uint32(offset)] = item
		flag, _ := uint16Value(item, tagRecordInUseFlag)
		if flag == 0 {
			byOffset[uint32(offset)] = &directoryNode{typ: RecordType(typ), offset: uint32(offset), inactive: true}
			continue
		}
		remaining := options.Limits.MaxTotalRecordKeyBytes - snapshot.keyBytes
		if remaining <= 0 {
			return nil, ErrResourceLimit
		}
		ownLimits := options.Limits
		if remaining < ownLimits.MaxRecordKeyBytes {
			ownLimits.MaxRecordKeyBytes = remaining
		}
		b := &directoryKeyBudget{ctx: ctx, limits: ownLimits}
		owned, err := ownDirectoryDataSet(item, b, 0)
		if err != nil {
			return nil, err
		}
		if b.bytes > options.Limits.MaxTotalRecordKeyBytes-snapshot.keyBytes {
			return nil, ErrResourceLimit
		}
		snapshot.keyBytes += b.bytes
		node := &directoryNode{typ: RecordType(typ), imported: owned, offset: uint32(offset)}
		if ref, ok := refByOffset[uint32(offset)]; ok {
			node.record = ref.selection // private immutable shared subset; no second payload copy.
		}
		if node.typ == recordTypeMRDR {
			ids, _ := item.GetStrings(tagReferencedFileID)
			node.record.FileID = append(FileID(nil), ids...)
		}
		byOffset[uint32(offset)] = node
		snapshot.flat = append(snapshot.flat, node)
	}
	var group func(uint32) []*directoryNode
	group = func(offset uint32) []*directoryNode {
		var nodes []*directoryNode
		for offset != 0 {
			node := byOffset[offset]
			raw := rawByOffset[offset]
			if node.inactive {
				offset, _ = uint32Value(raw, tagOffsetNextDirectoryRecord)
				continue
			}
			nodes = append(nodes, node)
			lower, _ := uint32Value(raw, tagOffsetLowerLevelRecord)
			node.children = group(lower)
			if target, ok := uint32Value(raw, tagMRDRDirectoryRecordOffset); ok {
				node.mrdr = byOffset[target]
			}
			next, _ := uint32Value(raw, tagOffsetNextDirectoryRecord)
			node.next = byOffset[next]
			offset = next
		}
		return nodes
	}
	first, _ := uint32Value(dataset, tagOffsetFirstRootDirectoryRecord)
	snapshot.roots = group(first)
	// Common entity identity/keys come from the actual imported record, not
	// from an arbitrary first referenced child.
	for _, node := range snapshot.flat {
		raw := object.FromDataSet(node.imported, nil)
		switch node.typ {
		case RecordTypePatient:
			node.record.PatientID, _ = raw.GetString(tagPatientIDFS)
			node.record.PatientName, _ = raw.GetString(tagPatientNameFS)
		case RecordTypeStudy:
			node.record.StudyInstanceUID, _ = raw.GetUID(tagStudyInstanceUIDFS)
			node.record.StudyDate, _ = raw.GetString(tagStudyDateFS)
			node.record.StudyTime, _ = raw.GetString(tagStudyTimeFS)
			node.record.StudyDescription, _ = raw.GetString(tagStudyDescriptionFS)
			node.record.StudyID, _ = raw.GetString(tagStudyIDFS)
			node.record.AccessionNumber, _ = raw.GetString(tagAccessionNumberFS)
		case RecordTypeSeries:
			node.record.SeriesInstanceUID, _ = raw.GetUID(tagSeriesInstanceUIDFS)
			node.record.Modality, _ = raw.GetString(tagModalityFS)
			node.record.SeriesNumber, _ = raw.GetString(tagSeriesNumberFS)
		}
	}
	current, err := handle.Stat()
	if err != nil || !os.SameFile(info, current) || current.Size() != info.Size() || !current.ModTime().Equal(info.ModTime()) {
		return nil, ErrSourceChanged
	}
	return snapshot, nil
}

func (s *directorySnapshot) matches(ctx context.Context) bool {
	if s == nil {
		return true
	}
	handle, _, err := OpenReferencedFile(filepath.Dir(s.path), Reference{FileID: []string{filepath.Base(s.path)}})
	if err != nil {
		return false
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || !os.SameFile(s.info, info) || info.Size() != s.info.Size() || !info.ModTime().Equal(s.info.ModTime()) {
		return false
	}
	digest, err := hashDirectoryHandle(ctx, handle, s.info.Size())
	return err == nil && digest == s.hash
}

func directoryNodeKey(node *directoryNode) string {
	switch node.typ {
	case RecordTypePatient:
		return "P:" + node.record.PatientID
	case RecordTypeStudy:
		return "S:" + node.record.StudyInstanceUID
	case RecordTypeSeries:
		return "R:" + node.record.SeriesInstanceUID
	default:
		return "F:" + node.record.FileID.String()
	}
}

func mergeImportedDirectory(s *directorySnapshot, fresh []*directoryNode) ([]*directoryNode, []*directoryNode, error) {
	if s == nil {
		return nil, nil, ErrInvalidOptions
	}
	current := map[string]FileRecord{}
	var collect func([]*directoryNode)
	collect = func(nodes []*directoryNode) {
		for _, n := range nodes {
			if standardFileRecordType(n.typ) {
				current[n.record.FileID.String()] = n.record
			}
			collect(n.children)
		}
	}
	collect(fresh)
	aliases := map[string][]*directoryNode{}
	for _, n := range s.flat {
		if standardFileRecordType(n.typ) {
			aliases[n.record.FileID.String()] = append(aliases[n.record.FileID.String()], n)
		}
	}
	assigned := map[string]bool{}
	physical := map[*directoryNode]*directoryNode{}
	cloneLeaf := func(prior *directoryNode, r FileRecord) *directoryNode {
		n := &directoryNode{typ: r.RecordType, record: cloneFileRecord(r)}
		if prior != nil {
			n.imported = cloneDirectoryKeys(prior.imported)
			if prior.mrdr != nil {
				target := physical[prior.mrdr]
				if target == nil {
					target = &directoryNode{typ: recordTypeMRDR, record: FileRecord{FileID: append(FileID(nil), prior.mrdr.record.FileID...)}, imported: cloneDirectoryKeys(prior.mrdr.imported)}
					physical[prior.mrdr] = target
				}
				n.mrdr = target
				target.references++
			}
		}
		return n
	}
	var retain func(*directoryNode, string, string, string) *directoryNode
	retain = func(prior *directoryNode, patient, study, series string) *directoryNode {
		if standardFileRecordType(prior.typ) {
			r, ok := current[prior.record.FileID.String()]
			if !ok || r.PatientID != patient || r.StudyInstanceUID != study || r.SeriesInstanceUID != series {
				return nil
			}
			assigned[r.FileID.String()] = true
			return cloneLeaf(prior, r)
		}
		n := &directoryNode{typ: prior.typ, record: directoryEntityRecord(prior.record), imported: cloneDirectoryKeys(prior.imported)}
		switch n.typ {
		case RecordTypePatient:
			patient = n.record.PatientID
		case RecordTypeStudy:
			study = n.record.StudyInstanceUID
		case RecordTypeSeries:
			series = n.record.SeriesInstanceUID
		}
		for _, child := range prior.children {
			if kept := retain(child, patient, study, series); kept != nil {
				n.children = append(n.children, kept)
			}
		}
		if len(prior.children) > 0 && len(n.children) == 0 {
			return nil
		}
		// A surviving source determines refreshed common keys on this same path.
		leaves := leafNodes(n)
		for _, leaf := range leaves {
			if standardFileRecordType(leaf.typ) {
				n.record = directoryEntityRecord(leaf.record)
				break
			}
		}
		return n
	}
	var roots []*directoryNode
	for _, prior := range s.roots {
		if kept := retain(prior, "", "", ""); kept != nil {
			roots = append(roots, kept)
		}
	}
	find := func(nodes []*directoryNode, typ RecordType, key string) *directoryNode {
		for _, n := range nodes {
			if n.typ == typ {
				switch typ {
				case RecordTypePatient:
					if n.record.PatientID == key {
						return n
					}
				case RecordTypeStudy:
					if n.record.StudyInstanceUID == key {
						return n
					}
				case RecordTypeSeries:
					if n.record.SeriesInstanceUID == key {
						return n
					}
				}
			}
		}
		return nil
	}
	// Fresh hierarchy order supplies deterministic insertion order. Existing
	// duplicate entity records remain separate; first matching entity receives
	// newly discovered/moved files, while every old alias keeps its opaque keys.
	var add func([]*directoryNode) error
	add = func(nodes []*directoryNode) error {
		for _, leaf := range nodes {
			if !standardFileRecordType(leaf.typ) {
				if err := add(leaf.children); err != nil {
					return err
				}
				continue
			}
			r := leaf.record
			key := r.FileID.String()
			if assigned[key] {
				continue
			}
			var children *[]*directoryNode
			if rootFileRecordType(r.RecordType) {
				children = &roots
			} else {
				patient := find(roots, RecordTypePatient, r.PatientID)
				if patient == nil {
					patient = &directoryNode{typ: RecordTypePatient, record: directoryEntityRecord(r)}
					roots = append(roots, patient)
				} else if patient.record.PatientName != r.PatientName {
					return ErrInvalidRecord
				}
				study := find(patient.children, RecordTypeStudy, r.StudyInstanceUID)
				if study == nil {
					study = &directoryNode{typ: RecordTypeStudy, record: directoryEntityRecord(r)}
					patient.children = append(patient.children, study)
				} else if !sameStudyKeys(study.record, r) {
					return ErrInvalidRecord
				}
				series := find(study.children, RecordTypeSeries, r.SeriesInstanceUID)
				if series == nil {
					series = &directoryNode{typ: RecordTypeSeries, record: directoryEntityRecord(r)}
					study.children = append(study.children, series)
				} else if !sameSeriesKeys(series.record, r) {
					return ErrInvalidRecord
				}
				children = &series.children
			}
			if len(aliases[key]) == 0 {
				*children = append(*children, cloneLeaf(nil, r))
			} else {
				for _, prior := range aliases[key] {
					*children = append(*children, cloneLeaf(prior, r))
				}
			}
			assigned[key] = true
		}
		return nil
	}
	if err := add(fresh); err != nil {
		return nil, nil, err
	}
	var flat []*directoryNode
	var visit func([]*directoryNode)
	visit = func(nodes []*directoryNode) {
		for i, n := range nodes {
			n.next = nil
			if i+1 < len(nodes) {
				n.next = nodes[i+1]
			}
			flat = append(flat, n)
			visit(n.children)
		}
	}
	visit(roots)
	for _, prior := range s.flat {
		if n := physical[prior]; n != nil {
			flat = append(flat, n)
		}
	}
	check := directoryEntityKeyValidator{}
	for _, node := range flat {
		if node.typ != recordTypeMRDR {
			if err := check.admit(node.typ, node.record); err != nil {
				return nil, nil, err
			}
		}
	}
	return roots, flat, nil
}

// plannedDirectoryRecordCount uses identifiers and counters only. It admits
// the hierarchy before constructing nodes or cloning imported key payloads.
func plannedDirectoryRecordCount(snapshot *directorySnapshot, records []sourceSnapshot, maximum int) (int, error) {
	current := map[string]FileRecord{}
	for _, stored := range records {
		current[stored.record.FileID.String()] = stored.record
	}
	count := 0
	charge := func(n int) error {
		if n < 0 || n > maximum-count {
			return ErrResourceLimit
		}
		count += n
		return nil
	}
	entities := map[string]bool{}
	assigned := map[string]bool{}
	targets := map[*directoryNode]bool{}
	aliases := map[string][]*directoryNode{}
	if snapshot != nil {
		for _, n := range snapshot.flat {
			if standardFileRecordType(n.typ) {
				aliases[n.record.FileID.String()] = append(aliases[n.record.FileID.String()], n)
			}
		}
	}
	pathKey := func(typ RecordType, p, s, r string) string { return typ.String() + ":" + p + "/" + s + "/" + r }
	var retain func(*directoryNode, string, string, string) (bool, error)
	retain = func(n *directoryNode, p, s, r string) (bool, error) {
		if standardFileRecordType(n.typ) {
			v, ok := current[n.record.FileID.String()]
			if !ok || v.PatientID != p || v.StudyInstanceUID != s || v.SeriesInstanceUID != r {
				return false, nil
			}
			if err := charge(1); err != nil {
				return false, err
			}
			assigned[v.FileID.String()] = true
			if n.mrdr != nil {
				targets[n.mrdr] = true
			}
			return true, nil
		}
		switch n.typ {
		case RecordTypePatient:
			p = n.record.PatientID
		case RecordTypeStudy:
			s = n.record.StudyInstanceUID
		case RecordTypeSeries:
			r = n.record.SeriesInstanceUID
		}
		alive := len(n.children) == 0
		for _, child := range n.children {
			kept, err := retain(child, p, s, r)
			if err != nil {
				return false, err
			}
			alive = alive || kept
		}
		if alive {
			if err := charge(1); err != nil {
				return false, err
			}
			entities[pathKey(n.typ, p, s, r)] = true
		}
		return alive, nil
	}
	if snapshot != nil {
		for _, root := range snapshot.roots {
			if _, err := retain(root, "", "", ""); err != nil {
				return 0, err
			}
		}
	}
	for _, stored := range records {
		v := stored.record
		key := v.FileID.String()
		if assigned[key] {
			continue
		}
		if !rootFileRecordType(v.RecordType) {
			for _, entity := range []struct {
				typ     RecordType
				p, s, r string
			}{{RecordTypePatient, v.PatientID, "", ""}, {RecordTypeStudy, v.PatientID, v.StudyInstanceUID, ""}, {RecordTypeSeries, v.PatientID, v.StudyInstanceUID, v.SeriesInstanceUID}} {
				key := pathKey(entity.typ, entity.p, entity.s, entity.r)
				if !entities[key] {
					if err := charge(1); err != nil {
						return 0, err
					}
					entities[key] = true
				}
			}
		}
		n := len(aliases[key])
		if n == 0 {
			n = 1
		}
		if err := charge(n); err != nil {
			return 0, err
		}
		for _, prior := range aliases[key] {
			if prior.mrdr != nil {
				targets[prior.mrdr] = true
			}
		}
	}
	if err := charge(len(targets)); err != nil {
		return 0, err
	}
	return count, nil
}

func (s *directorySnapshot) hasMRDR() bool {
	for _, n := range s.flat {
		if n.typ == recordTypeMRDR {
			return true
		}
	}
	return false
}

func directoryEntityRecord(r FileRecord) FileRecord {
	r.keys = core.DataSet{}
	r.keyBytes = 0
	r.FileID = nil
	r.RelatedGeneralSOPClassUIDs = nil
	r.InventedKeys = nil
	return r
}

func plannedDirectoryKeyBytes(snapshot *directorySnapshot, records []sourceSnapshot, maximum int64) error {
	_, err := plannedDirectoryKeyUsage(snapshot, records, maximum)
	return err
}
func plannedDirectoryKeyUsage(snapshot *directorySnapshot, records []sourceSnapshot, maximum int64) (int64, error) {
	used := int64(0)
	aliases := map[string]int{}
	if snapshot != nil {
		used = snapshot.keyBytes
		for _, node := range snapshot.flat {
			if standardFileRecordType(node.typ) {
				aliases[node.record.FileID.String()]++
			}
		}
	}
	if used > maximum {
		return 0, ErrResourceLimit
	}
	for _, stored := range records {
		n := aliases[stored.record.FileID.String()]
		if n == 0 {
			n = 1
		}
		bytes := stored.record.keyBytes
		if bytes < 0 || bytes > 0 && int64(n) > (maximum-used)/bytes {
			return 0, ErrResourceLimit
		}
		used += bytes * int64(n)
	}
	return used, nil
}

// Empty imported entities participate too: a new source must not repurpose
// their UIDs or attach the same study/series to a different parent.
type directoryEntityKeyValidator struct {
	patients, studies, series map[string]FileRecord
	roles                     map[string]RecordType
}

func (v *directoryEntityKeyValidator) admit(typ RecordType, r FileRecord) error {
	if v.roles == nil {
		v.roles = map[string]RecordType{}
		v.patients = map[string]FileRecord{}
		v.studies = map[string]FileRecord{}
		v.series = map[string]FileRecord{}
	}
	var id string
	var role RecordType
	var records map[string]FileRecord
	switch typ {
	case RecordTypePatient:
		id = r.PatientID
		records = v.patients
	case RecordTypeStudy:
		id = r.StudyInstanceUID
		role = typ
		records = v.studies
	case RecordTypeSeries:
		id = r.SeriesInstanceUID
		role = typ
		records = v.series
	default:
		id = r.SOPInstanceUID
		role = RecordTypeImage
	}
	if id == "" {
		return nil
	}
	if role != "" {
		if old, ok := v.roles[id]; ok && old != role {
			return ErrInvalidRecord
		}
		v.roles[id] = role
	}
	if records == nil {
		return nil
	}
	if old, ok := records[id]; ok {
		switch typ {
		case RecordTypePatient:
			if old.PatientName != r.PatientName {
				return ErrInvalidRecord
			}
		case RecordTypeStudy:
			if old.PatientID != r.PatientID || !sameStudyKeys(old, r) {
				return ErrInvalidRecord
			}
		case RecordTypeSeries:
			if old.PatientID != r.PatientID || old.StudyInstanceUID != r.StudyInstanceUID || !sameSeriesKeys(old, r) {
				return ErrInvalidRecord
			}
		}
	}
	records[id] = r
	return nil
}
