package scanner

import (
	"testing"
)

func TestDefaultExcludes(t *testing.T) {
	cases := []struct {
		path    string
		wantHit bool
	}{
		{".git", true},
		{".git/config", true},
		{"node_modules", true},
		{"node_modules/lodash/index.js", true},
		{"src/node_modules/foo", true},
		{".DS_Store", true},
		{"__pycache__/foo.pyc", true},
		{".venv/lib/python3.11", true},
		{"target/release/binary", true},
		{"dist/bundle.js", true},
		{".idea/workspace.xml", true},
		{".vscode/settings.json", true},
		{"src/main.go", false},
		{"README.md", false},
		{"internal/db/db.go", false},
	}

	m, err := NewMatcher(nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		got := m.ShouldSkipRel(tc.path)
		if got != tc.wantHit {
			t.Errorf("ShouldSkipRel(%q) = %v, want %v", tc.path, got, tc.wantHit)
		}
	}
}

func TestNoDefaultExcludes(t *testing.T) {
	m, err := NewMatcher(nil, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.ShouldSkipRel(".git") {
		t.Error("expected .git NOT excluded when noDefaults=true and no user patterns")
	}
}

func TestUserPatternAdded(t *testing.T) {
	m, err := NewMatcher([]string{`\.log$`}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ShouldSkipRel("logs/app.log") {
		t.Error("expected .log file to be excluded by user pattern")
	}
}

func TestInvalidPatternError(t *testing.T) {
	_, err := NewMatcher([]string{`[invalid`}, false, nil)
	if err == nil {
		t.Fatal("expected error for invalid regex pattern")
	}
}

func TestAbsExclusion(t *testing.T) {
	m, err := NewMatcher(nil, false, []string{"/tmp/db.sqlite"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.ShouldSkipAbs("/tmp/db.sqlite") {
		t.Error("expected /tmp/db.sqlite to be excluded")
	}
	if m.ShouldSkipAbs("/tmp/other.sqlite") {
		t.Error("expected /tmp/other.sqlite NOT excluded")
	}
}
