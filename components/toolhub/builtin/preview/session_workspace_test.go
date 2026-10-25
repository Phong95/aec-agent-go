package preview

import "testing"

// A-160: every preview, app and service tool works in the session's workspace (the
// runner folder the user attached), so the page it shows and the links it makes are
// the same folder the file tools wrote to - as in core/ts.
func TestPreviewToolsUseTheSessionWorkspace(t *testing.T) {
	tools := NewTools()
	if len(tools) != 7 {
		t.Fatalf("%d tools, want 7", len(tools))
	}
	for _, tool := range tools {
		if def := tool.Definition(); !def.SessionWorkspace {
			t.Errorf("%s: SessionWorkspace = false", def.Name)
		}
	}
}
