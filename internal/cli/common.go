package cli

import (
	"fmt"
	"path/filepath"

	"github.com/RoninForge/budgetclaw/internal/paths"
)

// configPath returns the absolute path of config.toml under the
// user's XDG config directory. Honors XDG_CONFIG_HOME.
func configPath() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config dir: %w", err)
	}
	return filepath.Join(dir, "config.toml"), nil
}
