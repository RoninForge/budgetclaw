package goei

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// PointerFileName is the committed, non-secret team pointer a Goei team lead drops in
// a repo root. It carries only routing info plus a revocable join code, never a device
// token. Its presence is what lets `budgetclaw team join` (and the discovery line) know
// a team dashboard exists for this repo.
const PointerFileName = ".budgetclaw.toml"

// RepoPointer is a parsed [goei] pointer section from a committed .budgetclaw.toml.
type RepoPointer struct {
	Team     string
	Endpoint string
	JoinCode string
	Mode     string
	prompts  *bool
	Path     string // absolute path of the file it was read from
}

type pointerTOML struct {
	Goei struct {
		Team     string `toml:"team"`
		Endpoint string `toml:"endpoint"`
		JoinCode string `toml:"join_code"`
		Mode     string `toml:"mode"`
		Prompts  *bool  `toml:"prompts"`
	} `toml:"goei"`
}

// PromptsEnabled reports whether the discovery line may render for this repo. An absent
// key means yes; `[goei] prompts = false` in the pointer silences it forever.
func (p *RepoPointer) PromptsEnabled() bool {
	return p.prompts == nil || *p.prompts
}

// ParsePointer decodes a pointer file's bytes. It returns (nil, nil) when the file
// carries no [goei] join_code, so an unrelated .budgetclaw.toml (for instance a
// hand-written local config that happens to sit in a repo) is not mistaken for a
// team pointer.
func ParsePointer(data []byte, path string) (*RepoPointer, error) {
	var t pointerTOML
	if err := toml.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t.Goei.JoinCode == "" {
		return nil, nil
	}
	return &RepoPointer{
		Team:     t.Goei.Team,
		Endpoint: t.Goei.Endpoint,
		JoinCode: t.Goei.JoinCode,
		Mode:     t.Goei.Mode,
		prompts:  t.Goei.Prompts,
		Path:     path,
	}, nil
}

// FindRepoPointer walks up from startDir looking for a .budgetclaw.toml that carries a
// [goei] join_code, so a teammate can run `budgetclaw team join` from anywhere inside
// the repo, not only its root. The walk stops at the repo root (the first directory
// containing a .git entry): a pointer belongs to a specific repository, so a stray
// .budgetclaw.toml sitting in a shared parent (a home directory, a CI runner) must not
// be picked up by unrelated repos nested beneath it. Returns (nil, nil) when no pointer
// is found within the repo (or before the filesystem root when startDir is not in a
// repo). A malformed pointer file surfaces its parse error.
func FindRepoPointer(startDir string) (*RepoPointer, error) {
	dir := startDir
	for {
		candidate := filepath.Join(dir, PointerFileName)
		if data, err := os.ReadFile(candidate); err == nil {
			p, perr := ParsePointer(data, candidate)
			if perr != nil {
				return nil, perr
			}
			if p != nil {
				return p, nil
			}
		}
		// Stop at the repo root: a pointer above it belongs to some other repo.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return nil, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, nil // reached the filesystem root
		}
		dir = parent
	}
}
