package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that also accepts "d" (day) and "w" (week) suffixes,
// e.g. "7d", "12h", "1w", "1d12h".
type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	rest := s
	// peel leading "<n>d" / "<n>w" parts, hand the remainder to time.ParseDuration
	for {
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(rest) || (rest[i] != 'd' && rest[i] != 'w') {
			break
		}
		n, err := strconv.Atoi(rest[:i])
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		unit := 24 * time.Hour
		if rest[i] == 'w' {
			unit = 7 * 24 * time.Hour
		}
		total += time.Duration(n) * unit
		rest = rest[i+1:]
	}
	if rest != "" {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		total += d
	}
	if total <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return total, nil
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}
