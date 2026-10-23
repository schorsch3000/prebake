package retention

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	f, s, ok := Parse("default-20261022-020000")
	if !ok || f != "default" || s != "20261022-020000" {
		t.Errorf("%q %q %v", f, s, ok)
	}
	f, _, ok = Parse("cuda-12-dev-20261022-020000")
	if !ok || f != "cuda-12-dev" {
		t.Errorf("dashes in flavor: %q %v", f, ok)
	}
	for _, tag := range []string{"latest", "cuda", "default-20261022-020000-candidate", "20261022-020000", "x-2026-1", ""} {
		if _, _, ok := Parse(tag); ok {
			t.Errorf("%q must not parse as a date tag", tag)
		}
	}
}

func TestPlanKeepsNewestPerFlavor(t *testing.T) {
	tags := []string{
		"default-20261001-020000", "default-20261003-020000", "default-20261002-020000", "default-20261004-020000",
		"cuda-20261001-020000", "cuda-20261002-020000",
		"latest", "cuda",
	}
	got := Plan(tags, 2, nil)
	want := []string{"default-20261001-020000", "default-20261002-020000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// keep larger than what exists deletes nothing
	if got := Plan(tags, 10, nil); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestPlanDeletesOldestFirstAcrossFlavors(t *testing.T) {
	tags := []string{
		"a-20261003-000000", "a-20261001-000000", "a-20261005-000000",
		"b-20261002-000000", "b-20261004-000000", "b-20261006-000000",
	}
	got := Plan(tags, 1, nil)
	want := []string{"a-20261001-000000", "b-20261002-000000", "a-20261003-000000", "b-20261004-000000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestPlanProtectsPromoted(t *testing.T) {
	tags := []string{"default-20261001-020000", "default-20261002-020000", "default-20261003-020000"}
	got := Plan(tags, 1, map[string]bool{"default-20261001-020000": true})
	want := []string{"default-20261002-020000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	// several protected tags (aliases of different flavors or identical digests)
	got = Plan(tags, 1, map[string]bool{"default-20261001-020000": true, "default-20261002-020000": true})
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

func TestPlanSafety(t *testing.T) {
	tags := []string{"default-20261001-020000"}
	if got := Plan(tags, 0, nil); got != nil {
		t.Errorf("keep 0 must delete nothing: %v", got)
	}
	if got := Plan(tags, -3, nil); got != nil {
		t.Errorf("negative keep must delete nothing: %v", got)
	}
	if got := Plan(nil, 3, nil); len(got) != 0 {
		t.Errorf("%v", got)
	}
	// aliases and candidates are never selected
	got := Plan([]string{"latest", "cuda", "default-20261001-020000-candidate"}, 1, nil)
	if len(got) != 0 {
		t.Errorf("%v", got)
	}
}
