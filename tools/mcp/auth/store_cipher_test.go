package auth_test

// TDD: encrypted token-at-rest support for FileTokenStore. A host can install a
// TokenCipher (OS keyring / AES) through WithCipher; new documents carry an
// encrypted version marker, while legacy plaintext v1 documents stay readable.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tools/mcp/auth"
)

// xorCipher is a deterministic test cipher (not encryption for production).
type xorCipher struct{ key byte }

func (c xorCipher) Encrypt(plaintext []byte) ([]byte, error) {
	out := make([]byte, 0, len(plaintext)+1)
	out = append(out, c.key) // key tag: a wrong key fails authentication
	for _, value := range plaintext {
		out = append(out, value^c.key)
	}
	return out, nil
}

func (c xorCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 || ciphertext[0] != c.key {
		return nil, errors.New("wrong cipher key")
	}
	out := make([]byte, 0, len(ciphertext)-1)
	for _, value := range ciphertext[1:] {
		out = append(out, value^c.key)
	}
	return out, nil
}

type failingCipher struct {
	encryptErr error
	decryptErr error
}

func (c failingCipher) Encrypt([]byte) ([]byte, error) { return nil, c.encryptErr }
func (c failingCipher) Decrypt([]byte) ([]byte, error) { return nil, c.decryptErr }

func readTokenDocument(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token document: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode token document: %v", err)
	}
	return document
}

func TestFileTokenStoreCipherRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ctx := context.Background()
	secret := []byte(`{"version":1,"token":"alpha-secret"}`)
	second := []byte(`{"version":1,"token":"beta-secret"}`)
	cipher := xorCipher{key: 0x5a}

	store := auth.FileTokenStore(path, auth.WithCipher(cipher))
	if err := store.Save(ctx, "alpha", secret); err != nil {
		t.Fatalf("Save(alpha): %v", err)
	}
	if err := store.Save(ctx, "beta", second); err != nil {
		t.Fatalf("Save(beta): %v", err)
	}
	if data, found, err := store.Load(ctx, "alpha"); err != nil || !found || !bytes.Equal(data, secret) {
		t.Fatalf("Load(alpha) = %q/%v/%v", data, found, err)
	}

	// The file carries the encrypted version marker and no plaintext record.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tokens: %v", err)
	}
	if !strings.Contains(string(raw), `"version": 2`) {
		t.Fatalf("document version marker missing: %s", raw)
	}
	if bytes.Contains(raw, secret) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(secret))) {
		t.Fatalf("plaintext token leaked into %s", raw)
	}

	// A fresh store with the same cipher reads both records.
	reopened := auth.FileTokenStore(path, auth.WithCipher(cipher))
	if data, found, err := reopened.Load(ctx, "beta"); err != nil || !found || !bytes.Equal(data, second) {
		t.Fatalf("reopened Load(beta) = %q/%v/%v", data, found, err)
	}

	// The wrong cipher must fail closed on Load and Save (never clobber the
	// existing encrypted records).
	wrong := auth.FileTokenStore(path, auth.WithCipher(xorCipher{key: 0x11}))
	if _, _, err := wrong.Load(ctx, "alpha"); err == nil {
		t.Fatal("Load with the wrong cipher returned nil error")
	}
	if err := wrong.Save(ctx, "alpha", []byte("replacement")); err == nil {
		t.Fatal("Save with the wrong cipher returned nil error")
	}
	if data, found, err := reopened.Load(ctx, "alpha"); err != nil || !found || !bytes.Equal(data, secret) {
		t.Fatalf("correct store after wrong-cipher Save = %q/%v/%v", data, found, err)
	}
}

func TestFileTokenStoreCipherDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ctx := context.Background()
	cipher := xorCipher{key: 0x33}
	store := auth.FileTokenStore(path, auth.WithCipher(cipher))
	if err := store.Save(ctx, "alpha", []byte("one")); err != nil {
		t.Fatalf("Save(alpha): %v", err)
	}
	if err := store.Save(ctx, "beta", []byte("two")); err != nil {
		t.Fatalf("Save(beta): %v", err)
	}
	if err := store.Delete(ctx, "alpha"); err != nil {
		t.Fatalf("Delete(alpha): %v", err)
	}
	if _, found, err := store.Load(ctx, "alpha"); err != nil || found {
		t.Fatalf("alpha after delete = found %v, err %v", found, err)
	}
	if data, found, err := auth.FileTokenStore(path, auth.WithCipher(cipher)).Load(ctx, "beta"); err != nil || !found || string(data) != "two" {
		t.Fatalf("beta after delete = %q/%v/%v", data, found, err)
	}
	if err := store.Delete(ctx, "missing"); err != nil {
		t.Fatalf("Delete(missing): %v", err)
	}
	// The document keeps its encrypted version marker after a delete.
	if document := readTokenDocument(t, path); document["version"] != float64(auth.TokenDocumentVersionEncrypted) {
		t.Fatalf("version after delete = %v, want %d", document["version"], auth.TokenDocumentVersionEncrypted)
	}
}

func TestFileTokenStoreReadsLegacyPlaintext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	ctx := context.Background()
	legacy := []byte("legacy-token-bytes")
	plainDocument := map[string]any{
		"version": 1,
		"tokens": map[string]string{
			"alpha": base64.StdEncoding.EncodeToString(legacy),
		},
	}
	encoded, err := json.Marshal(plainDocument)
	if err != nil {
		t.Fatalf("encode legacy document: %v", err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write legacy document: %v", err)
	}

	// Legacy plaintext is readable with or without a cipher.
	for name, store := range map[string]*auth.FileStore{
		"plain":  auth.FileTokenStore(path),
		"cipher": auth.FileTokenStore(path, auth.WithCipher(xorCipher{key: 0x7f})),
	} {
		data, found, err := store.Load(ctx, "alpha")
		if err != nil || !found || !bytes.Equal(data, legacy) {
			t.Fatalf("%s Load(legacy) = %q/%v/%v", name, data, found, err)
		}
	}

	// Saving through a cipher upgrades the document and re-encrypts the legacy
	// record; a store without the cipher can no longer read it.
	cipherStore := auth.FileTokenStore(path, auth.WithCipher(xorCipher{key: 0x7f}))
	if err := cipherStore.Save(ctx, "beta", []byte("new-token")); err != nil {
		t.Fatalf("Save(beta): %v", err)
	}
	if document := readTokenDocument(t, path); document["version"] != float64(auth.TokenDocumentVersionEncrypted) {
		t.Fatalf("upgraded version = %v, want %d", document["version"], auth.TokenDocumentVersionEncrypted)
	}
	if data, found, err := cipherStore.Load(ctx, "alpha"); err != nil || !found || !bytes.Equal(data, legacy) {
		t.Fatalf("legacy after upgrade = %q/%v/%v", data, found, err)
	}
	if _, _, err := auth.FileTokenStore(path).Load(ctx, "alpha"); err == nil {
		t.Fatal("plaintext store read an encrypted document without a cipher")
	}
}

func TestFileTokenStoreCipherErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("cipher unavailable")

	t.Run("encrypt failure leaves no file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.json")
		store := auth.FileTokenStore(path, auth.WithCipher(failingCipher{encryptErr: boom}))
		if err := store.Save(ctx, "alpha", []byte("secret")); !errors.Is(err, boom) {
			t.Fatalf("Save error = %v, want %v", err, boom)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed Save left a token file: %v", err)
		}
	})

	t.Run("decrypt failure fails Load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.json")
		if err := auth.FileTokenStore(path, auth.WithCipher(xorCipher{key: 0x41})).Save(ctx, "alpha", []byte("secret")); err != nil {
			t.Fatalf("seed Save: %v", err)
		}
		store := auth.FileTokenStore(path, auth.WithCipher(failingCipher{decryptErr: boom}))
		if _, _, err := store.Load(ctx, "alpha"); !errors.Is(err, boom) {
			t.Fatalf("Load error = %v, want %v", err, boom)
		}
	})

	t.Run("nil cipher keeps plaintext schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "tokens.json")
		store := auth.FileTokenStore(path, auth.WithCipher(nil))
		if err := store.Save(ctx, "alpha", []byte("plain")); err != nil {
			t.Fatalf("Save: %v", err)
		}
		if document := readTokenDocument(t, path); document["version"] != float64(auth.TokenDocumentVersion) {
			t.Fatalf("plaintext version = %v, want %d", document["version"], auth.TokenDocumentVersion)
		}
	})
}
