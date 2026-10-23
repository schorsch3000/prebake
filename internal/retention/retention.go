// Package retention decides which date tags to delete from the registry
// (docs/design.md §10). It is pure: it only looks at tag names.
package retention

import (
	"regexp"
	"sort"
)

// Date tags look like "<flavor>-<YYYYmmdd>-<HHMMSS>" (UTC). The flavor may
// itself contain dashes. Aliases ("latest", "cuda") and anything else do not
// match and are never touched.
var dateTag = regexp.MustCompile(`^(.+)-(\d{8}-\d{6})$`)

// Parse splits a date tag into flavor and timestamp part; ok is false for any
// other tag.
func Parse(tag string) (flavor, stamp string, ok bool) {
	m := dateTag.FindStringSubmatch(tag)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// Plan returns the tags to delete from one repository, oldest first.
//
// Per flavor the newest keep date tags survive. Tags in protected (the date
// tags the aliases currently point to) survive even if they are not among the
// newest, so Plan may leave more than keep tags for a flavor.
// Non-date tags are ignored. keep < 1 deletes nothing (a safety net; the
// validator already requires keep >= 1).
func Plan(tags []string, keep int, protected map[string]bool) []string {
	if keep < 1 {
		return nil
	}
	byFlavor := map[string][]string{} // flavor -> date tags
	for _, t := range tags {
		if f, _, ok := Parse(t); ok {
			byFlavor[f] = append(byFlavor[f], t)
		}
	}
	var del []string
	for _, ts := range byFlavor {
		// the stamp is fixed-width and sorts chronologically
		sort.Slice(ts, func(i, j int) bool {
			_, a, _ := Parse(ts[i])
			_, b, _ := Parse(ts[j])
			return a > b
		})
		for i, t := range ts {
			if i < keep || protected[t] {
				continue
			}
			del = append(del, t)
		}
	}
	sort.Slice(del, func(i, j int) bool {
		_, a, _ := Parse(del[i])
		_, b, _ := Parse(del[j])
		if a != b {
			return a < b
		}
		return del[i] < del[j]
	})
	return del
}
