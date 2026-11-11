// Package fake is the in-memory driver used to exercise the whole
// validate → plan → apply loop without Docker or a cluster. The CLI backs
// it with a JSON state file so results survive across process invocations;
// unit tests use the in-memory form directly.
package fake

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MRGHOSJ/afon/pkg/driver"
)

// Driver keeps a map of live objects. All access takes the mutex: parallel
// resource nodes call Plan and Apply concurrently (engine rule 6).
// Apply is atomic per call — new state is built in memory, written once
// with temp-and-rename, and only then swapped in; a failure midway leaves
// the previous state (memory and file) untouched.
//
// Known gap (engine rule 6): two processes writing the same state file at
// the same time are not coordinated; file locking lands with inventory.
type Driver struct {
	mu      sync.Mutex
	objects map[string]driver.Object
	// path is "" for pure in-memory use (unit tests).
	path string
}

// New returns an in-memory driver.
func New() *Driver {
	return &Driver{objects: map[string]driver.Object{}}
}

// NewPersistent returns a driver rooted at path. A missing file starts
// empty; a corrupt file is an error (never silently wipe state).
func NewPersistent(path string) (*Driver, error) {
	d := &Driver{objects: map[string]driver.Object{}, path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading fake state %s: %w", path, err)
	}
	var objs []driver.Object
	if err := json.Unmarshal(b, &objs); err != nil {
		return nil, fmt.Errorf("parsing fake state %s: %w", path, err)
	}
	for _, o := range objs {
		d.objects[o.Key()] = o
	}
	return d, nil
}

// Plan diffs desired objects against live state. Read-only: takes the lock
// but never mutates.
func (d *Driver) Plan(ctx context.Context, desired []driver.Object) ([]driver.Diff, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	diffs := make([]driver.Diff, 0, len(desired))
	for _, want := range desired {
		live, exists := d.objects[want.Key()]
		switch {
		case !exists:
			diffs = append(diffs, driver.Diff{Object: want, Op: driver.OpCreate})
		case driver.Equal(live.Data, want.Data):
			// Canonical comparison: 1 vs 1.0 and map ordering cannot
			// produce phantom updates (engine rule 7).
			diffs = append(diffs, driver.Diff{Object: want, Op: driver.OpNone})
		default:
			diffs = append(diffs, driver.Diff{Object: want, Op: driver.OpUpdate})
		}
	}
	return diffs, nil
}

// Apply carries diffs into live state, atomically per call.
func (d *Driver) Apply(ctx context.Context, diffs []driver.Diff) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	// Build the full next state in memory first.
	next := make(map[string]driver.Object, len(d.objects)+len(diffs))
	for k, v := range d.objects {
		next[k] = v
	}
	for _, df := range diffs {
		switch df.Op {
		case driver.OpNone:
			continue
		case driver.OpCreate, driver.OpUpdate:
			next[df.Object.Key()] = df.Object
		default:
			return fmt.Errorf("fake driver: unsupported op %q", df.Op)
		}
	}

	if d.path != "" {
		if err := writeState(d.path, next); err != nil {
			return err // old file and old memory both untouched
		}
	}
	d.objects = next
	return nil
}

// Objects returns a snapshot of live objects (test helper).
func (d *Driver) Objects() []driver.Object {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]driver.Object, 0, len(d.objects))
	for _, o := range d.objects {
		out = append(out, o)
	}
	return out
}

// writeState serializes next to path atomically: temp file in the same
// directory, fsync-free rename over the destination.
func writeState(path string, next map[string]driver.Object) error {
	objs := make([]driver.Object, 0, len(next))
	for _, o := range next {
		objs = append(objs, o)
	}
	b, err := json.MarshalIndent(objs, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding fake state: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".fake-state-*")
	if err != nil {
		return fmt.Errorf("creating temp state file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("writing state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("closing state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("committing state: %w", err)
	}
	return nil
}
