package team

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// Bounded activity fold and its text helpers.
//
// Every state change appends one row. The fold is bounded twice: per member
// (MaxActivityPerMember, newest kept) and overall (MaxActivity, newest kept).
// Texts are collapsed onto one line and bounded so a member answer can never
// grow the read model without limit.

// addActivityLocked appends one bounded activity row and prunes the fold. A nil
// member records a team-level row (MemberID ""). The caller holds c.mu.
func (c *Coordinator) addActivityLocked(registered *member, kind ActivityKind, text string, isError bool) {
	text = collapseText(text, c.maxTextRunes)
	if strings.TrimSpace(text) == "" {
		return
	}
	c.nextActivity++
	row := Activity{
		ID:    fmt.Sprintf("team-activity-%d", c.nextActivity),
		Kind:  kind,
		Text:  text,
		Time:  c.now(),
		Error: isError,
	}
	if registered != nil {
		row.MemberID = registered.info.ID
	}
	c.activities = append(c.activities, row)
	c.pruneActivitiesLocked()
}

// pruneActivitiesLocked enforces the per-member and total bounds, keeping the
// newest rows. The caller holds c.mu.
func (c *Coordinator) pruneActivitiesLocked() {
	if len(c.activities) == 0 {
		return
	}
	perMember := make(map[string]int, 8)
	kept := make([]Activity, 0, len(c.activities))
	for index := len(c.activities) - 1; index >= 0; index-- {
		row := c.activities[index]
		if row.MemberID != "" && perMember[row.MemberID] >= c.maxActivityPerMember {
			continue
		}
		if row.MemberID != "" {
			perMember[row.MemberID]++
		}
		kept = append(kept, row)
	}
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	if len(kept) > c.maxActivity {
		kept = kept[len(kept)-c.maxActivity:]
	}
	c.activities = kept
}

// activityForMemberLocked returns the member's bounded activity rows, oldest
// first. The caller holds c.mu.
func (c *Coordinator) activityForMemberLocked(memberID string) []Activity {
	if memberID == "" {
		return nil
	}
	rows := make([]Activity, 0, c.maxActivityPerMember)
	for _, row := range c.activities {
		if row.MemberID != memberID {
			continue
		}
		rows = append(rows, row)
	}
	if len(rows) > c.maxActivityPerMember {
		rows = rows[len(rows)-c.maxActivityPerMember:]
	}
	return rows
}

// foldMemberActivity rebuilds the team fold from per-member activity rows (a
// Store that persists only members). Rows are ordered by time, then by the
// member order of the input.
func foldMemberActivity(members []Teammate) []Activity {
	total := 0
	for _, member := range members {
		total += len(member.Activity)
	}
	if total == 0 {
		return nil
	}
	rows := make([]Activity, 0, total)
	for _, member := range members {
		rows = append(rows, member.Activity...)
	}
	sort.SliceStable(rows, func(left, right int) bool {
		if rows[left].Time.Equal(rows[right].Time) {
			return false
		}
		return rows[left].Time.Before(rows[right].Time)
	})
	return rows
}

// collapseText renders arbitrary text as one bounded line: code fences and their
// contents are dropped, the common Markdown line markers are stripped, inline
// code/emphasis markers are removed and the whitespace is collapsed.
func collapseText(text string, limit int) string {
	var kept []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		trimmed = strings.TrimLeft(trimmed, "#>-*+ \t")
		trimmed = strings.ReplaceAll(trimmed, "`", "")
		trimmed = strings.ReplaceAll(trimmed, "**", "")
		trimmed = strings.ReplaceAll(trimmed, "__", "")
		if trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return truncateText(strings.Join(strings.Fields(strings.Join(kept, " ")), " "), limit)
}

// truncateText bounds text to limit runes with a trailing ellipsis; limit <= 0
// keeps the text unchanged.
func truncateText(text string, limit int) string {
	if limit <= 0 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}
