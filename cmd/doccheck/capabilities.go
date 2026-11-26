package main

import (
	"fmt"
	"strings"
)

// The capability entry point uses one row per area. Duplicate rows can each
// look plausible while contradicting one another, so check the inventory
// independently of link and anchor validation.
func (c *checker) checkCapabilityAreas(name, content string) {
	if c.exempt(c.moduleRelative(name), "capabilities", parseDirectives(content)) {
		return
	}
	seen := make(map[string]int)
	active, table := false, false
	var fence fenceState
	for index, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if updateFence(trimmed, &fence, index+1) || fence.length != 0 {
			table = false
			continue
		}
		if heading := headingPattern.FindStringSubmatch(trimmed); heading != nil {
			if len(heading[1]) <= 2 {
				active = heading[2] == "Supported surfaces"
			}
			table = false
			continue
		}
		if !active || !strings.HasPrefix(trimmed, "|") || strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			table = false
			continue
		}
		cells := strings.Split(trimmed, "|")
		if len(cells) < 3 {
			continue
		}
		area := strings.Join(strings.Fields(cells[1]), " ")
		key := strings.ToLower(area)
		if key == "area" {
			table = true
			continue
		}
		if !table || key == "" || strings.Trim(key, "-: ") == "" {
			continue
		}
		if first, ok := seen[key]; ok {
			c.add(name, index+1, "capabilities", fmt.Sprintf("duplicate capability area %q (first at line %d)", area, first))
		} else {
			seen[key] = index + 1
		}
	}
	if len(seen) == 0 {
		c.add(name, 1, "capabilities", "missing or empty Supported surfaces inventory")
	}
}
