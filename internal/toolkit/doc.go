// Package toolkit holds the shared plumbing of the built-in tool packages
// (builtin/clarify, builtin/skills, builtin/workspace and the packages of the later
// plans of Phase 11): the decoding of a tool call's arguments with JS semantics, the
// builders of the tool outputs, the lookup of the agent's sandbox (SandboxFor) and the
// byte formatting of the workspace tools (FormatBytes).
//
// The package imports no host package: like every built-in tool package it is
// host-access guarded (tests/arch/hostaccess_test.go hostAccessGuarded), so a tool reaches files,
// processes and the network only through the contracts it is given.
package toolkit
