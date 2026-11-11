package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// CacheKey hashes exactly what the node references: {type, typeVersion,
// resolvedConfig}. resolvedConfig already embeds upstream values through
// substitution, so hashing all of upstreamOutputs would let a needs-only
// edge or an unrelated output invalidate the cache for no reason
// (engine rule 3).
func CacheKey(nodeType, typeVersion string, resolvedConfig any) (string, error) {
	payload := map[string]any{
		"type":        nodeType,
		"typeVersion": typeVersion,
		"config":      resolvedConfig,
	}
	norm, err := driver.Normalize(payload)
	if err != nil {
		return "", fmt.Errorf("normalizing cache input: %w", err)
	}
	// encoding/json sorts map keys; Normalize already canonicalized numbers
	// and deep-copied, so the encoding is stable across runs.
	b, err := json.Marshal(norm)
	if err != nil {
		return "", fmt.Errorf("encoding cache input: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Store persists action outputs keyed by cache key. Verify is the slot for
// design §1's "cache hit is verified, not trusted"; Phase 2 stubs it to
// true so a later registry check slots in without an interface change.
type Store interface {
	Get(key string) (Outputs, bool, error)
	Put(key string, outs Outputs) error
	Verify(key string, outs Outputs) bool
}

// MemoryStore is an in-memory Store for tests. It is safe for concurrent
// use: parallel nodes Get and Put at the same time.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]Outputs
}

// NewMemoryStore returns an empty memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]Outputs{}}
}

// Get implements Store.
func (m *MemoryStore) Get(key string) (Outputs, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	outs, ok := m.entries[key]
	if !ok {
		return nil, false, nil
	}
	return outs, true, nil
}

// Put implements Store.
func (m *MemoryStore) Put(key string, outs Outputs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[key] = outs
	return nil
}

// Verify implements Store; Phase 2 always trusts the entry.
func (m *MemoryStore) Verify(key string, outs Outputs) bool { return true }

// DiskStore persists entries as one JSON file per key under dir. Writes are
// atomic: temp file then rename, so a crash never leaves a half-written
// entry (engine rule 4 territory: callers only Put after success).
type DiskStore struct {
	dir string
}

// NewDiskStore creates dir if needed and returns a store rooted there.
func NewDiskStore(dir string) (*DiskStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating cache dir %s: %w", dir, err)
	}
	return &DiskStore{dir: dir}, nil
}

func (d *DiskStore) path(key string) string {
	// Keys are hex sha256, but sanitize anyway against path tricks.
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '_'
	}, key)
	return filepath.Join(d.dir, clean+".json")
}

// Get implements Store.
func (d *DiskStore) Get(key string) (Outputs, bool, error) {
	b, err := os.ReadFile(d.path(key))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("reading cache entry: %w", err)
	}
	var outs Outputs
	if err := json.Unmarshal(b, &outs); err != nil {
		// Corrupt entry: treat as a miss rather than failing the run.
		return nil, false, nil
	}
	// JSON round-trip loses Known on zero-value Data; entries are only
	// written after a successful run, so everything stored is known.
	for k, v := range outs {
		v.Known = true
		v.From = ""
		outs[k] = v
	}
	return outs, true, nil
}

// Put implements Store with a temp-file + rename write.
func (d *DiskStore) Put(key string, outs Outputs) error {
	b, err := json.Marshal(outs)
	if err != nil {
		return fmt.Errorf("encoding cache entry: %w", err)
	}
	final := d.path(key)
	tmp, err := os.CreateTemp(d.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp cache file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing cache entry: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("committing cache entry: %w", err)
	}
	return nil
}

// Verify implements Store; Phase 2 always trusts the entry.
func (d *DiskStore) Verify(key string, outs Outputs) bool { return true }

// SortedKeys is a test helper: deterministic key listing.
func SortedKeys(m map[string]Outputs) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
