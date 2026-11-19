// Package memory is the store contract for an agent's long-term notes: a
// progressive wiki where the index carries one line per page and the body is
// read only when the model asks for it.
//
// The contract lives here, in a public package, so a storage backend outside
// this module can implement it. `internal/memwiki` ships the two that need no
// database (files on disk, and in-process); `session/sqlite` ships the one that
// keeps notes next to the rest of a session's state.
package memory

import "github.com/zeokeel/zeokeel-rig/internal/memwiki"

// Page is one stored note.
type Page = memwiki.Page

// PageMeta is a note's index entry: no body.
type PageMeta = memwiki.PageMeta

// Wiki is the storage contract. Implementations must be safe for concurrent
// use, and Index/Search must return metadata only — a body is only ever read
// through Read.
type Wiki = memwiki.Wiki

// Limits a backend enforces.
const (
	// MaxSummaryBytes caps the one-line summary the index shows.
	MaxSummaryBytes = memwiki.MaxSummaryBytes
	// MaxNameLength caps a page name.
	MaxNameLength = memwiki.MaxNameLength
	// LogHeading is the section an append writes into.
	LogHeading = memwiki.LogHeading
	// MaxIndexResults caps an index listing.
	MaxIndexResults = memwiki.MaxIndexResults
	// MaxSearchResults caps a search result set.
	MaxSearchResults = memwiki.MaxSearchResults
)

// Errors a caller branches on.
var (
	// ErrInvalidName reports a name that is not a slug.
	ErrInvalidName = memwiki.ErrInvalidName
	// ErrSummaryRequired reports a page written without a summary.
	ErrSummaryRequired = memwiki.ErrSummaryRequired
	// ErrSummaryTooLong reports a summary over MaxSummaryBytes.
	ErrSummaryTooLong = memwiki.ErrSummaryTooLong
	// ErrNotFound reports a page that does not exist.
	ErrNotFound = memwiki.ErrNotFound
	// ErrEmptyContent reports an empty body.
	ErrEmptyContent = memwiki.ErrEmptyContent
)

// ValidateName rejects a name that is not a slug. It deliberately does not
// rewrite one: the model writes these names and would not find a silently
// renamed page afterwards.
func ValidateName(name string) error { return memwiki.ValidateName(name) }

// NormalizeSummary validates and normalises a summary.
func NormalizeSummary(summary string) (string, error) { return memwiki.NormalizeSummary(summary) }

// NormalizeTitle returns the title to show, falling back to the name.
func NormalizeTitle(name, title string) string { return memwiki.NormalizeTitle(name, title) }

// AppendLog appends a dated entry to a page body's Log section.
func AppendLog(content, text string) string { return memwiki.AppendLog(content, text) }

// RewriteLog appends the "rewritten" line a Write records.
func RewriteLog(content string, previousBytes int) string {
	return memwiki.RewriteLog(content, previousBytes)
}

// SortMetas orders page metadata as the contract requires.
func SortMetas(metas []PageMeta) { memwiki.SortMetas(metas) }
