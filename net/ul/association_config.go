package ul

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// ErrAssociationConfig reports invalid association configuration without
// including its values, file path, or extended-negotiation payload.
var ErrAssociationConfig = errors.New("dicom ul: invalid association configuration")

// ErrAssociationConfigLimit reports admission failure before expansion/copy.
var ErrAssociationConfigLimit = errors.New("dicom ul: association configuration resource limit")

type associationConfigIOError struct{ cause error }

func (e associationConfigIOError) Error() string {
	return ErrAssociationConfig.Error() + ": I/O failure"
}
func (e associationConfigIOError) Unwrap() []error { return []error{ErrAssociationConfig, e.cause} }

func associationConfigIOFailure(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return associationConfigIOError{cause: err}
}

// AssociationConfigLimits bound parsing and detached profile expansion. Zero
// fields select finite defaults; negative values are invalid.
type AssociationConfigLimits struct {
	MaxBytes                                                             int64
	MaxLineBytes, MaxValueBytes, MaxNameBytes                            int
	MaxSections, MaxEntries, MaxProfiles                                 int
	MaxContextRules, MaxTransferSyntaxes, MaxRoleItems, MaxExtendedItems int
	MaxExtendedItemBytes, MaxExtendedBytes, MaxCompiledBytes             int
}

// AssociationConfigOptions configure a DCMTK association configuration reader.
// LocalByteOrder may be nil (host order), binary.LittleEndian or BigEndian.
type AssociationConfigOptions struct {
	Limits         AssociationConfigLimits
	LocalByteOrder binary.ByteOrder
}

// AssociationConfigMode distinguishes a wire proposal from an acceptor catalog.
type AssociationConfigMode uint8

const (
	AssociationConfigModeRequestor AssociationConfigMode = iota + 1
	AssociationConfigModeAcceptor
)

type associationConfigContext struct{ sop, syntaxList string }
type associationConfigProfile struct{ contexts, roles, extended string }
type associationConfigSection struct {
	entries map[string]string
	order   []string
}

// AssociationConfig is immutable after parsing. Profiles return detached data;
// neither the source reader nor an open handle is retained.
type AssociationConfig struct {
	limits   AssociationConfigLimits
	syntaxes map[string][]string
	contexts map[string][]associationConfigContext
	roles    map[string][]RoleSelectionItem
	extended map[string][]SopClassExtendedNegotiationItem
	profiles map[string]associationConfigProfile
}

// AssociationProfile contains an explicitly selected profile. Contexts is set
// for requestors, Rules for acceptors. Roles always describe the requestor,
// including when configuring an acceptor. Extended bytes are service-specific:
// resolving them does not implement or substantiate the advertised capability.
type AssociationProfile struct {
	Contexts            []PresentationContext
	Rules               []PresentationContextRule
	RoleSelections      []RoleSelectionItem
	ExtendedNegotiation []SopClassExtendedNegotiationItem
}

// ExtendedNegotiationHandler returns a frozen, standard-conforming static
// response policy. It responds only to requested configured SOPs; Accept's
// existing accepted-context filter applies. The caller must substantiate the
// service-specific meaning of the configured response bytes (PS3.7 D.3.3.5).
func (p AssociationProfile) ExtendedNegotiationHandler() ExtendedNegotiationHandler {
	items := make(map[string][]byte, len(p.ExtendedNegotiation))
	for _, item := range p.ExtendedNegotiation {
		data := make([]byte, len(item.Data))
		copy(data, item.Data)
		items[item.SopClassUID] = data
	}
	return func(request SopClassExtendedNegotiationItem) (ExtendedNegotiationResult, error) {
		data, found := items[request.SopClassUID]
		if !found {
			return ExtendedNegotiationResult{Action: ExtendedNegotiationActionIgnore}, nil
		}
		response := make([]byte, len(data))
		copy(response, data)
		return ExtendedNegotiationResult{Action: ExtendedNegotiationActionAccept, Data: response}, nil
	}
}

func normalizedAssociationConfigOptions(options AssociationConfigOptions) (AssociationConfigOptions, error) {
	defaults := AssociationConfigLimits{MaxBytes: 1 << 20, MaxLineBytes: 8192, MaxValueBytes: 8192, MaxNameBytes: 128,
		MaxSections: 1024, MaxEntries: 16384, MaxProfiles: 256, MaxContextRules: 1024, MaxTransferSyntaxes: 256,
		MaxRoleItems: 1024, MaxExtendedItems: 1024, MaxExtendedItemBytes: 1024, MaxExtendedBytes: 64 << 10, MaxCompiledBytes: 8 << 20}
	if options.Limits.MaxBytes < 0 || options.Limits.MaxBytes == math.MaxInt64 {
		return options, ErrAssociationConfigLimit
	}
	if options.Limits.MaxBytes == 0 {
		options.Limits.MaxBytes = defaults.MaxBytes
	}
	pairs := [][2]*int{{&options.Limits.MaxLineBytes, &defaults.MaxLineBytes}, {&options.Limits.MaxValueBytes, &defaults.MaxValueBytes}, {&options.Limits.MaxNameBytes, &defaults.MaxNameBytes},
		{&options.Limits.MaxSections, &defaults.MaxSections}, {&options.Limits.MaxEntries, &defaults.MaxEntries}, {&options.Limits.MaxProfiles, &defaults.MaxProfiles},
		{&options.Limits.MaxContextRules, &defaults.MaxContextRules}, {&options.Limits.MaxTransferSyntaxes, &defaults.MaxTransferSyntaxes}, {&options.Limits.MaxRoleItems, &defaults.MaxRoleItems},
		{&options.Limits.MaxExtendedItems, &defaults.MaxExtendedItems}, {&options.Limits.MaxExtendedItemBytes, &defaults.MaxExtendedItemBytes}, {&options.Limits.MaxExtendedBytes, &defaults.MaxExtendedBytes}, {&options.Limits.MaxCompiledBytes, &defaults.MaxCompiledBytes}}
	for _, pair := range pairs {
		if *pair[0] < 0 || *pair[0] > math.MaxInt-2 {
			return options, ErrAssociationConfigLimit
		}
		if *pair[0] == 0 {
			*pair[0] = *pair[1]
		}
	}
	if options.Limits.MaxExtendedItemBytes > math.MaxUint16-66 {
		return options, ErrAssociationConfigLimit
	}
	if options.LocalByteOrder != nil && options.LocalByteOrder != binary.LittleEndian && options.LocalByteOrder != binary.BigEndian {
		return options, ErrAssociationConfig
	}
	return options, nil
}

type associationConfigReader struct {
	ctx context.Context
	r   io.Reader
}

func (r associationConfigReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func associationConfigLines(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		if c == '\n' {
			return i + 1, data[:i], nil
		}
		if c == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance := i + 1
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
			return advance, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func associationConfigName(value string, limit int) (string, error) {
	if len(value) > limit {
		return "", ErrAssociationConfigLimit
	}
	var out strings.Builder
	for _, c := range []byte(value) {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return "", ErrAssociationConfig
		}
		out.WriteByte(c)
	}
	if out.Len() == 0 {
		return "", ErrAssociationConfig
	}
	return out.String(), nil
}

// ParseAssociationConfig reads the documented DCMTK two-level cfg format.
// Duplicate keys, numbering gaps, unknown keys and controls are diagnosed rather
// than silently replaced/ignored. Reader ownership remains with the caller;
// cancellation cannot interrupt a caller Reader that blocks without context.
func ParseAssociationConfig(ctx context.Context, reader io.Reader, options AssociationConfigOptions) (*AssociationConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, ErrAssociationConfig
	}
	options, err := normalizedAssociationConfigOptions(options)
	if err != nil {
		return nil, err
	}
	l := options.Limits
	limited := &io.LimitedReader{R: associationConfigReader{ctx, reader}, N: l.MaxBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, min(1024, l.MaxLineBytes+2)), l.MaxLineBytes+2)
	scanner.Split(associationConfigLines)
	groups := make(map[string]map[string]*associationConfigSection)
	known := map[string]bool{"TRANSFERSYNTAXES": true, "PRESENTATIONCONTEXTS": true, "PROFILES": true, "SCPSCUROLESELECTION": true, "EXTENDEDNEGOTIATION": true}
	var group string
	var section *associationConfigSection
	var previousKey string
	sectionCount, entryCount, lineNumber := 0, 0, 0
	for scanner.Scan() {
		lineNumber++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if limited.N == 0 {
			return nil, ErrAssociationConfigLimit
		}
		line := scanner.Text()
		if len(line) > l.MaxLineBytes {
			return nil, ErrAssociationConfigLimit
		}
		for _, c := range []byte(line) {
			if c == 0 || c < 32 && c != '\t' || c == 127 || c >= 128 {
				return nil, fmt.Errorf("%w: line %d", ErrAssociationConfig, lineNumber)
			}
		}
		if line == "" || line[0] == '#' || strings.TrimSpace(line) == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed[0] == '[' {
			previousKey = ""
			if strings.HasPrefix(trimmed, "[[") {
				if !strings.HasSuffix(trimmed, "]]") {
					return nil, ErrAssociationConfig
				}
				group, err = associationConfigName(trimmed[2:len(trimmed)-2], l.MaxNameBytes)
				if err != nil {
					return nil, err
				}
				if !known[group] || groups[group] != nil {
					return nil, ErrAssociationConfig
				}
				groups[group] = make(map[string]*associationConfigSection)
				section = nil
			} else {
				if !strings.HasSuffix(trimmed, "]") || groups[group] == nil {
					return nil, ErrAssociationConfig
				}
				name, nameErr := associationConfigName(trimmed[1:len(trimmed)-1], l.MaxNameBytes)
				if nameErr != nil {
					return nil, nameErr
				}
				if groups[group][name] != nil {
					return nil, ErrAssociationConfig
				}
				if sectionCount >= l.MaxSections || group == "PROFILES" && len(groups[group]) >= l.MaxProfiles {
					return nil, ErrAssociationConfigLimit
				}
				sectionCount++
				section = &associationConfigSection{entries: make(map[string]string)}
				groups[group][name] = section
			}
			continue
		}
		if section == nil {
			return nil, ErrAssociationConfig
		}
		if (line[0] == ' ' || line[0] == '\t') && previousKey != "" {
			value := section.entries[previousKey]
			if len(value) >= l.MaxValueBytes || len(trimmed) > l.MaxValueBytes-len(value)-1 {
				return nil, ErrAssociationConfigLimit
			}
			section.entries[previousKey] = value + "\n" + trimmed
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, ErrAssociationConfig
		}
		key, err = associationConfigName(key, l.MaxNameBytes)
		if err != nil {
			return nil, err
		}
		value = strings.TrimSpace(value)
		if len(value) > l.MaxValueBytes {
			return nil, ErrAssociationConfigLimit
		}
		if _, found := section.entries[key]; found {
			return nil, ErrAssociationConfig
		}
		if entryCount >= l.MaxEntries {
			return nil, ErrAssociationConfigLimit
		}
		entryCount++
		section.entries[key] = value
		section.order = append(section.order, key)
		previousKey = key
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, ErrAssociationConfigLimit
		}
		return nil, associationConfigIOFailure(err)
	}
	if limited.N == 0 {
		return nil, ErrAssociationConfigLimit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return compileAssociationConfig(ctx, groups, options)
}

// ReadAssociationConfigFile closes the handle opened here. Error strings do not
// reveal the configured path; errors.Is still exposes OS/context sentinels.
func ReadAssociationConfigFile(ctx context.Context, path string, options AssociationConfigOptions) (*AssociationConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, associationConfigIOFailure(err)
	}
	defer f.Close()
	return ParseAssociationConfig(ctx, f, options)
}

func associationConfigNumbered(section *associationConfigSection, prefix string, maximum int) ([]string, error) {
	if len(section.entries) > maximum {
		return nil, ErrAssociationConfigLimit
	}
	out := make([]string, len(section.entries))
	for i := range out {
		value, found := section.entries[prefix+strconv.Itoa(i+1)]
		if !found {
			return nil, ErrAssociationConfig
		}
		out[i] = value
	}
	return out, nil
}

func associationConfigPair(value string) (string, string, error) {
	a, b, found := strings.Cut(value, "\\")
	if !found || strings.Contains(b, "\\") || strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return "", "", ErrAssociationConfig
	}
	return strings.TrimSpace(a), strings.TrimSpace(b), nil
}

func compileAssociationConfig(ctx context.Context, groups map[string]map[string]*associationConfigSection, options AssociationConfigOptions) (*AssociationConfig, error) {
	for _, required := range []string{"TRANSFERSYNTAXES", "PRESENTATIONCONTEXTS", "PROFILES"} {
		if len(groups[required]) == 0 {
			return nil, ErrAssociationConfig
		}
	}
	l := options.Limits
	c := &AssociationConfig{limits: l, syntaxes: make(map[string][]string), contexts: make(map[string][]associationConfigContext), roles: make(map[string][]RoleSelectionItem), extended: make(map[string][]SopClassExtendedNegotiationItem), profiles: make(map[string]associationConfigProfile)}
	for name, section := range groups["TRANSFERSYNTAXES"] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values, err := associationConfigNumbered(section, "TRANSFERSYNTAX", l.MaxTransferSyntaxes)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			return nil, ErrAssociationConfig
		}
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			uid, err := associationConfigUID(value, options.LocalByteOrder, true)
			if err != nil {
				return nil, err
			}
			if seen[uid] {
				return nil, ErrAssociationConfig
			}
			seen[uid] = true
			c.syntaxes[name] = append(c.syntaxes[name], uid)
		}
	}
	for name, section := range groups["PRESENTATIONCONTEXTS"] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values, err := associationConfigNumbered(section, "PRESENTATIONCONTEXT", l.MaxContextRules)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			return nil, ErrAssociationConfig
		}
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			a, b, err := associationConfigPair(value)
			if err != nil {
				return nil, err
			}
			uid, err := associationConfigUID(a, options.LocalByteOrder, false)
			if err != nil {
				return nil, err
			}
			label, err := associationConfigName(b, l.MaxNameBytes)
			if err != nil {
				return nil, err
			}
			if c.syntaxes[label] == nil {
				return nil, ErrAssociationConfig
			}
			c.contexts[name] = append(c.contexts[name], associationConfigContext{uid, label})
		}
	}
	for name, section := range groups["SCPSCUROLESELECTION"] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values, err := associationConfigNumbered(section, "ROLE", l.MaxRoleItems)
		if err != nil {
			return nil, err
		}
		items := make([]RoleSelectionItem, 0, len(values))
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			a, b, err := associationConfigPair(value)
			if err != nil {
				return nil, err
			}
			uid, err := associationConfigUID(a, options.LocalByteOrder, false)
			if err != nil {
				return nil, err
			}
			if seen[uid] {
				return nil, ErrAssociationConfig
			}
			seen[uid] = true
			role := strings.ToUpper(b)
			if role != "SCU" && role != "SCP" && role != "BOTH" {
				return nil, ErrAssociationConfig
			}
			items = append(items, RoleSelectionItem{SopClassUID: uid, SCURole: role == "SCU" || role == "BOTH", SCPRole: role == "SCP" || role == "BOTH"})
		}
		c.roles[name] = items
	}
	totalExtended := 0
	for name, section := range groups["EXTENDEDNEGOTIATION"] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		values, err := associationConfigNumbered(section, "EXTENDEDNEGOTIATION", l.MaxExtendedItems)
		if err != nil {
			return nil, err
		}
		items := make([]SopClassExtendedNegotiationItem, 0, len(values))
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			a, b, err := associationConfigPair(value)
			if err != nil {
				return nil, err
			}
			uid, err := associationConfigUID(a, options.LocalByteOrder, false)
			if err != nil {
				return nil, err
			}
			if seen[uid] || len(b)%2 != 0 {
				return nil, ErrAssociationConfig
			}
			seen[uid] = true
			n := len(b) / 2
			if n > l.MaxExtendedItemBytes || n > l.MaxExtendedBytes-totalExtended {
				return nil, ErrAssociationConfigLimit
			}
			data, err := hex.DecodeString(b)
			if err != nil {
				return nil, ErrAssociationConfig
			}
			totalExtended += n
			items = append(items, SopClassExtendedNegotiationItem{SopClassUID: uid, Data: data})
		}
		c.extended[name] = items
	}
	for name, section := range groups["PROFILES"] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for key := range section.entries {
			if key != "PRESENTATIONCONTEXTS" && key != "SCPSCUROLESELECTION" && key != "EXTENDEDNEGOTIATION" {
				return nil, ErrAssociationConfig
			}
		}
		profile := associationConfigProfile{}
		var err error
		profile.contexts, err = associationConfigName(section.entries["PRESENTATIONCONTEXTS"], l.MaxNameBytes)
		if err != nil {
			return nil, err
		}
		contexts := c.contexts[profile.contexts]
		if contexts == nil {
			return nil, ErrAssociationConfig
		}
		for _, field := range []struct {
			key string
			dst *string
		}{{"SCPSCUROLESELECTION", &profile.roles}, {"EXTENDEDNEGOTIATION", &profile.extended}} {
			value, found := section.entries[field.key]
			if found {
				*field.dst, err = associationConfigName(value, l.MaxNameBytes)
				if err != nil {
					return nil, err
				}
			}
		}
		if profile.roles != "" {
			if _, ok := c.roles[profile.roles]; !ok {
				return nil, ErrAssociationConfig
			}
		}
		if profile.extended != "" {
			if _, ok := c.extended[profile.extended]; !ok {
				return nil, ErrAssociationConfig
			}
		}
		present := make(map[string]bool, len(contexts))
		for _, pc := range contexts {
			present[pc.sop] = true
		}
		for _, role := range c.roles[profile.roles] {
			if !present[role.SopClassUID] {
				return nil, ErrAssociationConfig
			}
		}
		for _, item := range c.extended[profile.extended] {
			if !present[item.SopClassUID] {
				return nil, ErrAssociationConfig
			}
		}
		c.profiles[name] = profile
	}
	return c, nil
}

// Profile compiles a detached proposal or acceptor rule catalog. All size checks
// precede output allocation. Acceptor catalogs can exceed the wire's 128 context
// limit; a requestor can repeat SOPs but an acceptor catalog cannot.
func (c *AssociationConfig) Profile(name string, mode AssociationConfigMode) (AssociationProfile, error) {
	if c == nil || mode != AssociationConfigModeRequestor && mode != AssociationConfigModeAcceptor {
		return AssociationProfile{}, ErrAssociationConfig
	}
	name, err := associationConfigName(name, c.limits.MaxNameBytes)
	if err != nil {
		return AssociationProfile{}, err
	}
	definition, found := c.profiles[name]
	if !found {
		return AssociationProfile{}, ErrAssociationConfig
	}
	contexts := c.contexts[definition.contexts]
	if mode == AssociationConfigModeRequestor && len(contexts) > MaxPresentationContexts {
		return AssociationProfile{}, ErrAssociationConfigLimit
	}
	remaining := c.limits.MaxCompiledBytes
	consume := func(n int) error {
		if n < 0 || n > remaining {
			return ErrAssociationConfigLimit
		}
		remaining -= n
		return nil
	}
	for _, pc := range contexts {
		if err := consume(128 + len(pc.sop)); err != nil {
			return AssociationProfile{}, err
		}
		wire := 8 + len(pc.sop)
		for _, ts := range c.syntaxes[pc.syntaxList] {
			if err := consume(16 + len(ts)); err != nil {
				return AssociationProfile{}, err
			}
			wire += 4 + len(ts)
		}
		if mode == AssociationConfigModeRequestor && wire > math.MaxUint16 {
			return AssociationProfile{}, ErrAssociationConfigLimit
		}
	}
	roles := c.roles[definition.roles]
	extended := c.extended[definition.extended]
	for _, role := range roles {
		if err := consume(64 + len(role.SopClassUID)); err != nil {
			return AssociationProfile{}, err
		}
	}
	for _, item := range extended {
		if err := consume(64 + len(item.SopClassUID) + len(item.Data)); err != nil {
			return AssociationProfile{}, err
		}
	}
	// Auxiliary maps have explicit logical admission charges. Their runtime
	// buckets/allocator overhead are not an RSS bound. Complete preflight
	// before allocating either maps or output with exact slice capacities.
	var roleBySOP map[string]RoleSelectionItem
	if mode == AssociationConfigModeAcceptor {
		if err := consume(64); err != nil {
			return AssociationProfile{}, err
		}
		for range contexts {
			if err := consume(64); err != nil {
				return AssociationProfile{}, err
			}
		}
		if len(roles) > 0 {
			if err := consume(64); err != nil {
				return AssociationProfile{}, err
			}
			for range roles {
				if err := consume(64); err != nil {
					return AssociationProfile{}, err
				}
			}
		}
		seen := make(map[string]bool, len(contexts))
		for _, pc := range contexts {
			if seen[pc.sop] {
				return AssociationProfile{}, ErrAssociationConfig
			}
			seen[pc.sop] = true
		}
		if len(roles) > 0 {
			roleBySOP = make(map[string]RoleSelectionItem, len(roles))
			for _, role := range roles {
				roleBySOP[role.SopClassUID] = role
			}
		}
	}
	out := AssociationProfile{RoleSelections: make([]RoleSelectionItem, len(roles)), ExtendedNegotiation: make([]SopClassExtendedNegotiationItem, len(extended))}
	copy(out.RoleSelections, roles)
	for i, item := range extended {
		data := make([]byte, len(item.Data))
		copy(data, item.Data)
		out.ExtendedNegotiation[i] = SopClassExtendedNegotiationItem{SopClassUID: item.SopClassUID, Data: data}
	}
	if mode == AssociationConfigModeRequestor {
		out.Contexts = make([]PresentationContext, len(contexts))
	} else {
		out.Rules = make([]PresentationContextRule, len(contexts))
	}
	for i, pc := range contexts {
		sourceSyntaxes := c.syntaxes[pc.syntaxList]
		syntaxes := make([]string, len(sourceSyntaxes))
		copy(syntaxes, sourceSyntaxes)
		if mode == AssociationConfigModeRequestor {
			out.Contexts[i] = PresentationContext{ID: byte(2*i + 1), AbstractSyntaxUID: pc.sop, TransferSyntaxUIDs: syntaxes}
		} else {
			rule := PresentationContextRule{AbstractSyntaxUID: pc.sop, TransferSyntaxes: syntaxes}
			if role, ok := roleBySOP[pc.sop]; ok {
				rule.Roles = &RequestorRoles{SCU: role.SCURole, SCP: role.SCPRole}
			}
			out.Rules[i] = rule
		}
	}
	return out, nil
}
