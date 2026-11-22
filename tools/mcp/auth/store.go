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
	// TokenDocumentVersion is the plaintext on-disk schema version (base64
	// token records readable by every host).
	TokenDocumentVersion = 1
	// TokenDocumentVersionEncrypted marks a document whose token records are
	// encrypted by the configured TokenCipher. The server-name keys stay
	// readable so a host can still delete individual records without the key.
	TokenDocumentVersionEncrypted = 2
	// tokenStoreFileMode is the token file permission.
	tokenStoreFileMode = 0o600
	// tokenStoreDirMode is the token directory permission.
	tokenStoreDirMode = 0o700
)

// TokenCipher encrypts and decrypts one opaque token record for at-rest
// storage. Hosts plug in an OS keyring or an AES implementation; the returned
// ciphertext is base64-encoded inside the token document. Encrypt and Decrypt
// must be deterministic for the same key/input and must not retain the input.
type TokenCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
}

// FileStoreOption configures a FileStore created by FileTokenStore.
type FileStoreOption func(*FileStore)

// WithCipher installs a TokenCipher. A non-nil cipher makes every later Save
// upgrade the document to TokenDocumentVersionEncrypted (legacy plaintext
// records are re-encrypted on first save). A nil cipher keeps the plaintext
// schema; opening an encrypted document without the cipher fails closed.
func WithCipher(cipher TokenCipher) FileStoreOption {
	return func(store *FileStore) {
		if store != nil {
			store.cipher = cipher
		}
	}
}

// tokenDocument is the persisted shape: server name -> base64 token record
// (plaintext for v1, ciphertext for v2).
type tokenDocument struct {
	Version int               `json:"version"`
	Tokens  map[string]string `json:"tokens,omitempty"`
}

// FileStore implements mcp.TokenStore over one JSON document. It is safe for
// concurrent use. The zero value is not usable; call FileTokenStore.
type FileStore struct {
	path   string
	cipher TokenCipher
	mu     sync.Mutex
	// memory holds the tokens when path is empty.
	memory map[string][]byte
}

// FileTokenStore returns a token store backed by path. An empty path creates a
// memory-only store. The document (and its directory) is created lazily on the
// first Save.
func FileTokenStore(path string, options ...FileStoreOption) *FileStore {
	path = strings.TrimSpace(path)
	store := &FileStore{path: path}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
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
	if document.Version == TokenDocumentVersionEncrypted {
		if s.cipher == nil {
			return nil, false, fmt.Errorf("mcp/auth: token store %q is encrypted but no cipher is configured", s.path)
		}
		data, err = s.cipher.Decrypt(data)
		if err != nil {
			return nil, false, fmt.Errorf("mcp/auth: decrypt token for %q: %w", server, err)
		}
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
	tokens, err := s.decodeTokensLocked(document)
	if err != nil {
		return err
	}
	tokens[server] = append([]byte(nil), data...)
	return s.writeTokensLocked(tokens)
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
	// Deleting does not reveal any token bytes, so it can proceed without the
	// cipher and preserves the document's current schema version.
	return s.writeDocumentLocked(document)
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
	switch document.Version {
	case TokenDocumentVersion, TokenDocumentVersionEncrypted:
	default:
		return tokenDocument{}, fmt.Errorf("mcp/auth: token store %q has unsupported version %d", s.path, document.Version)
	}
	if document.Tokens == nil {
		document.Tokens = make(map[string]string)
	}
	return document, nil
}

// decodeTokensLocked returns the plaintext token map. A v2 document requires
// the configured cipher; a wrong cipher fails before any write. The caller
// holds s.mu.
func (s *FileStore) decodeTokensLocked(document tokenDocument) (map[string][]byte, error) {
	tokens := make(map[string][]byte, len(document.Tokens))
	for server, encoded := range document.Tokens {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("mcp/auth: decode token for %q: %w", server, err)
		}
		if document.Version == TokenDocumentVersionEncrypted {
			if s.cipher == nil {
				return nil, fmt.Errorf("mcp/auth: token store %q is encrypted but no cipher is configured", s.path)
			}
			data, err = s.cipher.Decrypt(data)
			if err != nil {
				return nil, fmt.Errorf("mcp/auth: decrypt token for %q: %w", server, err)
			}
		}
		tokens[server] = data
	}
	return tokens, nil
}

// writeTokensLocked encodes the token map for the configured schema (encrypted
// v2 with a cipher, plaintext v1 otherwise) and writes it atomically. The
// caller holds s.mu.
func (s *FileStore) writeTokensLocked(tokens map[string][]byte) error {
	document := tokenDocument{Version: TokenDocumentVersion, Tokens: make(map[string]string, len(tokens))}
	if s.cipher != nil {
		document.Version = TokenDocumentVersionEncrypted
		for server, data := range tokens {
			encrypted, err := s.cipher.Encrypt(data)
			if err != nil {
				return fmt.Errorf("mcp/auth: encrypt token for %q: %w", server, err)
			}
			document.Tokens[server] = base64.StdEncoding.EncodeToString(encrypted)
		}
		return s.writeDocumentLocked(document)
	}
	for server, data := range tokens {
		document.Tokens[server] = base64.StdEncoding.EncodeToString(data)
	}
	return s.writeDocumentLocked(document)
}

// writeDocumentLocked writes the document atomically. The caller holds s.mu.
func (s *FileStore) writeDocumentLocked(document tokenDocument) error {
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
