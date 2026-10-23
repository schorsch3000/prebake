package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Slot is a daily time window "HH:MM-HH:MM" in local time. End may be before
// Start, meaning the window crosses midnight (e.g. "22:00-02:00").
type Slot struct {
	Start, End time.Duration // offset from local midnight
}

func (s Slot) String() string { return fmtOffset(s.Start) + "-" + fmtOffset(s.End) }

func fmtOffset(d time.Duration) string {
	m := int(d / time.Minute)
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

func parseOffset(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

func ParseSlot(s string) (Slot, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Slot{}, fmt.Errorf("invalid slot %q, want HH:MM-HH:MM", s)
	}
	start, err := parseOffset(a)
	if err != nil {
		return Slot{}, err
	}
	end, err := parseOffset(b)
	if err != nil {
		return Slot{}, err
	}
	if start == end {
		return Slot{}, fmt.Errorf("slot %q has zero length", s)
	}
	return Slot{Start: start, End: end}, nil
}

// Length is the window duration, accounting for midnight crossing.
func (s Slot) Length() time.Duration {
	if s.End > s.Start {
		return s.End - s.Start
	}
	return 24*time.Hour - s.Start + s.End
}

func (s *Slot) UnmarshalYAML(n *yaml.Node) error {
	var str string
	if err := n.Decode(&str); err != nil {
		return err
	}
	v, err := ParseSlot(str)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*s = v
	return nil
}
