// Package workspace implements a host-agnostic workspace backend: the durable
// project-root registry and the sandboxed file reader/search behind a files
// inspector.
//
// The registry document is
//
//	{"workspaces":[{"id":"ws-...","name":"...","root":"...","createdAt":"..."}],"currentId":"ws-..."}
//
// and every mutation is written atomically (temporary file + Sync + rename in
// the same directory), so a crash cannot leave a half-written registry. A
// missing file starts an empty registry; an unreadable or malformed file is
// quarantined as <path>.corrupt-<unixnano> and the service starts empty instead
// of failing activation (the original bytes are preserved for recovery).
//
// File access is confined to the current workspace root: only relative paths
// are accepted, ".." escapes are rejected, and symbolic links are resolved and
// re-checked against the canonical root, so a link can never read outside the
// workspace. Reads are bounded by a configurable byte limit and search by a
// directory depth and a result count.
//
// The package deliberately has no product dependencies: hosts supply the data
// directory (or an absolute registry path), a recovery logger, and call the
// plain methods. It does not run git, render anything, or perform content
// search; see the README-level docs in the method comments for the exact
// semantics.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFileName is the registry file name under the data directory.
	DefaultFileName = "workspaces.json"

	// DefaultReadLimitBytes caps one ReadFile (1 MiB).
	DefaultReadLimitBytes = 1 << 20

	// DefaultSearchDepth is how many directory levels Search descends into.
	DefaultSearchDepth = 6

	// DefaultSearchLimit caps one Search result set.
	DefaultSearchLimit = 50

	// fileMode is the registry file permission.
	fileMode = 0o600

	// dirMode is the registry directory permission.
	dirMode = 0o700
)

// Config configures a Service. The zero value is valid: the registry lives at
// <dataDir>/workspaces.json with the default read and search bounds.
type Config struct {
	// Path is the registry file: absolute, or relative to the data directory
	// (default workspaces.json there).
	Path string
	// ReadLimitBytes caps one ReadFile (0 = default 1 MiB).
	ReadLimitBytes int
	// SearchDepth bounds the recursive Search directory depth (0 = default 6).
	SearchDepth int
	// Logger receives recovery warnings such as a quarantined registry (nil
	// discards them).
	Logger func(format string, args ...any)
}

// Errors returned by the workspace service.
var (
	// ErrNoWorkspace reports a file operation without a current workspace.
	ErrNoWorkspace = errors.New("workspace: no current workspace")
	// ErrWorkspaceNotFound reports an unknown workspace ID.
	ErrWorkspaceNotFound = errors.New("workspace: not found")
	// ErrPathOutsideRoot reports a path escaping the workspace root.
	ErrPathOutsideRoot = errors.New("workspace: path is outside the workspace root")
	// ErrNotDirectory reports a root or directory argument that is not a
	// directory.
	ErrNotDirectory = errors.New("workspace: not a directory")
	// ErrNotFile reports a ReadFile target that is not a regular file.
	ErrNotFile = errors.New("workspace: not a regular file")
	// ErrInvalidLimit reports a negative ReadFile/Search limit.
	ErrInvalidLimit = errors.New("workspace: invalid limit")
	// ErrNameEmpty reports a rename to an empty display name.
	ErrNameEmpty = errors.New("workspace: workspace name is empty")
	// ErrNameTaken reports a rename to a name another workspace already uses
	// (trimmed, case-insensitive).
	ErrNameTaken = errors.New("workspace: workspace name is already in use")
)

// Workspace is one registered project root.
type Workspace struct {
	// ID is the durable workspace identity ("ws-<unixnano>").
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Root is the canonical absolute project root.
	Root string `json:"root"`
	// CreatedAt is when the workspace was registered.
	CreatedAt time.Time `json:"createdAt"`
	// Color is the UI palette id chosen for this workspace (empty = default).
	Color string `json:"color,omitempty"`
}

// FileEntry is one entry of a workspace directory listing or search result.
type FileEntry struct {
	// Name is the base name.
	Name string `json:"name"`
	// Path is the path relative to the workspace root (slash separated).
	Path string `json:"path"`
	// Dir marks a directory.
	Dir bool `json:"dir"`
	// Size is the file size in bytes (directories and symlinks: the lstat
	// size).
	Size int64 `json:"size,omitempty"`
	// ModTime is the entry's modification time.
	ModTime time.Time `json:"modTime,omitempty"`
}

// workspaceRecord is one workspace in the JSON registry.
type workspaceRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"createdAt"`
	Color     string    `json:"color,omitempty"`
}

// registryDocument is the on-disk registry.
type registryDocument struct {
	Workspaces []workspaceRecord `json:"workspaces"`
	CurrentID  string            `json:"currentId,omitempty"`
}

// Service is a durable workspace registry plus the sandboxed file service of
// its current workspace. It is safe for concurrent use. The zero value is not
// usable; call New.
type Service struct {
	mu sync.Mutex

	path   string
	logger func(format string, args ...any)

	readLimit   int
	searchDepth int

	workspaces []Workspace
	currentID  string
}

// New opens (or starts) the registry at cfg.Path under dataDir. A missing file
// starts an empty registry; an unreadable or malformed file is quarantined and
// the service starts empty. Only invalid configuration and an unresolvable path
// are errors.
func New(dataDir string, cfg Config) (*Service, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	path, err := resolveRegistryPath(dataDir, cfg.Path)
	if err != nil {
		return nil, err
	}
	readLimit := cfg.ReadLimitBytes
	if readLimit == 0 {
		readLimit = DefaultReadLimitBytes
	}
	searchDepth := cfg.SearchDepth
	if searchDepth == 0 {
		searchDepth = DefaultSearchDepth
	}
	service := &Service{
		path:        path,
		logger:      cfg.Logger,
		readLimit:   readLimit,
		searchDepth: searchDepth,
	}
	service.load()
	return service, nil
}

// validateConfig rejects configs the service could not honour.
func validateConfig(cfg Config) error {
	if strings.ContainsRune(cfg.Path, 0) {
		return errors.New("workspace: path contains a NUL byte")
	}
	if cfg.ReadLimitBytes < 0 {
		return errors.New("workspace: readLimitBytes must not be negative")
	}
	if cfg.SearchDepth < 0 {
		return errors.New("workspace: searchDepth must not be negative")
	}
	return nil
}

// resolveRegistryPath resolves Config.Path against the data directory.
func resolveRegistryPath(dataDir, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = DefaultFileName
	}
	if strings.ContainsRune(requested, 0) {
		return "", errors.New("workspace: path contains a NUL byte")
	}
	if filepath.IsAbs(requested) {
		return filepath.Clean(requested), nil
	}
	dataDir = strings.TrimSpace(dataDir)
	if dataDir == "" {
		return "", errors.New("workspace: data directory is empty")
	}
	return filepath.Join(dataDir, filepath.FromSlash(requested)), nil
}

// Path returns the resolved registry file path (diagnostics and tests).
func (s *Service) Path() string { return s.path }

// load reads the registry, treating a missing file as empty. An unreadable or
// malformed registry is quarantined: the bad file is renamed to
// <path>.corrupt-<nanos>, a warning is logged and the service starts with no
// workspaces instead of failing.
func (s *Service) load() {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		// A non-NotExist READ error (EIO/ESTALE/ELOOP) quarantines exactly like
		// a decode error: starting empty while the entry stays in place would
		// let the next persist overwrite it and lose the original bytes.
		// Rename is a directory operation, so it succeeds even when the file
		// itself cannot be read.
		s.quarantine(fmt.Sprintf("workspace registry %q could not be read (%v)", s.path, err))
		return
	}
	document, err := decodeRegistry(data)
	if err != nil {
		s.quarantine(fmt.Sprintf("workspace registry %q is corrupt (%v)", s.path, err))
		return
	}
	workspaces := make([]Workspace, 0, len(document.Workspaces))
	for _, record := range document.Workspaces {
		if strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.Root) == "" {
			continue
		}
		workspaces = append(workspaces, Workspace(record))
	}
	s.workspaces = workspaces
	s.currentID = strings.TrimSpace(document.CurrentID)
}

// quarantine renames the unusable registry out of the way, logs a warning and
// lets the service start empty.
func (s *Service) quarantine(reason string) {
	backup := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().UTC().UnixNano())
	message := reason + "; starting with no workspaces"
	if err := os.Rename(s.path, backup); err != nil {
		message += fmt.Sprintf(" (could not quarantine the file: %v)", err)
	} else {
		message += fmt.Sprintf(" (quarantined as %s)", backup)
	}
	if s.logger != nil {
		s.logger("%s: %s", "workspace", message)
	}
}

// decodeRegistry accepts the canonical object and, for convenience, a bare
// workspace array. A blank document is an empty registry.
func decodeRegistry(data []byte) (registryDocument, error) {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return registryDocument{}, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var records []workspaceRecord
		if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
			return registryDocument{}, err
		}
		return registryDocument{Workspaces: records}, nil
	}
	var document registryDocument
	if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
		return registryDocument{}, err
	}
	return document, nil
}

// persistLocked writes the registry atomically (temporary file + Sync +
// rename). The caller holds mu.
func (s *Service) persistLocked() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, dirMode); err != nil {
		return fmt.Errorf("workspace: create registry directory %q: %w", directory, err)
	}
	document := registryDocument{CurrentID: s.currentID}
	document.Workspaces = make([]workspaceRecord, 0, len(s.workspaces))
	for _, workspace := range s.workspaces {
		document.Workspaces = append(document.Workspaces, workspaceRecord(workspace))
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: encode registry: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".workspaces-*.tmp")
	if err != nil {
		return fmt.Errorf("workspace: create temporary registry: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(fileMode); err != nil {
		cleanup()
		return fmt.Errorf("workspace: chmod temporary registry: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("workspace: write temporary registry: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("workspace: sync temporary registry: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("workspace: close temporary registry: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("workspace: replace registry %q: %w", s.path, err)
	}
	return nil
}

// cloneWorkspaces returns a copy of the workspace slice.
func cloneWorkspaces(workspaces []Workspace) []Workspace {
	return append([]Workspace(nil), workspaces...)
}

// contextErr returns ctx.Err(), tolerating a nil context.
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// --- registry operations ---------------------------------------------------

// List returns the registered workspaces in insertion order.
func (s *Service) List(ctx context.Context) ([]Workspace, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneWorkspaces(s.workspaces), nil
}

// Current returns the current workspace. The bool is false when no workspace is
// current (an empty registry, or a registry whose currentId no longer matches).
func (s *Service) Current(ctx context.Context) (Workspace, bool, error) {
	if err := contextErr(ctx); err != nil {
		return Workspace{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, workspace := range s.workspaces {
		if workspace.ID == s.currentID {
			return workspace, true, nil
		}
	}
	return Workspace{}, false, nil
}

// Create registers the project root and makes it current. The root must exist
// and be a directory; it is canonicalized (absolute, symlinks resolved). A root
// that is already registered is reused: its existing workspace becomes current
// and no duplicate is added. An empty name defaults to the root's base name.
func (s *Service) Create(ctx context.Context, name, root string) (Workspace, error) {
	if err := contextErr(ctx); err != nil {
		return Workspace{}, err
	}
	root = strings.TrimSpace(root)
	if root == "" {
		return Workspace{}, errors.New("workspace: root is required")
	}
	if strings.ContainsRune(root, 0) {
		return Workspace{}, errors.New("workspace: root contains a NUL byte")
	}
	absolute, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: resolve root %q: %w", root, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: workspace root %q: %w", root, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: workspace root %q: %w", root, err)
	}
	if !info.IsDir() {
		return Workspace{}, fmt.Errorf("%w: %q", ErrNotDirectory, root)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = filepath.Base(canonical)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.workspaces {
		if existing.Root != canonical {
			continue
		}
		previousCurrent := s.currentID
		if existing.ID != s.currentID {
			s.currentID = existing.ID
			if err := s.persistLocked(); err != nil {
				s.currentID = previousCurrent
				return Workspace{}, err
			}
		}
		return existing, nil
	}

	workspace := Workspace{
		ID:        s.newIDLocked(),
		Name:      name,
		Root:      canonical,
		CreatedAt: time.Now().UTC(),
	}
	previous := cloneWorkspaces(s.workspaces)
	previousCurrent := s.currentID
	s.workspaces = append(s.workspaces, workspace)
	s.currentID = workspace.ID
	if err := s.persistLocked(); err != nil {
		s.workspaces = previous
		s.currentID = previousCurrent
		return Workspace{}, err
	}
	return workspace, nil
}

// newIDLocked generates a unique "ws-<unixnano>" ID. The caller holds mu.
func (s *Service) newIDLocked() string {
	for {
		id := fmt.Sprintf("ws-%d", time.Now().UnixNano())
		exists := false
		for _, workspace := range s.workspaces {
			if workspace.ID == id {
				exists = true
				break
			}
		}
		if !exists {
			return id
		}
	}
}

// Open makes an existing workspace current and persists the registry.
func (s *Service) Open(ctx context.Context, id string) (Workspace, error) {
	if err := contextErr(ctx); err != nil {
		return Workspace{}, err
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, workspace := range s.workspaces {
		if workspace.ID != id {
			continue
		}
		previous := s.currentID
		s.currentID = workspace.ID
		if err := s.persistLocked(); err != nil {
			s.currentID = previous
			return Workspace{}, err
		}
		return workspace, nil
	}
	return Workspace{}, fmt.Errorf("%w: %q", ErrWorkspaceNotFound, id)
}

// Delete unregisters a workspace. The files on disk are not touched. Deleting
// the current workspace falls back to the most recently created remaining one
// (deterministically the later registry entry on equal times), so the file
// service keeps a usable root; with none left the current workspace is empty.
func (s *Service) Delete(ctx context.Context, id string) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for position, workspace := range s.workspaces {
		if workspace.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return fmt.Errorf("%w: %q", ErrWorkspaceNotFound, id)
	}
	previous := cloneWorkspaces(s.workspaces)
	previousCurrent := s.currentID
	s.workspaces = append(append([]Workspace(nil), s.workspaces[:index]...), s.workspaces[index+1:]...)
	if s.currentID == id {
		s.currentID = s.fallbackCurrentLocked()
	}
	if err := s.persistLocked(); err != nil {
		s.workspaces = previous
		s.currentID = previousCurrent
		return err
	}
	return nil
}

// fallbackCurrentLocked picks the current workspace after the current one was
// deleted: the most recently created remaining workspace. Ties are broken by
// registry order (later entries are newer), so the choice is deterministic
// across loads even for equal or legacy zero CreatedAt values. It returns ""
// when no workspace remains.
func (s *Service) fallbackCurrentLocked() string {
	selected := ""
	var newest time.Time
	for _, workspace := range s.workspaces {
		if workspace.ID == "" {
			continue
		}
		if selected == "" || !workspace.CreatedAt.Before(newest) {
			selected = workspace.ID
			newest = workspace.CreatedAt
		}
	}
	return selected
}

// Rename changes one workspace's display name and persists the registry
// atomically. The ID, root and creation time are preserved, and so is the
// current selection. An empty name and a name another workspace already uses
// (trimmed, case-insensitive) are rejected with ErrNameEmpty/ErrNameTaken; a
// rejected rename writes nothing.
func (s *Service) Rename(ctx context.Context, id, name string) (Workspace, error) {
	if err := contextErr(ctx); err != nil {
		return Workspace{}, err
	}
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if name == "" {
		return Workspace{}, fmt.Errorf("%w: %q", ErrNameEmpty, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for position, workspace := range s.workspaces {
		if workspace.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return Workspace{}, fmt.Errorf("%w: %q", ErrWorkspaceNotFound, id)
	}
	for _, other := range s.workspaces {
		if other.ID == id {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(other.Name), name) {
			return Workspace{}, fmt.Errorf("%w: %q", ErrNameTaken, name)
		}
	}
	previous := cloneWorkspaces(s.workspaces)
	renamed := s.workspaces[index]
	renamed.Name = name
	s.workspaces[index] = renamed
	if err := s.persistLocked(); err != nil {
		s.workspaces = previous
		return Workspace{}, err
	}
	return renamed, nil
}

// currentWorkspaceRoot returns the current workspace and its canonical root.
// SetColor stores the UI palette id for one workspace. The empty string resets
// it to the default. The value is opaque to the registry (the host owns the
// palette), bounded to 32 bytes so a corrupt UI cannot grow the document.
func (s *Service) SetColor(ctx context.Context, id, color string) (Workspace, error) {
	if err := contextErr(ctx); err != nil {
		return Workspace{}, err
	}
	id = strings.TrimSpace(id)
	color = strings.TrimSpace(color)
	if len(color) > 32 {
		return Workspace{}, fmt.Errorf("workspace: color id too long")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for position, workspace := range s.workspaces {
		if workspace.ID == id {
			index = position
			break
		}
	}
	if index < 0 {
		return Workspace{}, fmt.Errorf("%w: %q", ErrWorkspaceNotFound, id)
	}
	previous := cloneWorkspaces(s.workspaces)
	s.workspaces[index].Color = color
	if err := s.persistLocked(); err != nil {
		s.workspaces = previous
		return Workspace{}, err
	}
	return s.workspaces[index], nil
}

func (s *Service) currentWorkspaceRoot() (Workspace, string, error) {
	s.mu.Lock()
	workspace, ok := Workspace{}, false
	for _, candidate := range s.workspaces {
		if candidate.ID == s.currentID {
			workspace, ok = candidate, true
			break
		}
	}
	s.mu.Unlock()
	if !ok {
		return Workspace{}, "", ErrNoWorkspace
	}
	root, err := filepath.EvalSymlinks(workspace.Root)
	if err != nil {
		return Workspace{}, "", fmt.Errorf("workspace: workspace root %q: %w", workspace.Root, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return Workspace{}, "", fmt.Errorf("workspace: workspace root %q: %w", workspace.Root, err)
	}
	if !info.IsDir() {
		return Workspace{}, "", fmt.Errorf("%w: %q", ErrNotDirectory, workspace.Root)
	}
	return workspace, filepath.Clean(root), nil
}
