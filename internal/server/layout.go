package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// maxLayoutBytes bounds what will be accepted and stored.
const maxLayoutBytes = 4 << 20

// layoutStore persists canvas arrangements.
//
// State is kept outside the repository, in the user's XDG state directory, so
// the tool never writes into a working tree it was only asked to read.
type layoutStore struct {
	mu   sync.Mutex
	path string
}

func newLayoutStore(repoRoot string) (*layoutStore, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "diffcanvas")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating state directory: %w", err)
	}

	// The repo path is hashed rather than embedded: a path can contain
	// characters that are awkward in filenames, and on a shared machine the
	// filename itself should not disclose what is being reviewed.
	sum := sha256.Sum256([]byte(repoRoot))
	name := hex.EncodeToString(sum[:8]) + ".json"
	return &layoutStore{path: filepath.Join(dir, name)}, nil
}

func (l *layoutStore) readAll() map[string]json.RawMessage {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return map[string]json.RawMessage{}
	}
	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &out); err != nil {
		return map[string]json.RawMessage{}
	}
	return out
}

func (l *layoutStore) Get(key string) json.RawMessage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readAll()[key]
}

// Put writes the layout, replacing the file atomically so an interrupted write
// cannot leave a truncated file that loses every saved arrangement.
func (l *layoutStore) Put(key string, value json.RawMessage) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	all := l.readAll()
	if len(value) == 0 || string(value) == "null" {
		delete(all, key)
	} else {
		all[key] = value
	}

	data, err := json.Marshal(all)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".layout-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.path)
}

// layoutKey identifies one arrangement. Layouts are per revspec, because the
// same repository reviewed at a different range is a different set of cards.
func (s *Server) layoutKey() string {
	return s.Spec.Kind + ":" + s.Spec.Raw
}

func (s *Server) handleLayout(ctx context.Context, r *http.Request) (any, error) {
	switch r.Method {
	case http.MethodGet:
		raw := s.store.Get(s.layoutKey())
		if len(raw) == 0 {
			return map[string]any{"layout": nil}, nil
		}
		return map[string]any{"layout": json.RawMessage(raw)}, nil

	case http.MethodPut, http.MethodPost:
		body, err := io.ReadAll(io.LimitReader(r.Body, maxLayoutBytes))
		if err != nil {
			return nil, err
		}
		if !json.Valid(body) {
			return nil, fmt.Errorf("layout is not valid JSON")
		}
		if err := s.store.Put(s.layoutKey(), body); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil

	default:
		return nil, fmt.Errorf("method not allowed")
	}
}
