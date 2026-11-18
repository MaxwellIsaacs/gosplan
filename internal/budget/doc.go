// Package budget holds gosplan's pure budget decisions. Nothing here does I/O or reads a clock: every exported function
// is a deterministic function of its arguments, so `gosplan replay --check` can
// recompute each recorded decision. Estimates are estimates, a fitted ratio
// is not the plan's formula, and every config default is policy.
//
// Purity is enforced by TestBudgetPackageIsPure: no file here may import os,
// io, net, database/sql, math/rand, os/exec or syscall, or call time.Now,
// time.Since, time.Until, time.Sleep, time.After, time.Tick, time.NewTimer or
// time.NewTicker. Callers pass `now` and every input row in a Snapshot (or a
// BreakerInput); the time package is used for the time.Time and
// time.Duration types only.
//
// What the numbers are not:
//   - An Estimate is an estimate (p-quantile of past usage), never the cost.
//   - A Fit is a fitted ratio (utilisation per token, with its basis n, span
//     and residual), never the plan's formula.
//   - Utilisation from unifiedWindows, rateLimitType or the /usage text is
//     undocumented (Obs.Documented is false for it).
//   - Every Config default (DefaultConfig) is operator policy, not a
//     measurement.
package budget
