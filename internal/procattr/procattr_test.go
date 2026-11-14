package procattr

import "testing"

// TestHideWindowToleratesNil keeps the preparation safe for callers that may
// pass an empty command (tests and error paths).
func TestHideWindowToleratesNil(t *testing.T) {
	HideWindow(nil)
}
