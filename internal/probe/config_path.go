package probe

import (
	"errors"
	"path/filepath"
	"strings"
)

// ValidateFileHost refuses non-file backends: their actual configuration can
// differ from -config, so guessing would risk probing through the wrong exit.
// The native plugin supplies a live process-environment reader, and repeats this
// guard when reading configuration (the Host may load .env after registration).
func ValidateFileHost(args []string, getenv func(string) string) error {
	for _, key := range []string{"HOME_JWT", "PGSTORE_DSN", "OBJECTSTORE_ENDPOINT", "GITSTORE_GIT_URL"} {
		if getenv != nil && (strings.TrimSpace(getenv(key)) != "" || strings.TrimSpace(getenv(strings.ToLower(key))) != "") {
			return errors.New("automatic prewarming supports local-file CPA configuration only")
		}
	}
	_, home, err := hostFlags(args)
	if err != nil {
		return err
	}
	if home {
		return errors.New("prewarming does not support Home-managed configuration")
	}
	return nil
}

// hostFlags follows Go flag parsing: = forms, last occurrence wins, and parsing
// stops at -- or a positional argument. Unknown flags remain Host-owned.
func hostFlags(args []string) (config string, home bool, err error) {
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if arg == "--" || arg == "-" || !strings.HasPrefix(arg, "-") {
			break
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		key, value, equals := strings.Cut(name, "=")
		takesValue := false
		switch key {
		case "config", "home-jwt", "oauth-callback-port", "discover-timeout", "discover-service-type", "discover-include", "discover-exclude", "vertex-import", "vertex-import-prefix", "password":
			takesValue = true
		case "codex-login", "codex-device-login", "claude-login", "no-browser", "antigravity-login", "kimi-login", "xai-login", "devin-login", "meta-login", "discover", "discover-json", "home-disable-cluster-discovery", "tui", "standalone", "local-model":
		default:
			if !equals {
				return "", false, errors.New("unsupported Host startup flag; cannot infer file configuration safely")
			}
		}
		if takesValue && !equals {
			if i+1 >= len(args) {
				return "", false, errors.New("missing Host configuration flag value")
			}
			i++
			value = args[i]
		}
		if key != "config" && key != "home-jwt" {
			continue
		}
		if key == "home-jwt" {
			home = value != ""
			continue
		}
		if strings.TrimSpace(value) == "" {
			return "", false, errors.New("empty Host configuration path")
		}
		config = value
	}
	return config, home, nil
}

// ResolveHostConfigPath hides the legacy path knob on ordinary installations.
// Explicit overrides stay compatible, but never bypass non-file backend checks.
func ResolveHostConfigPath(explicit string, args []string, cwd string, getenv func(string) string) (string, error) {
	if err := ValidateFileHost(args, getenv); err != nil {
		return "", err
	}
	path := explicit
	if path == "" {
		var err error
		path, _, err = hostFlags(args)
		if err != nil {
			return "", err
		}
		if path == "" {
			path = "config.yaml"
		}
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return "", errors.New("cannot locate Host configuration; set host_config_file explicitly")
		}
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path), nil
}
