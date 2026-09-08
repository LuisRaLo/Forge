// Package yamlx holds YAML decoding helpers shared by the configuration and
// agent-definition loaders. It exists so that the domain package can stay free
// of serialisation concerns.
package yamlx

import (
	"fmt"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration decodes Go duration strings such as "30m" or "5s" from YAML. A bare
// number is accepted and read as seconds, since that is the only unit a
// unit-less duration could reasonably mean in this configuration.
type Duration time.Duration

// UnmarshalYAML decodes a duration scalar.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("line %d: expected a duration such as \"30m\": %w", node.Line, err)
	}

	if parsed, err := time.ParseDuration(s); err == nil {
		*d = Duration(parsed)
		return nil
	}

	// yaml.v3 happily decodes the scalar 90 into the string "90", so the
	// unit-less case is handled here rather than by a second Decode.
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}

	return fmt.Errorf(
		"line %d: invalid duration %q: use a unit such as \"30m\", or a bare number of seconds",
		node.Line, s)
}

// MarshalYAML renders the duration back as a string.
func (d Duration) MarshalYAML() (any, error) { return d.Duration().String(), nil }

// Duration converts to the standard library type.
func (d Duration) Duration() time.Duration { return time.Duration(d) }
