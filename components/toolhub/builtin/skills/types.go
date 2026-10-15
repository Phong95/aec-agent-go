package skills

// WorkspaceResult is the outcome of a workspace copy: Dir
// is where the files went, Err says why they did not, and the zero value means there
// is no sandbox.
type WorkspaceResult struct {
	Dir string
	Err string
}
