package budget

// Model routing. Routing is opt-in only: a candidate's prefer list
// is tried only when budget.routing.enabled is true; otherwise the candidate
// has exactly one model. There is no automatic downgrade without `prefer`,
// and this package never asks claude for a fallback model of its own.

// Models is the list of models Admit tries for c, in order. With
// routing on and more than one model in c.Models, it is c.Models with
// duplicates removed (first occurrence wins); otherwise it is c.Models[0]
// alone. A candidate with no model gets fallback (the caller's default
// model, possibly ""), so the list is never empty.
func Models(c Candidate, routing bool, fallback string) []string {
	if len(c.Models) == 0 {
		return []string{fallback}
	}
	if !routing || len(c.Models) == 1 {
		return []string{c.Models[0]}
	}
	seen := make(map[string]bool, len(c.Models))
	out := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// route tries decide for each model in order and returns the first decision
// that admits (Admit is what enforce mode would do, in observe mode too; an
// admit may carry rule admit, bootstrap or stale).
// When no model is admitted it returns the first model's decision, so a full
// refusal reports the first model's numbers. tried lists the
// models decided, in order.
func route(models []string, decide func(model string) Decision) (d Decision, tried []string) {
	var first Decision
	for i, m := range models {
		di := decide(m)
		tried = append(tried, m)
		if di.Admit {
			return di, tried
		}
		if i == 0 {
			first = di
		}
	}
	return first, tried
}
