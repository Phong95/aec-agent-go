package docker

import (
	"strings"

	"github.com/Phong95/aec-agent-go/components/sandboxhub"
	"github.com/Phong95/aec-agent-go/kernel"
)

// namesFor is the container and volume name of a sandbox id:
// "docker:shared" gives "<prefix>-shared", "docker:own:<agent>" gives
// "<prefix>-own-<agent>" and any other "docker:<name>" gives "<prefix>-env-<name>".
// The container and the volume share the name.
//
// Every character outside [A-Za-z0-9_.-] becomes "-", per UTF-16 code unit, so a
// character above U+FFFF (two units) becomes TWO dashes.
func namesFor(namePrefix, sandboxID string) (container, volume string) {
	bare := strings.TrimPrefix(sandboxID, "docker:")
	var suffix string
	switch {
	case bare == "shared":
		suffix = "shared"
	case strings.HasPrefix(bare, "own:"):
		suffix = "own-" + bare[len("own:"):]
	default:
		suffix = "env-" + bare
	}
	name := namePrefix + "-" + sanitizeName(suffix)
	return name, name
}

// sanitizeName replaces every UTF-16 unit outside [A-Za-z0-9_.-] with "-".
func sanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		case r >= 0x10000:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// checkSandboxID refuses a sandbox id (and, for an own sandbox, its agent id) that
// namesFor would map to a name another id also maps to. namesFor keeps a fixed mapping
// byte for byte so that an existing container and volume are found again, but that
// mapping is not injective: "docker:own:a/b", "docker:own:a b" and "docker:own:a-b" all
// give "<prefix>-own-a-b", and so do "docker:env:x" and "docker:env-x". Two such own
// sandboxes would share one container and one /workspace volume, so
// Dispose(removeData) or RemoveData of one would delete the other's files, so such an
// id is refused.
//
// namesFor strips a leading "docker:" with TrimPrefix, which does nothing when the
// prefix is missing, so "shared", "own:x" and "x" would share a container and volume with
// "docker:shared", "docker:own:x" and "docker:x". The id must therefore start with
// "docker:" (the sandbox hub only ever creates "<kind>:shared").
//
// The ids that remain are mapped injectively:
//
//	docker:shared             -> <prefix>-shared
//	docker:own:<agentId>      -> <prefix>-own-<agentId>   agentId passes AssertSafeAgentID
//	docker:<name>             -> <prefix>-env-<name>      name only has [A-Za-z0-9_.-]
//
// No id that maps to a name of its own (every id with plain characters, which is every
// id the SDK and the server produce) changes its name, so existing containers are still
// found (the fingerprint and name goldens are untouched). The error is
// policy_denied and carries the sandbox id.
func checkSandboxID(cc sandboxhub.CreateContext) error {
	bare, ok := strings.CutPrefix(cc.ID, "docker:")
	if !ok {
		return unmappableID(cc.ID, "the id must start with \"docker:\"")
	}
	switch {
	case bare == "shared":
	case strings.HasPrefix(bare, "own:"):
		if err := sandboxhub.AssertSafeAgentID(bare[len("own:"):]); err != nil {
			return unmappableID(cc.ID, "the agent id in an own sandbox id must be a plain identifier")
		}
	default:
		if sanitizeName(bare) != bare {
			return unmappableID(cc.ID, "the name may only use letters, digits, \"_\", \".\" and \"-\"")
		}
	}
	if cc.Tenancy == sandboxhub.TenancyOwn {
		if err := sandboxhub.AssertSafeAgentID(cc.AgentID); err != nil {
			return unmappableID(cc.ID, "the agent id of an own sandbox must be a plain identifier")
		}
	}
	return nil
}

func unmappableID(id, reason string) error {
	return kernel.NewSandboxError(
		"Sandbox id \""+id+"\" cannot be used as a Docker container and volume name: "+reason,
		kernel.SandboxCodePolicyDenied, id)
}
