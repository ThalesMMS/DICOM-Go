package audit

import "fmt"

// SuppressionRule matches all nonempty fields against an audit event. Empty
// fields are wildcards. Rules contain vocabulary codes, never patient values.
type SuppressionRule struct {
	EventID string `json:"eventID,omitempty"`
	Action  string `json:"action,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

const MaxSuppressionRules = 64

// ValidateSuppression rejects unbounded or empty catch-all rules.
func ValidateSuppression(rules []SuppressionRule) error {
	if len(rules) > MaxSuppressionRules {
		return fmt.Errorf("%w: suppression limit", ErrSyslogConfig)
	}
	for _, r := range rules {
		if r.EventID == "" && r.Action == "" && r.Outcome == "" {
			return fmt.Errorf("%w: empty suppression rule", ErrSyslogConfig)
		}
		if len(r.EventID) > 32 || len(r.Action) > 1 || len(r.Outcome) > 8 {
			return fmt.Errorf("%w: suppression field limit", ErrSyslogConfig)
		}
		if r.Action != "" && r.Action != "C" && r.Action != "R" && r.Action != "U" && r.Action != "D" && r.Action != "E" {
			return fmt.Errorf("%w: suppression action", ErrSyslogConfig)
		}
		if r.Outcome != "" && r.Outcome != "0" && r.Outcome != "4" && r.Outcome != "8" && r.Outcome != "12" {
			return fmt.Errorf("%w: suppression outcome", ErrSyslogConfig)
		}
	}
	return nil
}

func suppressed(rules []SuppressionRule, m Message) bool {
	for _, r := range rules {
		if (r.EventID == "" || r.EventID == m.Event.ID.Value) && (r.Action == "" || r.Action == m.Event.ActionCode) && (r.Outcome == "" || r.Outcome == m.Event.OutcomeIndicator) {
			return true
		}
	}
	return false
}
