package preview

import "testing"

// A-160: every preview, app and service tool works in the session's workspace (the
// runner folder the user attached), so the page it shows and the links it makes are
// the same folder the file tools wrote to - as in core/ts.
func TestPreviewToolsUseTheSessionWorkspace(t *testing.T) {
	// Update (A-210): two tools, preview and service.
	tools := NewTools(Options{})
	if len(tools) != 2 {
		t.Fatalf("%d tools, want 2", len(tools))
	}
	for _, tool := range tools {
		if def := tool.Definition(); !def.SessionWorkspace {
			t.Errorf("%s: SessionWorkspace = false", def.Name)
		}
	}
}
