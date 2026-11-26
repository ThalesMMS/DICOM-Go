package ldapconfig

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	ldap "github.com/go-ldap/ldap/v3"
)

// ErrIdentity reports that the search did not resolve to exactly one user
// with the requested uid.
var ErrIdentity = errors.New("LDAP directory user identity is missing or ambiguous")

// AuthenticationOptions selects the direct groupOfNames membership search.
// Config.BaseDN names the user subtree and GroupBaseDN names the group
// subtree. MaxGroups defaults to 128 and is capped at 256. Limits supplies
// the connection, wire and materialization budgets used by Read.
type AuthenticationOptions struct {
	GroupBaseDN string
	MaxGroups   int
	Limits      Options
}

// Identity is an authenticated user and their complete direct group list.
// It carries LDAP names only; local role mapping belongs to the application.
type Identity struct {
	DN     string
	Groups []string
}

// IdentityDNKey normalizes DN attribute types, escaping, and the ordering of
// attributes within each RDN. Values keep their original case, so mappings
// never broaden by applying a case-folded match across all LDAP attributes.
func IdentityDNKey(value string) (string, error) {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) != -1 {
		return "", ErrInvalid
	}
	return dnKey(value)
}

// Authenticate resolves one literal uid, binds as that user's DN over
// verified TLS and reads the complete direct membership as that user.
// An optional Config.BindDN is used only for the initial identity lookup.
// On any LDAP error, cancellation or limit, the result is empty.
func Authenticate(ctx context.Context, c Config, username, password string, options AuthenticationOptions) (Identity, error) {
	if username == "" || password == "" || len(username) > 4096 || len(password) > 4096 ||
		!utf8.ValidString(username) || strings.IndexFunc(username, unicode.IsControl) != -1 {
		return Identity{}, ErrAuthentication
	}
	maxGroups := options.MaxGroups
	if maxGroups == 0 {
		maxGroups = 128
	}
	if maxGroups < 1 || maxGroups > 256 {
		return Identity{}, ErrLimit
	}
	if c.AllowAnonymousCleartext || options.Limits.DeviceName != "" || options.Limits.AETitle != "" {
		return Identity{}, ErrInvalid
	}
	limits, err := normalized(options.Limits)
	if err != nil {
		return Identity{}, err
	}
	if _, err := IdentityDNKey(options.GroupBaseDN); err != nil {
		return Identity{}, err
	}
	groupRoot, err := ldap.ParseDN(options.GroupBaseDN)
	if err != nil {
		return Identity{}, ErrInvalid
	}
	endpoint, userRoot, err := validated(c)
	if err != nil {
		return Identity{}, err
	}

	var identity Identity
	err = withLDAPSession(ctx, c, endpoint, limits, func(session *ldapSession) error {
		var budget identitySearchBudget
		users, err := session.searchIdentityEntries(c.BaseDN, userRoot,
			"(uid="+ldap.EscapeFilter(username)+")", []string{"uid"}, 2, limits, &budget)
		if err != nil {
			return err
		}
		if len(users) != 1 {
			return ErrIdentity
		}
		user := users[0]
		uid, err := single(user, "uid", true)
		if err != nil || uid != username {
			return ErrIdentity
		}
		userKey, err := IdentityDNKey(user.DN)
		if err != nil {
			return err
		}
		if err := session.ctx.Err(); err != nil {
			return err
		}
		if err := session.client.Bind(user.DN, password); err != nil {
			return session.normalizeError(err)
		}

		groups, err := session.searchIdentityEntries(options.GroupBaseDN, groupRoot,
			"(&(objectClass=groupOfNames)(member="+ldap.EscapeFilter(user.DN)+"))",
			[]string{"objectClass", "member"}, maxGroups, limits, &budget)
		if err != nil {
			return err
		}
		groupNames, err := verifiedGroupNames(session.ctx, groups, userKey)
		if err != nil {
			return err
		}
		identity = Identity{DN: user.DN, Groups: groupNames}
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return identity, nil
}

type identitySearchBudget struct {
	requests, entries, bytes int
}

// An unpaged response is complete only when the final LDAP SearchResultDone
// succeeds. Discard accumulated entries on any LDAP result error.
func (s *ldapSession) searchIdentityEntries(
	base string, root *ldap.DN, filter string, attributes []string, maxEntries int, o Options, budget *identitySearchBudget,
) ([]*ldap.Entry, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if budget.requests == o.MaxPages {
		return nil, ErrLimit
	}
	budget.requests++
	limit := min(maxEntries, o.MaxEntries-budget.entries)
	request := ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, limit+1,
		int((o.Timeout+time.Second-1)/time.Second), false, filter, attributes, nil)
	result, err := s.client.Search(request)
	if err != nil {
		return nil, s.normalizeError(err)
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if len(result.Referrals) != 0 {
		return nil, ErrReferral
	}
	if len(result.Controls) != 0 {
		return nil, ErrProtocol
	}
	if len(result.Entries) > limit {
		return nil, ErrLimit
	}
	for _, entry := range result.Entries {
		weight, err := admitEntry(entry, root, o)
		if err != nil {
			return nil, err
		}
		if weight > o.MaxModelBytes-budget.bytes {
			return nil, ErrLimit
		}
		budget.bytes += weight
	}
	budget.entries += len(result.Entries)
	return result.Entries, nil
}

func verifiedGroupNames(ctx context.Context, entries []*ldap.Entry, userKey string) ([]string, error) {
	names := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key, err := IdentityDNKey(entry.DN)
		if err != nil || seen[key] {
			return nil, ErrInvalid
		}
		var groupClass, memberFound bool
		for _, value := range entry.GetEqualFoldAttributeValues("objectClass") {
			groupClass = groupClass || strings.EqualFold(value, "groupOfNames")
		}
		for _, value := range entry.GetEqualFoldAttributeValues("member") {
			member, err := IdentityDNKey(value)
			if err != nil {
				return nil, err
			}
			memberFound = memberFound || member == userKey
		}
		if !groupClass || !memberFound {
			return nil, ErrInvalid
		}
		seen[key] = true
		names = append(names, entry.DN)
	}
	sort.Strings(names)
	return names, nil
}
