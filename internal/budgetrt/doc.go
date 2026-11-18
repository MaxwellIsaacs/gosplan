// Package budgetrt is the budget gate: it implements reconcile.Budget over
// internal/state and the pure decisions of internal/budget.
//
// The Gate does I/O and bookkeeping only. Every threshold, estimate, fit and
// decision comes from internal/budget; the Gate loads the inputs inside the
// caller's transaction, calls the pure functions, writes the events
// (budget_events outbox, flushed to <run>/budget.jsonl) and the rows they
// explain in the same transaction, and keeps in memory what this process
// wrote so that a worker editing those rows is caught.
//
// What the numbers are, and are not: token and USD figures are
// estimates; window utilisation from unifiedWindows, rateLimitType and the
// /usage text is undocumented; --max-budget-usd is a client-side list-price
// estimate and does not cap plan usage; every budget default is policy, not
// measurement; headroom is headroom at the horizon (the reset only when the
// decision's horizon_basis says so); admission reduces rate-limit stalls, it
// does not prevent them; and `gosplan replay --check` proves that a recorded
// decision follows from its recorded inputs, not that the inputs were true.
package budgetrt
