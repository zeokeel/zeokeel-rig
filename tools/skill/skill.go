// Package skill provides the `skill` tool and a filesystem-backed
// rig.SkillStore implementation.
//
// A skill is a Markdown document (conventionally <dir>/SKILL.md) that an agent
// can list, search and load on demand. The tool is deliberately small — one
// model-facing tool with action=list|search|load — so hosts can expose a large
// skill library without spending context on it up front:
//
//	{"action":"list"}
//	{"action":"search","query":"release"}
//	{"action":"load","name":"release-checklist"}
//
// FileStore scans one or more directories for `*/SKILL.md` and (optionally)
// top-level `*.md` files. The skill name is the directory name (or the file
// name without `.md`); the description comes from a minimal `description:` line
// or YAML-ish front matter, falling back to the first `# Heading` or text line.
// Reads are confined to the configured directories: symlinks that escape a
// directory are skipped, missing directories are ignored, and duplicate names
// keep the first occurrence (directories are scanned in the order given).
//
// Tools(store) falls back to a FileStore when store is nil, using DefaultDirs:
// $ZEOKEEL_SKILLS (colon-separated), <cwd>/.zeokeel/skills and
// ~/.zeokeel/skills. The stdlib-only implementation never writes to the
// filesystem and is safe for concurrent use.
package skill

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/internal/toolutil"
)

// ToolName is the model-facing tool name.
const ToolName = "skill"

// Actions accepted by the skill tool.
const (
	ActionList   = "list"
	ActionSearch = "search"
	ActionLoad   = "load"
)

// MaxOutputBytes caps one skill tool response.
const MaxOutputBytes = 32 << 10

// Tools returns the skill tool bound to store. A nil (or typed nil)
// rig.SkillStore falls back to a filesystem store over DefaultDirs.
func Tools(store rig.SkillStore) []rig.Tool {
	if isNil(store) {
		store = NewDefaultFileStore(currentRoot())
	}
	return []rig.Tool{&skillTool{store: store}}
}

// currentRoot returns the process working directory ("" when unavailable); it
// is the <root> part of the default skill search path.
func currentRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// skillTool is the rig.Tool implementation of the skill tool.
type skillTool struct {
	store rig.SkillStore
}

// Name implements rig.Tool.
func (t *skillTool) Name() string { return ToolName }

// Description implements rig.Tool.
func (t *skillTool) Description() string {
	return "Load agent skills (Markdown documents with reusable instructions) by name. Calling with only " +
		"`name` loads that skill; action=list returns names and descriptions, action=search matches a query " +
		"against name, description and body. Use it before a specialized task to follow the skill's workflow."
}

// Schema implements rig.Tool.
func (t *skillTool) Schema() map[string]any {
	return toolutil.ObjectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"description": "list all skills, search for matching skills, or load one skill by name.",
			"enum":        []string{ActionList, ActionSearch, ActionLoad},
		},
		"query": toolutil.StringProp("Search query for action=search (matched case-insensitively against name, description and body)."),
		"name":  toolutil.StringProp("Skill name to load (DSH-compatible name-only call; as returned by action=list)."),
	})
}

type skillArgs struct {
	Action string `json:"action"`
	Query  string `json:"query"`
	Name   string `json:"name"`
}

// Invoke implements rig.Tool.
func (t *skillTool) Invoke(ctx context.Context, call rig.ToolCall) (rig.ToolResult, error) {
	var args skillArgs
	if err := toolutil.DecodeArgs(call, &args); err != nil {
		return rig.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if action == "" {
		// DSH compatibility: the model-facing schema is name-only; an omitted
		// action loads the named skill, or lists skills when no name is given.
		if strings.TrimSpace(args.Name) != "" {
			action = ActionLoad
		} else {
			action = ActionList
		}
	}
	switch action {
	case ActionList, ActionSearch, ActionLoad:
	default:
		return toolutil.ErrorResultf("skill: unknown action %q (want list, search or load)", args.Action), nil
	}
	if err := ctx.Err(); err != nil {
		return rig.ToolResult{}, err
	}
	switch action {
	case ActionList:
		skills, err := t.store.List(ctx)
		if err != nil {
			return toolutil.ErrorResultf("skill: %v", err), nil
		}
		if len(skills) == 0 {
			return toolutil.TextResult("no skills found"), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(formatSkillList(skills), MaxOutputBytes, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", MaxOutputBytes))), nil
	case ActionSearch:
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return toolutil.ErrorResultf("skill: query is required for action=search"), nil
		}
		skills, err := t.store.List(ctx)
		if err != nil {
			return toolutil.ErrorResultf("skill: %v", err), nil
		}
		matches := searchSkills(skills, query)
		if len(matches) == 0 {
			return toolutil.TextResult(fmt.Sprintf("no skills found matching %q", query)), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(formatSkillList(matches), MaxOutputBytes, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", MaxOutputBytes))), nil
	default: // ActionLoad
		name := strings.TrimSpace(args.Name)
		if name == "" {
			return toolutil.ErrorResultf("skill: name is required for action=load"), nil
		}
		loaded, err := t.store.Read(ctx, name)
		if err != nil {
			return toolutil.ErrorResultf("skill: %v", err), nil
		}
		return toolutil.TextResult(toolutil.TruncateBytes(formatSkillContent(loaded), MaxOutputBytes, fmt.Sprintf("…\n[truncated: output exceeded %d bytes]\n", MaxOutputBytes))), nil
	}
}

// formatSkillList renders skills as "- name: description" lines.
func formatSkillList(skills []rig.Skill) string {
	var builder strings.Builder
	for _, entry := range skills {
		builder.WriteString("- ")
		builder.WriteString(entry.Name)
		if description := strings.TrimSpace(entry.Description); description != "" {
			builder.WriteString(": ")
			builder.WriteString(description)
		}
		builder.WriteString("\n")
	}
	return builder.String()
}

// searchSkills matches query case-insensitively against name, description and
// body. Multi-word queries require every word to appear somewhere in the skill.
func searchSkills(skills []rig.Skill, query string) []rig.Skill {
	terms := strings.Fields(strings.ToLower(query))
	var matches []rig.Skill
	for _, entry := range skills {
		haystack := strings.ToLower(entry.Name + "\n" + entry.Description + "\n" + entry.Body)
		matchedAll := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matchedAll = false
				break
			}
		}
		if matchedAll {
			matches = append(matches, entry)
		}
	}
	return matches
}

// formatSkillContent renders one full skill for the model.
func formatSkillContent(skill rig.Skill) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "name: %s\n", skill.Name)
	if description := strings.TrimSpace(skill.Description); description != "" {
		fmt.Fprintf(&builder, "description: %s\n", description)
	}
	fmt.Fprintf(&builder, "path: %s\n", skill.Path)
	builder.WriteString("---\n")
	builder.WriteString(skill.Body)
	if !strings.HasSuffix(skill.Body, "\n") {
		builder.WriteString("\n")
	}
	return builder.String()
}

// --- shared helpers --------------------------------------------------------

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
