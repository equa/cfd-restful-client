package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const configBasename = "cfd_client_config.json"

// findConfig locates the config file, mirroring the Python search order:
// explicit path, then $CFD_CLIENT_CONFIG, then next to the executable, then CWD.
// Returns "" if none exists.
func findConfig(explicit string) string {
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if env := os.Getenv("CFD_CLIENT_CONFIG"); env != "" {
		candidates = append(candidates, env)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), configBasename))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, configBasename))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// loadConfig resolves the backend to use from the JSON config file, working on
// generic maps just like the Python original so the `config` dump and the
// selection rules match exactly. Returns the chosen backend map, tagged with
// the resolved "backend" name and with server_url applied from the override.
func loadConfig(explicit, serverURLOverride, host string) (map[string]any, error) {
	cfg := map[string]any{}
	path := findConfig(explicit)
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("cannot read config %s: %v", path, err)
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("cannot read config %s: %v", path, err)
		}
	}

	backends, _ := cfg["backends"].(map[string]any)
	hostTag := host
	if hostTag == "" {
		hostTag, _ = cfg["default_backend"].(string)
	}

	var backend map[string]any
	if be, ok := backends[hostTag].(map[string]any); ok && hostTag != "" {
		backend = map[string]any{}
		for k, v := range be {
			backend[k] = v
		}
	} else if serverURLOverride != "" {
		// No resolvable backend, but an explicit URL was given -- run with it.
		backend = map[string]any{}
	} else if hostTag != "" {
		avail := backendNames(backends)
		return nil, fmt.Errorf(
			"unknown backend '%s': not in the config's backends (%s); "+
				"pass --host <tag> or --server-url", hostTag, avail)
	} else {
		loc := path
		if loc == "" {
			loc = configBasename
		}
		return nil, fmt.Errorf(
			"no backend selected: set 'default_backend' in the config, or pass "+
				"--host <tag> or --server-url (%s)", loc)
	}

	if serverURLOverride != "" {
		backend["server_url"] = serverURLOverride
	}
	if s, _ := backend["server_url"].(string); s == "" {
		return nil, fmt.Errorf("backend '%s' has no 'server_url'", hostTag)
	}
	backend["backend"] = hostTag
	return backend, nil
}

func backendNames(backends map[string]any) string {
	if len(backends) == 0 {
		return "(none)"
	}
	names := make([]string, 0, len(backends))
	for k := range backends {
		names = append(names, k)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
