package dsig

import (
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/attrpath"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrItemNotFound reports a signature location that is not an existing
// sequence item.
var ErrItemNotFound = errors.New("dicom dsig: sequence item not found")

// parseLocation parses a sequence item location: empty for the top level,
// otherwise an attrpath that selects one item, such as Signature.Path
// "(0040,A730)[0]" or "ContentSequence[0].ContentSequence[2]".
func parseLocation(text string) (attrpath.Path, error) {
	if text == "" {
		return nil, nil
	}
	path, err := attrpath.Parse(text, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrItemNotFound, err)
	}
	if !path.SelectsItem() || path.Wildcard() {
		return nil, fmt.Errorf("%w: %q does not select one item", ErrItemNotFound, text)
	}
	return path, nil
}

// segmentTag resolves the tag of a path segment in elements, including a
// segment that names a private attribute by creator.
func segmentTag(elements []core.Element, segment attrpath.Segment) (core.Tag, error) {
	if !segment.Private() {
		return segment.Tag, nil
	}
	tag, err := object.FromElements(elements, std.Dictionary).PrivateTag(segment.Tag.Group, segment.Creator, uint8(segment.Tag.Element))
	if err != nil {
		return core.Tag{}, fmt.Errorf("%w: %w", ErrItemNotFound, err)
	}
	return tag, nil
}

// locationElements returns the elements of the data set at path.
func locationElements(obj *object.Object, path attrpath.Path) ([]core.Element, error) {
	elements := obj.Elements()
	for i, segment := range path {
		tag, err := segmentTag(elements, segment)
		if err != nil {
			return nil, err
		}
		element, ok := findElement(elements, tag)
		sequence, isSequence := element.Value.(core.SequenceValue)
		if !ok || !isSequence || segment.Item >= len(sequence.Items) {
			return nil, fmt.Errorf("%w: %s", ErrItemNotFound, path[:i+1])
		}
		elements = sequence.Items[segment.Item].Elements
	}
	return elements, nil
}

// edit is a change to one data set: elements to add or replace and tags to
// remove.
type edit struct {
	put    []core.Element
	remove []core.Tag
}

// apply returns elements with the edit made.
func (e edit) apply(elements []core.Element) []core.Element {
	drop := make(map[core.Tag]bool, len(e.put)+len(e.remove))
	for _, element := range e.put {
		drop[element.Tag()] = true
	}
	for _, tag := range e.remove {
		drop[tag] = true
	}
	out := make([]core.Element, 0, len(elements)+len(e.put))
	for _, element := range elements {
		if !drop[element.Tag()] {
			out = append(out, element)
		}
	}
	return sortedElements(append(out, e.put...))
}

// updateLocation makes the edit change returns for the data set at path,
// rebuilding the enclosing items and sequences. At the top level only the
// edited elements are touched.
func updateLocation(obj *object.Object, path attrpath.Path, change func([]core.Element) (edit, error)) error {
	elements := obj.Elements()
	if len(path) == 0 {
		e, err := change(elements)
		if err != nil {
			return err
		}
		for _, tag := range e.remove {
			obj.Remove(tag)
		}
		for _, element := range e.put {
			obj.Put(element)
		}
		return nil
	}
	// rebuild returns the sequence element of path[depth] in elements with
	// the edit made below it.
	var rebuild func(elements []core.Element, depth int) (core.Element, error)
	rebuild = func(elements []core.Element, depth int) (core.Element, error) {
		segment := path[depth]
		tag, err := segmentTag(elements, segment)
		if err != nil {
			return core.Element{}, err
		}
		element, ok := findElement(elements, tag)
		sequence, isSequence := element.Value.(core.SequenceValue)
		if !ok || !isSequence || segment.Item >= len(sequence.Items) {
			return core.Element{}, fmt.Errorf("%w: %s", ErrItemNotFound, path[:depth+1])
		}
		items := append([]core.DataSet(nil), sequence.Items...)
		item := items[segment.Item].Elements
		var e edit
		if depth == len(path)-1 {
			if e, err = change(item); err != nil {
				return core.Element{}, err
			}
		} else {
			nested, err := rebuild(item, depth+1)
			if err != nil {
				return core.Element{}, err
			}
			e.put = []core.Element{nested}
		}
		items[segment.Item] = core.DataSet{Elements: e.apply(item)}
		header := element.Header
		header.Length, header.LengthSet = core.UndefinedLength, true
		return core.Element{Header: header, Value: core.SequenceValue{Items: items}}, nil
	}
	top, err := rebuild(elements, 0)
	if err != nil {
		return err
	}
	obj.Put(top)
	return nil
}
