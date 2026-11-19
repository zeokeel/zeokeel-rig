package sqlite

// Oversized tool results, stored beside the session that produced them. The
// model sees a short handle; the bytes stay in the database and are collected
// with the rest of a session's state.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	rig "github.com/zeokeel/zeokeel-rig"
)

// Spill returns the spill store backed by this database.
func (s *Store) Spill() rig.SpillStore { return spillStore{store: s} }

// spillStore implements rig.SpillStore over rig_spill.
type spillStore struct{ store *Store }

var _ rig.SpillStore = spillStore{}

// Save stores one oversized result and returns its handle.
func (s spillStore) Save(ctx context.Context, call rig.ToolCall, content string) (rig.SpillRef, error) {
	if err := s.store.usable(); err != nil {
		return rig.SpillRef{}, err
	}
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	now := time.Now().UTC()
	id := fmt.Sprintf("spill-%013d-%s", now.UnixMilli(), hex.EncodeToString(suffix[:]))
	if _, err := s.store.db.ExecContext(ctx,
		`INSERT INTO rig_spill(id, tool, bytes, content, created_at) VALUES(?, ?, ?, ?, ?)`,
		id, call.Name, len(content), content, now.UnixMilli()); err != nil {
		return rig.SpillRef{}, fmt.Errorf("sqlite: save spilled result: %w", err)
	}
	return rig.SpillRef{ID: id, Bytes: len(content)}, nil
}

// Load returns the stored content behind a handle.
func (s spillStore) Load(ctx context.Context, id string) (string, error) {
	if err := s.store.usable(); err != nil {
		return "", err
	}
	var content string
	err := s.store.db.QueryRowContext(ctx, `SELECT content FROM rig_spill WHERE id = ?`, id).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", rig.ErrSpillNotFound
	}
	if err != nil {
		return "", fmt.Errorf("sqlite: load spilled result %q: %w", id, err)
	}
	return content, nil
}

// GC keeps the newest keep results and drops the rest.
func (s spillStore) GC(ctx context.Context, keep int) (int, error) {
	if err := s.store.usable(); err != nil {
		return 0, err
	}
	if keep <= 0 {
		return 0, nil
	}
	result, err := s.store.db.ExecContext(ctx,
		`DELETE FROM rig_spill WHERE id NOT IN (
		   SELECT id FROM rig_spill ORDER BY seq DESC LIMIT ?
		 )`, keep)
	if err != nil {
		return 0, fmt.Errorf("sqlite: collect spilled results: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite: collect spilled results: %w", err)
	}
	return int(affected), nil
}
