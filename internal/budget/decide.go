package budget

import "sort"

// Decide is the one admission entry point. It is pure: composition only.
// For each pool in s.Obs (sorted names) it fits the pool's token->utilisation
// ratio and builds the pool view; for each distinct model among the
// candidates it builds the estimate; then Admit decides. Every rule lives in
// the functions it calls, none here.
func Decide(s Snapshot) Decision {
	cfg := s.Config
	pools := make([]string, 0, len(s.Obs))
	for p := range s.Obs {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	views := make(map[string]PoolView, len(pools))
	for _, pool := range pools {
		rows := s.Obs[pool]
		iv := Intervals(pool, rows, s.Spans, cfg.ModelFamilies, cfg.ResetEpsilon, s.Now, cfg.FitLookback)
		fit := FitRatio(pool, iv, cfg)
		views[pool] = ViewPool(pool, rows, floorOf(s.Floors, pool), fit, s.Now, cfg)
	}

	// maxTurns per model: the For candidate's when it lists the model, else
	// the first candidate (in Snapshot order) that lists it.
	maxTurns := map[string]int{}
	for _, c := range s.Candidates {
		if c.Key != s.For {
			continue
		}
		for _, m := range c.Models {
			if _, ok := maxTurns[m]; !ok {
				maxTurns[m] = c.MaxTurns
			}
		}
	}
	for _, c := range s.Candidates {
		for _, m := range c.Models {
			if _, ok := maxTurns[m]; !ok {
				maxTurns[m] = c.MaxTurns
			}
		}
	}
	models := make([]string, 0, len(maxTurns))
	for m := range maxTurns {
		models = append(models, m)
	}
	sort.Strings(models)
	ests := make(map[string]Estimate, len(models))
	for _, m := range models {
		ests[m] = EstimateFor(m, s.Samples[m], maxTurns[m], cfg)
	}
	return Admit(s, views, ests)
}

// floorOf returns a copy of the pool's floor observation, or nil.
func floorOf(floors map[string]Obs, pool string) *Obs {
	f, ok := floors[pool]
	if !ok {
		return nil
	}
	return &f
}
