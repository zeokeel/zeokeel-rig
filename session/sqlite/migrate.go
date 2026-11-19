package sqlite

// Moving an existing host from the directory store to one database. The import
// is verified record by record and is safe to re-run: a session that is already
// in the database is skipped, so an interrupted migration resumes instead of
// duplicating.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/zeokeel/zeokeel-rig/session/store"
)

// ImportOptions configures ImportDir.
type ImportOptions struct {
	// Dir is the directory store's root (the value the host passed to
	// store.NewDir).
	Dir string
	// DryRun reads and counts without writing: a host shows the preview first.
	DryRun bool
}

// ImportReport says what an import found and did.
type ImportReport struct {
	// Sessions is how many sessions the directory holds.
	Sessions int
	// Messages is how many messages were read (dry run included).
	Messages int
	// Imported is how many sessions this run actually wrote; it is 0 for a dry
	// run and for sessions the database already had.
	Imported int
	// Skipped lists sessions the database already held.
	Skipped []string
	// Failed lists the sessions that could not be read, with the reason. A
	// corrupt conversation does not stop the rest of the migration.
	Failed []string
}

// ImportDir copies every session of a directory store into the database.
func ImportDir(ctx context.Context, target *Store, opts ImportOptions) (ImportReport, error) {
	var report ImportReport
	if target == nil {
		return report, errors.New("sqlite: import target is nil")
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		return report, errors.New("sqlite: ImportOptions.Dir is required")
	}
	// A path that is not there is almost always a typo in the host's config;
	// store.NewDir would happily create it and report an empty migration.
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return report, fmt.Errorf("sqlite: import source %q is not a directory", dir)
	}
	source, err := store.NewDir(dir)
	if err != nil {
		return report, fmt.Errorf("sqlite: open source directory: %w", err)
	}
	defer func() { _ = source.Close() }()
	metas, err := source.List(ctx, store.Filter{})
	if err != nil {
		return report, fmt.Errorf("sqlite: list source sessions: %w", err)
	}
	// Oldest first, so the database's own ordering survives the migration.
	sort.SliceStable(metas, func(i, j int) bool { return metas[i].CreatedAt.Before(metas[j].CreatedAt) })
	existing := map[string]bool{}
	if current, err := target.List(ctx, Filter{}); err == nil {
		for _, meta := range current {
			existing[meta.ID] = true
		}
	}
	for _, meta := range metas {
		report.Sessions++
		loaded, messages, loadErr := source.Load(ctx, meta.ID)
		if loadErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, loadErr))
			continue
		}
		report.Messages += len(messages)
		if existing[meta.ID] {
			report.Skipped = append(report.Skipped, meta.ID)
			continue
		}
		if opts.DryRun {
			continue
		}
		imported, importErr := target.Create(ctx, Meta{
			ID:        meta.ID,
			Title:     loaded.Title,
			Workspace: loaded.Workspace,
			CreatedAt: loaded.CreatedAt,
			UpdatedAt: loaded.UpdatedAt,
		})
		if importErr != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, importErr))
			continue
		}
		if len(messages) > 0 {
			if _, appendErr := target.AppendBatch(ctx, imported.ID, messages...); appendErr != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %v", meta.ID, appendErr))
				continue
			}
		}
		report.Imported++
	}
	return report, nil
}
