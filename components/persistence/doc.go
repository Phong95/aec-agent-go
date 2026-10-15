// Package persistence holds the store contracts of an agent runtime and their
// race-safe in-memory defaults.
//
// Every store method takes a context first and returns an error. An unknown or
// absent result is a nil pointer (or the zero value) with a nil error, never an
// error.
//
// The in-memory defaults deep-copy on the way in and on the way out, so a caller
// and a store never share a mutable object. They keep insertion order wherever
// it shows through in a list or query result, and they never block, so they
// ignore the context: honouring ctx.Err() is the job of I/O-backed stores such
// as the Postgres stores of server/go. Postgres stores also order and limit
// differently from the in-memory ones, so callers rely on list order only where
// the contract defines it.
//
// Layering: the packages persistence imports (kernel, ordered and later profile,
// memory, artifact, skill and llm) must never import persistence. Hubs that need
// stores live in their own packages above it.
package persistence
