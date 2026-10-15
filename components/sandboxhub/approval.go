package sandboxhub

// DefaultApprovalPolicy is the default approval rule per sandbox (wired into the
// approval gate by A-61):
//
//	| operation              | isolated (docker/remote) | not isolated (host) |
//	|------------------------|--------------------------|---------------------|
//	| fs.read / fs.search    | auto                     | auto                |
//	| fs.write               | auto                     | approval            |
//	| exec                   | auto                     | approval            |
//
// Only Traits.Isolated and the operation matter. On a sandbox that is not
// isolated the rule is an allow-list (fail closed): only the read-only
// operations fs.read and fs.search run without approval, and every other
// operation, fs.write, exec, "exec.shell" (a capability, not an operation), an
// empty or an unknown one, needs approval. An isolated sandbox never needs
// approval. The golden fixture sandbox-approval.json holds the full traits x
// operation matrix of the policy.
var DefaultApprovalPolicy ApprovalPolicy = func(traits Traits, op Operation) bool {
	if traits.Isolated {
		return false
	}
	return !approvalFreeOperation(op)
}

// approvalFreeOperation reports whether op only reads, so it needs no approval
// even on a sandbox that is not isolated from the host.
func approvalFreeOperation(op Operation) bool {
	switch op {
	case OperationFSRead, OperationFSSearch:
		return true
	default:
		return false
	}
}
