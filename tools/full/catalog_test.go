package full_test

import (
	"sort"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/catalog"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

// TestToolCatalogCoverage is the "is anything missing?" gate: it builds every
// static tool through tools.All and compares the names with the coverage
// ledger. Adding a legacy tool without wiring it into All, or removing one by
// accident, fails here before release.
func TestToolCatalogCoverage(t *testing.T) {
	all := full.All(full.AllOptions{Root: t.TempDir()})
	if len(all) == 0 {
		t.Fatal("full.All returned no tools")
	}

	names := make([]string, 0, len(all))
	seen := make(map[string]int, len(all))
	for _, tool := range all {
		name := tool.Name()
		names = append(names, name)
		seen[name]++
		if seen[name] > 1 {
			t.Errorf("duplicate tool name %q in full.All", name)
		}
	}
	sort.Strings(names)

	// Static coverage: every catalog.Static tool must be constructed.
	if report := catalog.Check(names); !report.OK() {
		t.Fatalf("%s", report)
	}

	// Modern audit: every expected modern tool name must exist; documented
	// capability gaps are listed in catalog.DocumentedGaps.
	gaps := make(map[string]bool, len(catalog.DocumentedGaps))
	for _, name := range catalog.DocumentedGaps {
		gaps[name] = true
	}
	for _, name := range catalog.AuditModern(names) {
		if !gaps[name] {
			t.Errorf("modern tool %q is missing from full.All and is not a documented gap", name)
		}
	}

	// Legacy audit: the only legacy names allowed to be absent from the static
	// build are the server-bound ones (an MCP/LSP server must be configured).
	missing := catalog.AuditLegacy(names)
	serverBound := make(map[string]bool, len(catalog.ServerBound))
	for _, name := range catalog.ServerBound {
		serverBound[name] = true
	}
	for _, name := range missing {
		if !serverBound[name] {
			t.Errorf("legacy tool %q is missing from full.All and is not server-bound", name)
		}
	}
	if t.Failed() {
		return
	}
	t.Logf("catalog complete: %d static tools, %d legacy names, %d modern names, %d documented gaps, %d server-bound, dynamic %v",
		len(names), len(catalog.Legacy), len(catalog.Modern), len(catalog.DocumentedGaps), len(catalog.ServerBound), catalog.Dynamic)
}
