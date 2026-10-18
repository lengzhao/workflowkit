package workflowkit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Load reads one workflow definition file. The workflow's ID defaults to the
// file name without its extension; an explicit id field wins.
func Load(path string) (*Workflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("workflowkit: read %s: %w", path, err)
	}
	wf, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if wf.ID == "" {
		wf.ID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	wf.Dir = filepath.Dir(path)
	wf.Path = path
	return wf, nil
}

// LoadDir reads every .json definition in dir, sorted by ID. A file that fails
// to parse is skipped and its error collected, so one bad definition does not
// hide the others.
func LoadDir(dir string) ([]*Workflow, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("workflowkit: read %s: %w", dir, err)}
	}
	var out []*Workflow
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		wf, err := Load(filepath.Join(dir, entry.Name()))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, wf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, errs
}

// LoadDirs reads definitions from several directories in precedence order.
// The first directory wins when two definitions share an ID, so a caller can
// layer a local directory over a shared one.
func LoadDirs(dirs ...string) ([]*Workflow, []error) {
	byID := map[string]*Workflow{}
	var errs []error
	for _, dir := range dirs {
		found, err := LoadDir(dir)
		errs = append(errs, err...)
		for _, wf := range found {
			if _, dup := byID[wf.ID]; dup {
				continue
			}
			byID[wf.ID] = wf
		}
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Workflow, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out, errs
}
