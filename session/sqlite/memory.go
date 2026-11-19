package sqlite

// Long-term notes, in the same database as the conversations that produced them:
// a page is a row, the index is an ordered read of four columns, and a search is
// a LIKE over title, summary and body.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/memory"
)

// Memory returns the wiki backend backed by this database.
func (s *Store) Memory() memory.Wiki { return memoryStore{store: s} }

// memoryStore implements memory.Wiki over rig_memory_pages.
type memoryStore struct{ store *Store }

var _ memory.Wiki = memoryStore{}

// Index lists every page's metadata, most recently updated first.
func (m memoryStore) Index(ctx context.Context) ([]memory.PageMeta, error) {
	if err := m.store.usable(); err != nil {
		return nil, err
	}
	return m.metas(ctx,
		`SELECT name, title, summary, updated_at, bytes FROM rig_memory_pages
		 ORDER BY updated_at DESC, name ASC LIMIT ?`, memory.MaxIndexResults)
}

// Read returns one page, body included.
func (m memoryStore) Read(ctx context.Context, name string) (memory.Page, error) {
	if err := m.store.usable(); err != nil {
		return memory.Page{}, err
	}
	var (
		page      memory.Page
		updatedAt int64
	)
	err := m.store.db.QueryRowContext(ctx,
		`SELECT name, title, summary, content, updated_at FROM rig_memory_pages WHERE name = ?`, name).
		Scan(&page.Name, &page.Title, &page.Summary, &page.Content, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Page{}, memory.ErrNotFound
	}
	if err != nil {
		return memory.Page{}, fmt.Errorf("sqlite: read memory %q: %w", name, err)
	}
	page.Updated = time.UnixMilli(updatedAt).UTC()
	return page, nil
}

// Write stores a page, replacing one that already existed. Replacing keeps the
// old body's size in the Log section, as the file store does: a reader can tell
// the page was rewritten rather than grown.
func (m memoryStore) Write(ctx context.Context, page memory.Page) error {
	if err := m.store.usable(); err != nil {
		return err
	}
	if err := memory.ValidateName(page.Name); err != nil {
		return err
	}
	summary, err := memory.NormalizeSummary(page.Summary)
	if err != nil {
		return err
	}
	updated := page.Updated
	if updated.IsZero() {
		updated = time.Now().UTC()
	} else {
		updated = updated.UTC()
	}
	content := page.Content
	var previous int
	err = m.store.db.QueryRowContext(ctx,
		`SELECT bytes FROM rig_memory_pages WHERE name = ?`, page.Name).Scan(&previous)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A new page: nothing to record.
	case err != nil:
		return fmt.Errorf("sqlite: write memory %q: %w", page.Name, err)
	default:
		content = memory.RewriteLog(content, previous)
	}
	title := memory.NormalizeTitle(page.Name, page.Title)
	if _, err := m.store.db.ExecContext(ctx,
		`INSERT INTO rig_memory_pages(name, title, summary, content, updated_at, bytes)
		 VALUES(?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   title = excluded.title, summary = excluded.summary,
		   content = excluded.content, updated_at = excluded.updated_at, bytes = excluded.bytes`,
		page.Name, title, summary, content, updated.UnixMilli(), len(content)); err != nil {
		return fmt.Errorf("sqlite: write memory %q: %w", page.Name, err)
	}
	return nil
}

// Append adds a dated entry to a page's Log section, creating the page when it
// does not exist yet.
func (m memoryStore) Append(ctx context.Context, name, text string) error {
	if err := m.store.usable(); err != nil {
		return err
	}
	if err := memory.ValidateName(name); err != nil {
		return err
	}
	page, err := m.Read(ctx, name)
	if errors.Is(err, memory.ErrNotFound) {
		page = memory.Page{Name: name}
	} else if err != nil {
		return err
	}
	page.Content = memory.AppendLog(page.Content, text)
	if page.Summary == "" {
		// A page born from an append has nothing else to be named by; the first
		// line of the entry becomes its index line.
		page.Summary = firstLine(text)
	}
	page.Updated = time.Time{}
	if err := m.Write(ctx, page); err != nil {
		return fmt.Errorf("sqlite: append memory %q: %w", name, err)
	}
	return nil
}

// Delete removes a page.
func (m memoryStore) Delete(ctx context.Context, name string) error {
	if err := m.store.usable(); err != nil {
		return err
	}
	result, err := m.store.db.ExecContext(ctx, `DELETE FROM rig_memory_pages WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("sqlite: delete memory %q: %w", name, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete memory %q: %w", name, err)
	}
	if affected == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// Search matches the query in a page's title, summary or body and returns the
// metadata only, so a search does not drag bodies into the context.
func (m memoryStore) Search(ctx context.Context, query string) ([]memory.PageMeta, error) {
	if err := m.store.usable(); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(query)
	if text == "" {
		return nil, nil
	}
	like := "%" + escapeLike(text) + "%"
	return m.metas(ctx,
		`SELECT name, title, summary, updated_at, bytes FROM rig_memory_pages
		 WHERE title LIKE ? ESCAPE '\' OR summary LIKE ? ESCAPE '\' OR content LIKE ? ESCAPE '\'
		 ORDER BY updated_at DESC, name ASC LIMIT ?`, like, like, like, memory.MaxSearchResults)
}

// metas runs a metadata query.
func (m memoryStore) metas(ctx context.Context, query string, args ...any) ([]memory.PageMeta, error) {
	rows, err := m.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read memory index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var metas []memory.PageMeta
	for rows.Next() {
		var (
			meta      memory.PageMeta
			updatedAt int64
		)
		if err := rows.Scan(&meta.Name, &meta.Title, &meta.Summary, &updatedAt, &meta.Bytes); err != nil {
			return nil, fmt.Errorf("sqlite: read memory index: %w", err)
		}
		meta.Updated = time.UnixMilli(updatedAt).UTC()
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

// firstLine is the summary an append-born page gets.
func firstLine(text string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(text), "\n", 2)[0])
	if len(line) > memory.MaxSummaryBytes {
		line = line[:memory.MaxSummaryBytes]
	}
	if line == "" {
		line = "log"
	}
	return line
}
