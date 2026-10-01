package scanner

import (
	"fmt"
	"regexp"
)

// DefaultExcludes is the list of built-in regex patterns applied to slash-normalized relative paths.
// Any path matching at least one pattern is excluded. Directories matching a pattern are pruned (subtree skipped).
//
// NOTE: the move command treats the *directory* entries here as atomic units (see
// atomicDirNames in cmd/move.go). Keep the two lists in sync when adding directory excludes.
var DefaultExcludes = []string{
	`(^|/)\.git(/|$)`,
	`(^|/)node_modules(/|$)`,
	`(^|/)\.svn(/|$)`,
	`(^|/)\.hg(/|$)`,
	`(^|/)\.DS_Store$`,
	`(^|/)Thumbs\.db$`,
	`(^|/)__pycache__(/|$)`,
	`(^|/)\.venv(/|$)`,
	`(^|/)target(/|$)`,
	`(^|/)dist(/|$)`,
	`(^|/)\.idea(/|$)`,
	`(^|/)\.vscode(/|$)`,
}

// Matcher holds compiled exclude regexes and a set of absolute paths to skip unconditionally.
type Matcher struct {
	patterns        []*regexp.Regexp
	absExcludePaths map[string]struct{}
}

// NewMatcher builds a Matcher from the given patterns and absolute-path exclusions.
// If noDefaults is false, DefaultExcludes are prepended.
// Returns an error (and exits) if any pattern fails to compile.
func NewMatcher(userPatterns []string, noDefaults bool, absExcludePaths []string) (*Matcher, error) {
	var src []string
	if !noDefaults {
		src = append(src, DefaultExcludes...)
	}
	src = append(src, userPatterns...)

	compiled := make([]*regexp.Regexp, 0, len(src))
	for _, p := range src {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}

	absSet := make(map[string]struct{}, len(absExcludePaths))
	for _, p := range absExcludePaths {
		absSet[p] = struct{}{}
	}

	return &Matcher{patterns: compiled, absExcludePaths: absSet}, nil
}

// ShouldSkipRel reports whether the slash-normalized relative path should be excluded.
func (m *Matcher) ShouldSkipRel(relPath string) bool {
	for _, re := range m.patterns {
		if re.MatchString(relPath) {
			return true
		}
	}
	return false
}

// ShouldSkipAbs reports whether the absolute path is in the unconditional exclusion set.
func (m *Matcher) ShouldSkipAbs(absPath string) bool {
	_, ok := m.absExcludePaths[absPath]
	return ok
}
