package importer

// Host-agnostic discovery of DSH session logs below <dir>/sessions.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// sessionFileName is the DSH v4 session log file name.
const sessionFileName = "session.v4.jsonl"

// Source is one discovered session log.
type Source struct {
	Session      *Session
	RelativePath string
}

// Discovery is the read-only scan result. Sessions/Messages/Workspaces are the
// preview counts; Sources keeps every parsable log in walk order (duplicates
// included, for the caller's import loop).
type Discovery struct {
	Sources    []Source
	Notes      []string
	Skipped    int
	Sessions   int
	Messages   int
	Workspaces int
}

// Discover walks <dir>/sessions and parses every uncompressed session.v4.jsonl.
// Symlinks are skipped and reported; .zstd logs, parse failures and unsupported
// versions are counted as skipped notes.
func Discover(ctx context.Context, dir string) (Discovery, error) {
	var discovery Discovery
	sessionsRoot := filepath.Join(dir, "sessions")
	info, err := os.Stat(sessionsRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return discovery, nil
	}
	if err != nil {
		return discovery, fmt.Errorf("importer: sessions directory %q: %w", sessionsRoot, err)
	}
	if !info.IsDir() {
		return discovery, fmt.Errorf("importer: %q is not a directory", sessionsRoot)
	}
	workspaceDirs := make(map[string]bool)
	seenSourceIDs := make(map[string]bool)
	walkErr := filepath.WalkDir(sessionsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		relative := relativeSourcePath(dir, path)
		if entry.Type()&fs.ModeSymlink != 0 {
			discovery.Notes = append(discovery.Notes, fmt.Sprintf("skipped symlink %s (not followed)", relative))
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if strings.HasSuffix(name, ".zstd") {
			discovery.Skipped++
			discovery.Notes = append(discovery.Notes, fmt.Sprintf("skipped compressed session %s (zstd is not supported)", relative))
			return nil
		}
		if name != sessionFileName {
			return nil
		}
		parsed, err := ParseSessionFile(path)
		if err != nil {
			discovery.Skipped++
			if errors.Is(err, ErrUnsupportedVersion) {
				discovery.Notes = append(discovery.Notes, fmt.Sprintf("unsupported session version in %s: %v", relative, err))
			} else {
				discovery.Notes = append(discovery.Notes, fmt.Sprintf("skipped %s: %v", relative, err))
			}
			return nil
		}
		if parsed.ID != "" {
			if seenSourceIDs[parsed.ID] {
				discovery.Notes = append(discovery.Notes,
					fmt.Sprintf("duplicate session id %q in %s; the first log is imported", parsed.ID, relative))
				discovery.Sources = append(discovery.Sources, Source{Session: parsed, RelativePath: relative})
				return nil
			}
			seenSourceIDs[parsed.ID] = true
		}
		if parsed.DroppedAttachments > 0 {
			discovery.Skipped += parsed.DroppedAttachments
			discovery.Notes = append(discovery.Notes,
				fmt.Sprintf("%s: %d image/file attachment(s) not imported", relative, parsed.DroppedAttachments))
		}
		if parsed.Cwd != "" {
			if cwdInfo, statErr := os.Stat(parsed.Cwd); statErr == nil && cwdInfo.IsDir() {
				workspaceDirs[parsed.Cwd] = true
			}
		}
		discovery.Sessions++
		discovery.Messages += len(parsed.Messages)
		discovery.Sources = append(discovery.Sources, Source{Session: parsed, RelativePath: relative})
		return nil
	})
	if walkErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return discovery, ctxErr
		}
		return discovery, fmt.Errorf("importer: walk %q: %w", sessionsRoot, walkErr)
	}
	discovery.Workspaces = len(workspaceDirs)
	return discovery, nil
}

// relativeSourcePath renders a session path relative to the DSH home.
func relativeSourcePath(root, path string) string {
	if relative, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(relative)
	}
	return filepath.ToSlash(path)
}
