package importer

// Host-agnostic import of discovered DSH sessions into a target store.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig"
)

const importedSessionPrefix = "dsh-"

// Target is the durable session store the import writes through.
type Target interface {
	// SessionIDs returns the ids of every existing session.
	SessionIDs(ctx context.Context) ([]string, error)
	CreateSession(ctx context.Context, id, title string) error
	RenameSession(ctx context.Context, id, title string) error
	ReplaceHistory(ctx context.Context, id string, messages []rig.Message) error
	AppendEvent(ctx context.Context, event rig.SessionEvent) error
}

// Workspaces registers imported working directories.
type Workspaces interface {
	// Current returns the current workspace id (ok=false when none).
	Current(ctx context.Context) (id string, ok bool, err error)
	// Create registers root and makes it current.
	Create(ctx context.Context, name, root string) error
	// Open makes id current.
	Open(ctx context.Context, id string) error
}

// Result summarizes one import.
type Result struct {
	ImportedSessions   int
	ImportedMessages   int
	ImportedWorkspaces int
	Skipped            int
	Errors             []string
}

// Import writes every source into target. Sessions already imported are
// skipped; per-session failures are reported but do not abort the run. The
// current workspace is restored after registrations (Create switches it).
func Import(ctx context.Context, sources []Source, skipped int, target Target, workspaces Workspaces, logger rig.Logger) (Result, error) {
	result := Result{Skipped: skipped}
	ids, err := target.SessionIDs(ctx)
	if err != nil {
		return result, fmt.Errorf("importer: list existing sessions: %w", err)
	}
	existing := make(map[string]bool, len(ids))
	for _, id := range ids {
		existing[id] = true
	}
	registeredWorkspaces := make(map[string]bool)
	previousCurrentID := ""
	if workspaces != nil {
		if current, ok, currentErr := workspaces.Current(ctx); currentErr == nil && ok {
			previousCurrentID = current
		}
	}
	registeredAny := false
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		session := source.Session
		sessionID := importedSessionPrefix + session.ID
		if existing[sessionID] {
			result.Skipped++
			continue
		}
		title := Title(session.Messages)
		if err := target.CreateSession(ctx, sessionID, title); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: create session: %v", source.RelativePath, err))
			continue
		}
		if err := target.RenameSession(ctx, sessionID, title); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: rename session: %v", source.RelativePath, err))
		}
		messages := Messages(session.Messages)
		if err := target.ReplaceHistory(ctx, sessionID, messages); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: replace history: %v", source.RelativePath, err))
			continue
		}
		appendImportEvent(ctx, target, sessionID, session.ID, &result)
		result.ImportedSessions++
		result.ImportedMessages += len(messages)
		existing[sessionID] = true
		if session.Cwd != "" && !registeredWorkspaces[session.Cwd] {
			registeredWorkspaces[session.Cwd] = true
			if info, statErr := os.Stat(session.Cwd); statErr == nil && info.IsDir() {
				if workspaces == nil {
					result.Errors = append(result.Errors,
						fmt.Sprintf("%s: register workspace %q: workspace service unavailable", source.RelativePath, session.Cwd))
				} else if err := workspaces.Create(ctx, filepath.Base(session.Cwd), session.Cwd); err != nil {
					result.Errors = append(result.Errors,
						fmt.Sprintf("%s: register workspace %q: %v", source.RelativePath, session.Cwd, err))
				} else {
					result.ImportedWorkspaces++
					registeredAny = true
				}
			}
		}
	}
	if previousCurrentID != "" && registeredAny && workspaces != nil {
		if err := workspaces.Open(ctx, previousCurrentID); err != nil && logger != nil {
			logger.Printf("importer: restore current workspace %q after import: %v", previousCurrentID, err)
		}
	}
	return result, nil
}

// appendImportEvent records the optional durable "import" event.
func appendImportEvent(ctx context.Context, target Target, sessionID, sourceID string, result *Result) {
	payload, err := json.Marshal(map[string]string{"source": strings.TrimSpace(sourceID)})
	if err != nil {
		return
	}
	if err := target.AppendEvent(ctx, rig.SessionEvent{
		SessionID: sessionID,
		Type:      "import",
		Time:      time.Now().UTC(),
		Payload:   payload,
	}); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s: append import event: %v", sessionID, err))
	}
}
