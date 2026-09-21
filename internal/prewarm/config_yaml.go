package prewarm

import (
	"errors"

	"gopkg.in/yaml.v3"
)

// Reject misspelled selectors rather than silently warming every account.
// Inspect merge/alias mappings too, while leaving YAML value decoding to yaml.v3.
// Neither field values nor parser excerpts are included in returned errors.
func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	known := map[string]bool{}
	for _, key := range []string{"enabled", "models", "max_probes_per_hour", "proxies", "accounts", "proxy_file", "host_config_file", "workers", "max_attempts", "attempt_interval_seconds", "cooldown_seconds", "ttl_seconds", "refresh_before_seconds", "probe_timeout_seconds", "max_pending"} {
		known[key] = true
	}
	seen := map[*yaml.Node]bool{}
	var check func(*yaml.Node) error
	check = func(n *yaml.Node) error {
		if n == nil {
			return errors.New("invalid prewarm configuration")
		}
		if seen[n] {
			return nil
		}
		seen[n] = true
		if n.Kind == yaml.AliasNode {
			return check(n.Alias)
		}
		if n.Kind == yaml.SequenceNode {
			for _, child := range n.Content {
				if err := check(child); err != nil {
					return err
				}
			}
			return nil
		}
		if n.Kind != yaml.MappingNode {
			return errors.New("prewarm configuration must be a mapping")
		}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if key == "<<" {
				if err := check(n.Content[i+1]); err != nil {
					return err
				}
				continue
			}
			if !known[key] {
				return errors.New("unknown prewarm configuration field")
			}
		}
		return nil
	}
	if node.Kind != yaml.MappingNode && node.Kind != yaml.AliasNode {
		return errors.New("prewarm configuration must be a mapping")
	}
	if err := check(node); err != nil {
		return err
	}
	type plain Config
	var value plain
	if err := node.Decode(&value); err != nil {
		return errors.New("invalid prewarm configuration value")
	}
	*c = Config(value)
	return nil
}
