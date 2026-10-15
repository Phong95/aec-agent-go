// Package clarify is the clarify tool: the built-in tool that asks the human user a
// question in the middle of a turn and waits for the answer.
//
// The tool has no I/O of its own. It reaches the user only through
// toolhub.Context.Interaction, the InteractionHandler the host sets on the tool hub (the
// same handler that answers approval requests), so the package imports no host
// package: it is host-access guarded (tests/arch/hostaccess_test.go hostAccessGuarded).
//
// The Go name drops the package prefix (clarify.Tool, not clarify.ClarifyTool), which
// keeps revive's stutter rule clean; the root package carries the name ClarifyTool as
// an alias.
package clarify
