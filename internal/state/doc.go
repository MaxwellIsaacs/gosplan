// Package state is the orchestrator's SQLite store (modernc.org/sqlite, pure
// Go). The design: one writer process, WAL,
// every write in a BEGIN IMMEDIATE transaction (Store.Immediate), every task
// status change through Transition, and an events outbox that FlushEvents
// copies into the run's M1 events.jsonl. The schema is schema.sql
// (user_version 1). DefaultDir computes the default state directory.
//
// Files: db.go, schema.sql, events_outbox.go and statedir.go belong to task
// 1.a; tasks.go (2.a), attempts.go (2.b) and windows.go (3.c) add queries.
package state
