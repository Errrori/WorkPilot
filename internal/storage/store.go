package storage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrTooLarge  = errors.New("file too large")
	ErrBadPath   = errors.New("invalid storage path")
	ErrNoStorage = errors.New("file not found in storage")
)

// Store persists uploaded files on the local filesystem.
type Store struct {
	root string
}

func New(root string) (*Store, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve storage dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	return &Store{root: abs}, nil
}

func (s *Store) Root() string { return s.root }

// Save streams r into <root>/<groupID>/<random><ext>, enforcing maxBytes.
// It returns the slash-separated path relative to the storage root.
func (s *Store) Save(groupID, originalName string, r io.Reader, maxBytes int64) (string, int64, error) {
	dir := filepath.Join(s.root, groupID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", 0, err
	}
	name, err := randomName(originalName)
	if err != nil {
		return "", 0, err
	}
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxBytes+1))
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", 0, err
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", 0, closeErr
	}
	if n > maxBytes {
		_ = os.Remove(path)
		return "", 0, ErrTooLarge
	}
	return filepath.ToSlash(filepath.Join(groupID, name)), n, nil
}

// Open returns a reader for a path previously returned by Save.
func (s *Store) Open(rel string) (*os.File, error) {
	path, err := s.Path(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoStorage
	}
	return f, err
}

// Remove deletes a stored file; missing files are treated as removed.
func (s *Store) Remove(rel string) error {
	path, err := s.Path(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Path resolves a stored path and rejects anything outside the storage root.
func (s *Store) Path(rel string) (string, error) {
	if rel == "" {
		return "", ErrBadPath
	}
	path := filepath.Join(s.root, filepath.FromSlash(rel))
	if path != s.root && !strings.HasPrefix(path, s.root+string(os.PathSeparator)) {
		return "", ErrBadPath
	}
	return path, nil
}

func randomName(originalName string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]) + safeExt(originalName), nil
}

func safeExt(name string) string {
	base := filepath.Base(name)
	ext := strings.ToLower(filepath.Ext(base))
	if ext == "" || ext == base || len(ext) > 10 {
		return ""
	}
	for _, r := range ext[1:] {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return ext
}
