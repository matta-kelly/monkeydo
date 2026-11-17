package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// Record is what each lane owns on the node, kept between ticks, so a thing that leaves git is removed and nothing
// monkeydo did not put there is ever touched.
type Record struct {
	path  string
	lanes map[string][]string
}

// LoadRecord reads the record at path. A missing record is an empty one: the first tick owns nothing yet.
func LoadRecord(path string) (*Record, error) {
	r := &Record{path: path, lanes: map[string][]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &r.lanes); err != nil {
		return nil, err
	}
	return r, nil
}

// Owned is what lane owns.
func (r *Record) Owned(lane string) []string { return append([]string(nil), r.lanes[lane]...) }

// Set records what lane owns.
func (r *Record) Set(lane string, owned []string) {
	owned = append([]string(nil), owned...)
	sort.Strings(owned)
	if len(owned) == 0 {
		delete(r.lanes, lane)
		return
	}
	r.lanes[lane] = owned
}

// Save writes the record atomically.
func (r *Record) Save() error {
	data, err := json.MarshalIndent(r.lanes, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".new"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
