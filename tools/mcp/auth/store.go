package auth

// FileTokenStore is the SDK's file-backed mcp.TokenStore: a versioned JSON
// document holding one opaque token record per server, written atomically
// (temporary file + fsync + rename) with 0600 file / 0700 directory permissions.
// The record bytes stay opaque to this package (the parent tools/mcp package
// serializes them); they are base64-encoded inside the document so any byte
// sequence round-trips.
//
// An empty path keeps the store in memory (useful for tests and hosts that
// persist tokens themselves).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

const (
	// TokenDocumentVersion is the on-disk schema version this store writes and
	// accepts.
	TokenDocumentVersion = 1
	// tokenStoreFileMode is the token file permission.
	tokenStoreFileMode = 0o600
	// tokenStoreDirMode is the token directory permission.
	tokenStoreDirMode = 0o700
)

// tokenDocument is the persisted shape: server name -> base64 token record.
type tokenDocument struct {
	Version int               `json:"version"`
	Tokens  map[string]string `json:"tokens,omitempty"`
}

// FileStore implements mcp.TokenStore over one JSON document. It is safe for
// concurrent use. The zero value is not usable; call FileTokenStore.
type FileStore struct {
	path string
	mu   sync.Mutex
	// memory holds the tokens when path is empty.
	memory map[string][]byte
}

// FileTokenStore returns a token store backed by path. An empty path creates a
// memory-only store. The document (and its directory) is created lazily on the
// first Save.
func FileTokenStore(path string) *FileStore {
	path = strings.TrimSpace(path)
	store := &FileStore{path: path}
	if path == "" {
		store.memory = make(map[string][]byte)
	}
	return store
}

// Path returns the document path ("" for a memory-only store).
func (s *FileStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Load implements mcp.TokenStore.
func (s *FileStore) Load(ctx context.Context, server string) ([]byte, bool, error) {
	if s == nil {
		return nil, false, nil
	}
	if err := contextErr(ctx); err != nil {
		return nil, false, err
	}
	server = strings.TrimSpace(server)
	if server == "" {
		return nil, false, errors.New("mcp/auth: token server name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		data, ok := s.memory[server]
		if !ok {
			return nil, false, nil
		}
		return append([]byte(nil), data...), true, nil
	}
	document, err := s.readLocked()
	if err != nil {
		return nil, false, err
	}
	encoded, ok := document.Tokens[server]
	if !ok {
		return nil, false, nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("mcp/auth: decode token for %q: %w", server, err)
	}
	return data, true, nil
}

// Save implements mcp.TokenStore.
func (s *FileStore) Save(ctx context.Context, server string, data []byte) error {
	if s == nil {
		return nil
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	server = strings.TrimSpace(server)
	if server == "" {
		return errors.New("mcp/auth: token server name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		s.memory[server] = append([]byte(nil), data...)
		return nil
	}
	document, err := s.readLocked()
	if err != nil {
		return err
	}
	if document.Tokens == nil {
		document.Tokens = make(map[string]string)
	}
	document.Tokens[server] = base64.StdEncoding.EncodeToString(data)
	return s.writeLocked(document)
}

// Delete implements mcp.TokenStore. Deleting an unknown server is a no-op.
func (s *FileStore) Delete(ctx context.Context, server string) error {
	if s == nil {
		return nil
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	server = strings.TrimSpace(server)
	if server == "" {
		return errors.New("mcp/auth: token server name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		delete(s.memory, server)
		return nil
	}
	document, err := s.readLocked()
	if err != nil {
		return err
	}
	if _, ok := document.Tokens[server]; !ok {
		return nil
	}
	delete(document.Tokens, server)
	return s.writeLocked(document)
}

// readLocked reads the document; a missing or blank file is empty. The caller
// holds s.mu.
func (s *FileStore) readLocked() (tokenDocument, error) {
	document := tokenDocument{Version: TokenDocumentVersion, Tokens: make(map[string]string)}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) || isNotDirOrMissing(err) {
			return document, nil
		}
		return tokenDocument{}, fmt.Errorf("mcp/auth: read token store %q: %w", s.path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return document, nil
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return tokenDocument{}, fmt.Errorf("mcp/auth: token store %q is corrupt: %w", s.path, err)
	}
	if document.Version != TokenDocumentVersion {
		return tokenDocument{}, fmt.Errorf("mcp/auth: token store %q has unsupported version %d", s.path, document.Version)
	}
	if document.Tokens == nil {
		document.Tokens = make(map[string]string)
	}
	return document, nil
}

// writeLocked writes the document atomically. The caller holds s.mu.
func (s *FileStore) writeLocked(document tokenDocument) error {
	document.Version = TokenDocumentVersion
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("mcp/auth: encode token store: %w", err)
	}
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, tokenStoreDirMode); err != nil {
		return fmt.Errorf("mcp/auth: create token directory %q: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".tokens-*.tmp")
	if err != nil {
		return fmt.Errorf("mcp/auth: create temporary token store: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(tokenStoreFileMode); err != nil {
		cleanup()
		return fmt.Errorf("mcp/auth: chmod temporary token store: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("mcp/auth: write temporary token store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("mcp/auth: sync temporary token store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("mcp/auth: close temporary token store: %w", err)
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("mcp/auth: replace token store %q: %w", s.path, err)
	}
	if handle, err := os.Open(directory); err == nil {
		_ = handle.Sync()
		_ = handle.Close()
	}
	return nil
}

// contextErr returns ctx.Err(), tolerating a nil context.
func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// isNotDirOrMissing treats ENOTDIR like a missing path (a parent turned into a
// file during teardown); the document cannot exist in that state.
func isNotDirOrMissing(err error) bool {
	return errors.Is(err, syscall.ENOTDIR)
}
