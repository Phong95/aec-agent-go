// Package host is the host sandbox: real folders on the server machine (not
// isolated), made of a Provider and the Sandbox values it creates.
//
//	own sandbox      <root>/<agentId>             the whole /workspace
//	shared sandbox   <root>/.shared/agents/<id>   one folder per agent
//
// Files are plain files - they survive restarts and can be opened from the
// machine. Commands run as the server's user, so the default approval policy asks
// the user before every write and command in a host sandbox.
//
// It is an opt-in leaf package: the root aec package does not re-export it, and
// with sandbox/docker it is one of the only two packages allowed to touch the host
// (import os, threat T-05-08). Every path a tool supplies is confined twice: the
// logical path is checked with sandboxhub.ResolveLogicalPath, the REAL host location
// (after a manual walk through symlinks and Windows junctions) is checked against
// the real host folders of the agent's roots, and the file is then opened through
// an os.Root at the matched root, so a link that is swapped in between the check
// and the use is refused by the operating system layer as well.
//
// The view provides the file side (read, write, stat, list and search) and commands
// (exec and exec.shell, with the process-tree kill: a process group on Unix, a Job
// Object on Windows), so it advertises all five capabilities.
//
// An agent id becomes a folder name, so the host also refuses the ids that name the
// same real folder as another id: on Windows a trailing dot or space, a reserved
// device name and the other invalid characters, and on Windows and macOS an id that
// differs only in case from an existing folder that the file system confirms is the
// same folder (checkAgentFolder). A parent folder that cannot be listed fails closed.
package host
