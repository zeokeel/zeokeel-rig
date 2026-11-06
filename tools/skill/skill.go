// Package skill provides the `skill` tool and a filesystem-backed
// v1.SkillStore implementation.
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
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"

	v1 "github.com/zeokeel/zeokeel-rig/contracts/v1"
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
// v1.SkillStore falls back to a filesystem store over DefaultDirs.
func Tools(store v1.SkillStore) []v1.Tool {
	if isNil(store) {
		store = NewDefaultFileStore(currentRoot())
	}
	return []v1.Tool{&skillTool{store: store}}
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

// skillTool is the v1.Tool implementation of the skill tool.
type skillTool struct {
	store v1.SkillStore
}

// Name implements v1.Tool.
func (t *skillTool) Name() string { return ToolName }

// Description implements v1.Tool.
func (t *skillTool) Description() string {
	return "List, search or load agent skills (Markdown documents with reusable instructions). " +
		"action=list returns names and descriptions; action=search matches a query against name, description " +
		"and body; action=load returns the full skill content. Use it before a specialized task to follow " +
		"the skill's workflow."
}

// Schema implements v1.Tool.
func (t *skillTool) Schema() map[string]any {
	return objectSchema(map[string]any{
		"action": map[string]any{
			"type":        "string",
			"description": "list all skills, search for matching skills, or load one skill by name.",
			"enum":        []string{ActionList, ActionSearch, ActionLoad},
		},
		"query": stringProp("Search query for action=search (matched case-insensitively against name, description and body)."),
		"name":  stringProp("Skill name for action=load (as returned by action=list)."),
	}, "action")
}

type skillArgs struct {
	Action string `json:"action"`
	Query  string `json:"query"`
	Name   string `json:"name"`
}

// Invoke implements v1.Tool.
func (t *skillTool) Invoke(ctx context.Context, call v1.ToolCall) (v1.ToolResult, error) {
	var args skillArgs
	if err := decodeArgs(call, &args); err != nil {
		return v1.ToolResult{}, err
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	switch action {
	case ActionList, ActionSearch, ActionLoad:
	default:
		return errorResult("skill: unknown action %q (want list, search or load)", args.Action), nil
	}
	if err := ctx.Err(); err != nil {
		return v1.ToolResult{}, err
	}
	switch action {
	case ActionList:
		skills, err := t.store.List(ctx)
		if err != nil {
			return errorResult("skill: %v", err), nil
		}
		if len(skills) == 0 {
			return textResult("no skills found"), nil
		}
		return textResult(truncateOutput(formatSkillList(skills))), nil
	case ActionSearch:
		query := strings.TrimSpace(args.Query)
		if query == "" {
			return errorResult("skill: query is required for action=search"), nil
		}
		skills, err := t.store.List(ctx)
		if err != nil {
			return errorResult("skill: %v", err), nil
		}
		matches := searchSkills(skills, query)
		if len(matches) == 0 {
			return textResult(fmt.Sprintf("no skills found matching %q", query)), nil
		}
		return textResult(truncateOutput(formatSkillList(matches))), nil
	default: // ActionLoad
		name := strings.TrimSpace(args.Name)
		if name == "" {
			return errorResult("skill: name is required for action=load"), nil
		}
		loaded, err := t.store.Read(ctx, name)
		if err != nil {
			return errorResult("skill: %v", err), nil
		}
		return textResult(truncateOutput(formatSkillContent(loaded))), nil
	}
}

// formatSkillList renders skills as "- name: description" lines.
func formatSkillList(skills []v1.Skill) string {
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
func searchSkills(skills []v1.Skill, query string) []v1.Skill {
	terms := strings.Fields(strings.ToLower(query))
	var matches []v1.Skill
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
func formatSkillContent(skill v1.Skill) string {
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

// truncateOutput caps one response without splitting a UTF-8 rune.
func truncateOutput(text string) string {
	if len(text) <= MaxOutputBytes {
		return text
	}
	cut := MaxOutputBytes
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…\n[truncated: output exceeded " + fmt.Sprintf("%d", MaxOutputBytes) + " bytes]\n"
}

// --- shared helpers --------------------------------------------------------

func decodeArgs(call v1.ToolCall, target any) error {
	raw := strings.TrimSpace(call.Arguments)
	if raw == "" {
		raw = "{}"
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return nil
}

func textResult(content string) v1.ToolResult { return v1.ToolResult{Content: content} }

func errorResult(format string, args ...any) v1.ToolResult {
	return v1.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

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

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}
