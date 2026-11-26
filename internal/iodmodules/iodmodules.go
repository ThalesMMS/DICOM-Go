// Package iodmodules holds the Modules of the DICOM PS3.3 IODs and the
// top-level Attributes of each Module and Macro, projected from the PS3.3
// DocBook by cmd/iodmodulegen (Include rows resolved; repeating-group
// Attributes such as (60xx,3000) and nested Sequence Items are not listed).
package iodmodules

import (
	"sort"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
)

// Attribute is a top-level Attribute of a Module with its Type (1, 1C, 2,
// 2C or 3).
type Attribute struct {
	Tag  uint32
	Type string
}

// ModuleUsage is a Module of an IOD and its usage (M, U or C), with the
// condition text of a C Module.
type ModuleUsage struct {
	Module    string
	Usage     string
	Condition string
}

// Row is one row of a Module or Macro table (see Rows).
type Row struct {
	// Depth is 0 for a top-level Attribute and n for the Attributes of the
	// Items of the Sequence n levels up.
	Depth uint8
	// Tag is the Attribute's tag, or 0 for an Attribute without a plain tag
	// (a repeating group), whose nested rows are opaque.
	Tag uint32
	// Type is 1, 1C, 2, 2C or 3, or empty when the table has no Type.
	Type string
	// Include names the Module or Macro included at this depth instead of
	// an Attribute.
	Include string
	// Condition is the condition text of a 1C or 2C Attribute, or of a
	// conditional Include ("Required if ...").
	Condition string
}

// Rows returns every row of a Module or Macro table, including nested
// Sequence Items and Include rows (which name other tables for Rows).
func Rows(table string) ([]Row, bool) {
	rows, ok := moduleRows[strings.TrimSpace(table)]
	if !ok {
		rows, ok = moduleRows[strings.TrimSpace(table)+" Module"]
	}
	return rows, ok
}

// Modules returns the Modules of an IOD, named as in PS3.3 without the
// trailing "IOD" (for example "Legacy Converted Enhanced CT Image").
func Modules(iod string) ([]ModuleUsage, bool) {
	modules, ok := iodModules[iod]
	return modules, ok
}

var (
	foldedOnce sync.Once
	folded     map[string][]Attribute
)

// Attributes returns the top-level Attributes of a Module ("Patient") or a
// Macro ("Frame Content Macro"); names match regardless of case, as PS3.3
// capitalizes some names differently in IOD and Module tables.
func Attributes(module string) ([]Attribute, bool) {
	foldedOnce.Do(func() {
		folded = make(map[string][]Attribute, len(moduleAttributes))
		for name, attributes := range moduleAttributes {
			folded[strings.ToLower(name)] = attributes
		}
	})
	key := strings.ToLower(strings.TrimSpace(module))
	if attributes, ok := folded[key+" module"]; ok {
		return attributes, true
	}
	attributes, ok := folded[key]
	return attributes, ok
}

// TopLevelTags returns the tags of every top-level Attribute of the IOD's
// Modules, whatever their usage.
func TopLevelTags(iod string) (map[core.Tag]struct{}, bool) {
	modules, ok := iodModules[iod]
	if !ok {
		return nil, false
	}
	tags := map[core.Tag]struct{}{}
	for _, use := range modules {
		attributes, _ := Attributes(use.Module)
		for _, a := range attributes {
			tags[core.NewTag(uint16(a.Tag>>16), uint16(a.Tag))] = struct{}{}
		}
	}
	return tags, true
}

// IODs returns the names of every IOD, sorted.
func IODs() []string {
	names := make([]string, 0, len(iodModules))
	for name := range iodModules {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RuleTables returns the names of every Module and Macro table of Rows,
// sorted.
func RuleTables() []string {
	names := make([]string, 0, len(moduleRows))
	for name := range moduleRows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
