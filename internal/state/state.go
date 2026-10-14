// Package state persists small JSON files in the state directory:
// project name ownership (projects.json) and resolved owner IDs (owners.json).
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrNameTaken means the project name belongs to another repository.
var ErrNameTaken = errors.New("project name is taken by another repository")

// Project records which repository owns a project name.
type Project struct {
	RepositoryID string    `json:"repository_id"`
	Repository   string    `json:"repository"`
	ClaimedAt    time.Time `json:"claimed_at"`
}

// Projects is the projects.json store. The file is re-read on every call, so
// manual edits (e.g. releasing a name) take effect without a restart.
type Projects struct {
	file string
	mu   sync.Mutex
}

func NewProjects(file string) *Projects {
	return &Projects{file: file}
}

// Claim binds name to repositoryID if the name is free. It returns isNew=true
// when the name was claimed by this call, and ErrNameTaken when another
// repository owns it.
func (p *Projects) Claim(name, repositoryID, repository string, now time.Time) (isNew bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	projects, err := p.load()
	if err != nil {
		return false, err
	}
	if owner, ok := projects[name]; ok {
		if owner.RepositoryID != repositoryID {
			return false, fmt.Errorf("%w: %q belongs to %s", ErrNameTaken, name, owner.Repository)
		}
		return false, nil
	}
	projects[name] = Project{RepositoryID: repositoryID, Repository: repository, ClaimedAt: now.UTC()}
	return true, writeJSON(p.file, projects)
}

// Get returns the owner of name, or nil if the name is free.
func (p *Projects) Get(name string) (*Project, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	projects, err := p.load()
	if err != nil {
		return nil, err
	}
	if owner, ok := projects[name]; ok {
		return &owner, nil
	}
	return nil, nil
}

func (p *Projects) load() (map[string]Project, error) {
	projects := map[string]Project{}
	if err := readJSON(p.file, &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// LoadOwners reads owners.json (owner login → numeric ID). A missing file is empty.
func LoadOwners(file string) (map[string]string, error) {
	owners := map[string]string{}
	if err := readJSON(file, &owners); err != nil {
		return nil, err
	}
	return owners, nil
}

// SaveOwners atomically writes owners.json.
func SaveOwners(file string, owners map[string]string) error {
	return writeJSON(file, owners)
}

func readJSON(file string, v any) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

func writeJSON(file string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), filepath.Base(file)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}
