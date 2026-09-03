package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const manifestFile = "run.json"

// shareRecord is the durable per-share outcome. It is written after every
// publish attempt so an interrupted run resumes from what it already has.
type shareRecord struct {
	ID            string    `json:"id"`
	CRID          string    `json:"crid,omitempty"`
	ResourceID    string    `json:"resource_id,omitempty"`
	RoutingID     string    `json:"routing_id,omitempty"`
	PublishedAt   time.Time `json:"published_at,omitempty"`
	PublishMS     int64     `json:"publish_ms,omitempty"`
	FoundExisting *bool     `json:"found_existing,omitempty"`
	Attempts      int       `json:"attempts"`
	APICalls      int       `json:"api_calls"`
	HTTP429       int       `json:"http_429"`
	ExitCode      int       `json:"exit_code"`
	TimedOut      bool      `json:"publish_timed_out,omitempty"`
	Resumed       bool      `json:"resumed,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// manifest is the run's durable state. The origin port is pinned per run
// because a re-publish with a different target restarts the share.
type manifest struct {
	Run       string                  `json:"run"`
	N         int                     `json:"n"`
	Port      int                     `json:"port"`
	Endpoint  string                  `json:"endpoint"`
	CreatedAt time.Time               `json:"created_at"`
	Shares    map[string]*shareRecord `json:"shares"`

	path string
	mu   sync.Mutex
}

func loadOrCreateManifest(dir, runName string, n, port int, endpoint string) (*manifest, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, manifestFile)
	m := &manifest{Run: runName, N: n, Port: port, Endpoint: endpoint, CreatedAt: time.Now().UTC(), Shares: map[string]*shareRecord{}, path: path}
	raw, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		if err := m.save(); err != nil {
			return nil, err
		}
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if m.Run != runName {
		return nil, fmt.Errorf("%s belongs to run %q, not %q", path, m.Run, runName)
	}
	if m.Shares == nil {
		m.Shares = map[string]*shareRecord{}
	}
	m.N = n
	if m.Port == 0 {
		m.Port = port
	}
	if err := m.save(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *manifest) save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return writeJSONAtomic(m.path, m)
}

func (m *manifest) record(id string) *shareRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.Shares[id]
	if !ok {
		rec = &shareRecord{ID: id}
		m.Shares[id] = rec
	}
	return rec
}

// ordered returns the records for indexes 1..N in id order.
func (m *manifest) ordered() []*shareRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*shareRecord, 0, len(m.Shares))
	for _, rec := range m.Shares {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (m *manifest) resourceIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.Shares))
	for _, rec := range m.Shares {
		if rec.ResourceID != "" {
			ids = append(ids, rec.ResourceID)
		}
	}
	sort.Strings(ids)
	return ids
}

// writeRedactedJSON writes value as JSON with the redactor applied to the
// serialized text, so paths and identities never reach a shareable file.
func writeRedactedJSON(path string, value any, red *redactor) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(red.apply(string(raw))))
}

func writeJSONAtomic(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, raw)
}

func writeFileAtomic(path string, raw []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
