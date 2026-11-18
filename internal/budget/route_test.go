package budget

import (
	"strings"
	"testing"
)

func TestBudgetModels(t *testing.T) {
	c := tCand("r", "t", ClassNormal, "sonnet", "opus", "sonnet")
	cases := []struct {
		name     string
		c        Candidate
		routing  bool
		fallback string
		want     string
	}{
		{"routing off uses the first model only", c, false, "", "sonnet"},
		{"routing on uses prefer order, deduplicated", c, true, "", "sonnet,opus"},
		{"one model is one model", tCand("r", "t", ClassNormal, "opus"), true, "", "opus"},
		{"no model takes the fallback", tCand("r", "t", ClassNormal), true, "haiku", "haiku"},
	}
	for _, tc := range cases {
		if got := strings.Join(Models(tc.c, tc.routing, tc.fallback), ","); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// routeFixture: week_sonnet is full, so sonnet is refused and opus fits.
func routeFixture(routing bool) (Snapshot, map[string]PoolView, map[string]Estimate) {
	c := tCand("r1", "t1", ClassCritical, "sonnet", "opus")
	s := tSnap(c.Key, []Candidate{c}, tRun("r1"))
	s.Config.ModelFamilies = []string{"opus", "sonnet"}
	s.Config.RoutingEnabled = routing
	v := views(tView("five_hour", 0.5), tView("week_sonnet", 0.01))
	e := map[string]Estimate{"sonnet": {Model: "sonnet"}, "opus": {Model: "opus"}}
	return s, v, e
}

func TestBudgetRoutingFirstAdmittedWins(t *testing.T) {
	s, v, e := routeFixture(true)
	d := Admit(s, v, e)
	if !d.Admit || d.Model != "opus" || d.Rule != RuleAdmit {
		t.Fatalf("admit=%v model=%q rule=%q, want opus admitted", d.Admit, d.Model, d.Rule)
	}
	if d.Estimate.Model != "opus" || len(d.Reserve) != 1 || d.Reserve[0].Pool != "five_hour" {
		t.Fatalf("estimate %q reserve %v", d.Estimate.Model, d.Reserve)
	}
	if !strings.Contains(strings.Join(d.Notes, "\n"), "routing: tried sonnet, opus; admitted opus") {
		t.Fatalf("notes %q", d.Notes)
	}
	// The first model wins when it fits.
	v["week_sonnet"] = tView("week_sonnet", 0.5)
	if d := Admit(s, v, e); d.Model != "sonnet" || !d.Admit {
		t.Fatalf("model %q admit %v, want sonnet", d.Model, d.Admit)
	}
}

func TestBudgetRoutingOffNeverRoutes(t *testing.T) {
	s, v, e := routeFixture(false)
	d := Admit(s, v, e)
	if d.Admit || d.Model != "sonnet" || d.Rule != RuleHeadroom || !strings.Contains(d.Reason, "pool week_sonnet") {
		t.Fatalf("admit=%v model=%q rule=%q reason %q, want sonnet refused on week_sonnet", d.Admit, d.Model, d.Rule, d.Reason)
	}
	for _, n := range d.Notes {
		if strings.HasPrefix(n, "routing:") {
			t.Fatalf("routing note with routing off: %q", n)
		}
	}
}

func TestBudgetRoutingFullRefusalReportsFirstModel(t *testing.T) {
	s, v, e := routeFixture(true)
	v["five_hour"] = tView("five_hour", 0.01) // now opus is refused too
	d := Admit(s, v, e)
	if d.Admit || d.Model != "sonnet" || d.Rule != RuleHeadroom {
		t.Fatalf("admit=%v model=%q rule=%q, want the first model's refusal", d.Admit, d.Model, d.Rule)
	}
	// sonnet's applicable pools are five_hour and week_sonnet; opus's only five_hour.
	if len(d.Pools) != 2 || d.Pools[0].Pool != "five_hour" || d.Pools[1].Pool != "week_sonnet" {
		t.Fatalf("pools %v, want sonnet's numbers", d.Pools)
	}
	if !strings.Contains(strings.Join(d.Notes, "\n"), "none admitted, reporting sonnet") || d.NotBefore == nil {
		t.Fatalf("notes %q notBefore %v", d.Notes, d.NotBefore)
	}
}
