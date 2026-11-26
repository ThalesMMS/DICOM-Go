package audit

import (
	"context"
	"testing"
)

func TestDurableSuppressionIsTerminalButDoesNotDiscardOtherEvents(t *testing.T) {
	o := durableTestOptions(t)
	m := atnaMessages()["application start"]
	o.Suppression = []SuppressionRule{{EventID: m.Event.ID.Value, Outcome: OutcomeSuccess}}
	l := openDurableTest(t, o)
	if err := l.LogSource(context.Background(), 1, m); err != nil {
		t.Fatal(err)
	}
	if l.Stats().Pending != 0 || l.Stats().Suppressed != 1 || l.SourceCursor() != 1 {
		t.Fatal("suppression did not checkpoint source")
	}
	l.Close()
	o.Suppression = nil
	l = openDurableTest(t, o)
	if err := l.LogSource(context.Background(), 1, m); err != nil {
		t.Fatal(err)
	}
	if l.Stats().Pending != 0 {
		t.Fatal("rule change resurrected suppressed event")
	}
	m.Event.OutcomeIndicator = OutcomeMinorFailure
	if err := l.LogSource(context.Background(), 2, m); err != nil {
		t.Fatal(err)
	}
	if l.Stats().Pending != 1 {
		t.Fatal("unsuppressed event absent")
	}
	if err := ValidateSuppression([]SuppressionRule{{}}); err == nil {
		t.Fatal("empty catch-all accepted")
	}
	if err := ValidateSuppression(make([]SuppressionRule, MaxSuppressionRules+1)); err == nil {
		t.Fatal("unbounded rules accepted")
	}
}
