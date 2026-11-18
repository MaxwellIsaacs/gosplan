# RED.md: expected failures of the red-by-design tests

PROTECTED. A human reviews every change by hand. Each row
is a test that must FAIL today, and the regexp (Go RE2, matched against that
test's full `go test -v` output) is the assertion it must fail on. A test not
listed here is green and must pass. A row leaves this table (because the
test went green), or its text changes, only through a human-reviewed commit;
no agent task may edit the test itself.

The suite started with 69 red rows (the orchestrator, then the budget gate).
Every one went green and was cleared in a human-reviewed commit. There is no
row now.

| test | clause | expected failure |
|---|---|---|
