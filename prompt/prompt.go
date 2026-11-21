// Package prompt assembles a model-facing system prompt from labelled sections.
//
// Hosts that show prompt provenance (a context inspector, a debugging UI) need
// more than one concatenated string: they need the ordered pieces, where each
// piece came from, how large it is and whether it changed. This package owns
// only that mechanic — section order, the environment group, trimming,
// measurement and content hashing. The text of every section stays with the
// host, because identity, approval policy wording and tool guidance are product
// decisions.
//
// A section whose text is empty (or whitespace) is dropped: hosts can build
// optional sections optimistically instead of branching on every one.
package prompt

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

// Well-known section IDs. They are conventions, not requirements: a host may
// use any ID, and only the IDs in Options.GroupIDs change rendering.
const (
	// IDIdentity introduces the agent.
	IDIdentity = "identity"
	// IDEnvironment describes OS/arch/shell.
	IDEnvironment = "environment"
	// IDWorkingDirectory describes the tool root and its file policy.
	IDWorkingDirectory = "working-directory"
	// IDDate is the run's local date ("Today's date is …").
	IDDate = "date"
)

// Section is one labelled piece of the system prompt.
type Section struct {
	// ID is a stable identifier (also the diff key between snapshots).
	ID string
	// Source names where the text came from (builtin, policy, config, workspace,
	// preset, memory, skills, tools, …).
	Source string
	// Text is the trimmed section text.
	Text string
	// Bytes is len(Text).
	Bytes int
	// Tokens is the estimated token count of Text.
	Tokens int
	// Hash is a short stable content hash (used to diff adjacent snapshots).
	Hash string
}

// DefaultGroupIDs are the sections rendered as one single-newline block: the
// agent identity followed by its runtime facts.
var DefaultGroupIDs = []string{IDIdentity, IDEnvironment, IDWorkingDirectory, IDDate}

// Options controls assembly and rendering.
type Options struct {
	// GroupIDs lists the section IDs rendered as one newline-joined block. Empty
	// means DefaultGroupIDs.
	GroupIDs []string
}

// NewSection builds one measured section, dropping an empty one.
func NewSection(id, source, text string) Section {
	text = strings.TrimSpace(text)
	if text == "" {
		return Section{}
	}
	return Section{
		ID:     strings.TrimSpace(id),
		Source: strings.TrimSpace(source),
		Text:   text,
		Bytes:  len(text),
		Tokens: rig.EstimateTextTokens(text),
		Hash:   Hash(text),
	}
}

// Measure trims every section, drops the empty ones and fills Bytes/Tokens/Hash.
// Sections that already carry measurements keep them (a host may pre-measure).
func Measure(sections []Section) []Section {
	out := make([]Section, 0, len(sections))
	for _, section := range sections {
		text := strings.TrimSpace(section.Text)
		if text == "" {
			continue
		}
		section.Text = text
		if section.Bytes == 0 {
			section.Bytes = len(text)
		}
		if section.Tokens == 0 {
			section.Tokens = rig.EstimateTextTokens(text)
		}
		if section.Hash == "" {
			section.Hash = Hash(text)
		}
		out = append(out, section)
	}
	return out
}

// Render joins measured sections: the group IDs form one block joined by single
// newlines; every other section is separated by a blank line. Sections outside a
// group split the block, so a host can interleave prose between environment
// facts deliberately.
func Render(sections []Section, groupIDs ...string) string {
	groups := make(map[string]bool, len(groupIDs))
	for _, id := range groupIDs {
		groups[strings.TrimSpace(id)] = true
	}
	if len(groups) == 0 {
		for _, id := range DefaultGroupIDs {
			groups[id] = true
		}
	}
	parts := make([]string, 0, len(sections))
	group := make([]string, 0, 4)
	flush := func() {
		if len(group) == 0 {
			return
		}
		parts = append(parts, strings.Join(group, "\n"))
		group = group[:0]
	}
	for _, section := range sections {
		text := strings.TrimSpace(section.Text)
		if text == "" {
			continue
		}
		if groups[section.ID] {
			group = append(group, text)
			continue
		}
		flush()
		parts = append(parts, text)
	}
	flush()
	return strings.Join(parts, "\n\n")
}

// Assemble measures the sections and renders them with the default grouping.
func Assemble(sections []Section) (string, []Section) {
	measured := Measure(sections)
	return Render(measured), measured
}

// Hash returns the short stable content hash of one section (8 bytes of SHA-256,
// hex). It is stable across processes, which is what snapshot diffing needs.
func Hash(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return hex.EncodeToString(sum[:8])
}

// DateSection is the conventional "today" section for run-local dates. It
// returns an empty section for a zero time so callers can pass an optional clock
// without branching.
func DateSection(now time.Time) Section {
	if now.IsZero() {
		return Section{}
	}
	return NewSection(IDDate, "builtin", "Today's date is "+now.Format("2006-01-02")+" ("+now.Weekday().String()+").")
}
