package audit_test

import (
	"sort"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/audit"
	"github.com/zeokeel/zeokeel-rig/tools/full"
)

// TestModernToolSurface is the gate: every name in audit.Modern must be produced
// by the complete static Rig tool set (tools/full.All). When this fails, the
// missing names are either a not-yet-landed feature (see the v0.7 parallel
// tasks) or a full.All wiring gap — never silently add them to the audit.
func TestModernToolSurface(t *testing.T) {
	all := full.All(full.AllOptions{Root: t.TempDir()})
	if len(all) == 0 {
		t.Fatal("tools/full.All returned no tools")
	}
	names := make([]string, len(all))
	seen := make(map[string]bool, len(all))
	for index, tool := range all {
		name := tool.Name()
		names[index] = name
		if name == "" {
			t.Fatalf("tool %d has an empty name", index)
		}
		if seen[name] {
			t.Errorf("duplicate tool name %q in tools/full.All", name)
		}
		seen[name] = true
	}

	coverage := audit.Check(names)
	if !coverage.OK() {
		t.Fatalf("modern tool surface is incomplete: %s; missing=%v\n"+
			"land the v0.7 parallel features and wire them into tools/full.All, then rerun",
			coverage, coverage.Missing)
	}
	t.Logf("modern tool surface covered: %s (static tools=%d)", coverage, len(names))
}

// TestReferenceAccounting checks the audit tables themselves: every reference
// tool is mapped or documented, tables do not contradict each other, and the
// Modern gate only asks for tools the tables actually justify.
func TestReferenceAccounting(t *testing.T) {
	if unmapped := audit.Unmapped(); len(unmapped) > 0 {
		t.Fatalf("reference tools missing from Mappings/DocumentedGaps: %v", unmapped)
	}

	reference := make(map[string]bool, len(audit.Reference))
	for _, builtin := range audit.Reference {
		if builtin.Name == "" || builtin.Package == "" {
			t.Fatalf("incomplete Reference entry: %+v", builtin)
		}
		if reference[builtin.Name] {
			t.Fatalf("duplicate Reference tool %q", builtin.Name)
		}
		reference[builtin.Name] = true
	}

	mapped := make(map[string]bool, len(audit.Mappings))
	for _, mapping := range audit.Mappings {
		if !reference[mapping.Reference] {
			t.Fatalf("mapping references unknown tool %q", mapping.Reference)
		}
		if mapped[mapping.Reference] {
			t.Fatalf("mapping for %q repeated", mapping.Reference)
		}
		mapped[mapping.Reference] = true
		if mapping.Rig == "" {
			t.Fatalf("mapping for %q has no Rig target", mapping.Reference)
		}
		if !contains(audit.Modern, mapping.Rig) && !contains(audit.ServerBound, mapping.Rig) {
			t.Fatalf("mapping %q -> %q is neither a Modern nor a ServerBound Rig tool",
				mapping.Reference, mapping.Rig)
		}
	}

	documented := make(map[string]bool, len(audit.DocumentedGaps))
	for _, gap := range audit.DocumentedGaps {
		if !reference[gap.Reference] {
			t.Fatalf("documented gap references unknown tool %q", gap.Reference)
		}
		if mapped[gap.Reference] {
			t.Fatalf("tool %q is both mapped and documented as a gap", gap.Reference)
		}
		if documented[gap.Reference] {
			t.Fatalf("documented gap %q repeated", gap.Reference)
		}
		documented[gap.Reference] = true
		if gap.Reason == "" || gap.Alternative == "" {
			t.Fatalf("documented gap %q needs a reason and an alternative", gap.Reference)
		}
	}

	if len(mapped)+len(documented) != len(reference) {
		t.Fatalf("reference accounted for %d times, want %d", len(mapped)+len(documented), len(reference))
	}

	// Modern must be sorted, unique, and must not ask for server-bound tools.
	if !sort.StringsAreSorted(audit.Modern) {
		t.Fatalf("Modern is not sorted: %v", audit.Modern)
	}
	for index := 1; index < len(audit.Modern); index++ {
		if audit.Modern[index] == audit.Modern[index-1] {
			t.Fatalf("Modern repeats %q", audit.Modern[index])
		}
	}
	for _, name := range audit.ServerBound {
		if contains(audit.Modern, name) {
			t.Fatalf("server-bound tool %q is in Modern", name)
		}
	}
	for _, name := range audit.ProductRequired {
		if !contains(audit.Modern, name) {
			t.Fatalf("product-required tool %q is not gated by Modern", name)
		}
	}
	t.Logf("audit tables: reference=%d mapped=%d gaps=%d modern=%d server-bound=%d",
		len(audit.Reference), len(audit.Mappings), len(audit.DocumentedGaps),
		len(audit.Modern), len(audit.ServerBound))
}

// TestMissing checks the helper itself: it reports only absent names, sorted,
// and returns nil when everything is covered.
func TestMissing(t *testing.T) {
	if missing := audit.Missing(audit.Modern); len(missing) != 0 {
		t.Fatalf("Missing(all Modern) = %v, want none", missing)
	}
	available := append([]string(nil), audit.Modern...)
	first := audit.Modern[0]
	available = available[1:] // drop the first required name
	missing := audit.Missing(available)
	if len(missing) != 1 || missing[0] != first {
		t.Fatalf("Missing = %v, want [%s]", missing, first)
	}
	if !sort.StringsAreSorted(missing) {
		t.Fatalf("Missing result is not sorted: %v", missing)
	}
	empty := audit.Missing(nil)
	if len(empty) != len(audit.Modern) {
		t.Fatalf("Missing(nil) = %d names, want %d", len(empty), len(audit.Modern))
	}
}

// TestDocumentedGapsAreIntentional makes the gaps machine-checkable: they are
// exactly the reference tools the SDK cannot express, so a shrink is a signal to
// update the report and a growth needs a reason.
func TestDocumentedGapsAreIntentional(t *testing.T) {
	want := []string{
		"cordis_inspect_list",
		"cordis_inspect_query",
		"list_mcp_resource_templates",
		"load_workspace_dependencies",
		"plugin_manager",
		"plugin_market",
		"run_code",
		"structured_output",
		"subagent_fork",
	}
	got := make([]string, 0, len(audit.DocumentedGaps))
	for _, gap := range audit.DocumentedGaps {
		got = append(got, gap.Reference)
	}
	sort.Strings(got)
	if !equalStrings(want, got) {
		t.Fatalf("documented gaps = %v, want %v", got, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}
