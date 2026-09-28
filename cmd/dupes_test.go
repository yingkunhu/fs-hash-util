package cmd

import (
	"path/filepath"
	"reflect"
	"testing"
)

func p(parts ...string) string { return filepath.Join(parts...) }

func TestIsStrictDescendant(t *testing.T) {
	cases := []struct {
		child, ancestor string
		want            bool
	}{
		{p("/a/b/c"), p("/a/b"), true},
		{p("/a/b"), p("/a/b"), false},       // identical is not a strict descendant
		{p("/a/b"), p("/a/b/c"), false},      // parent is not a descendant of child
		{p("/a/foo"), p("/a/fo"), false},     // boundary: prefix string but not path segment
		{p("/x/中医按摩2"), p("/x/中医按摩"), false}, // unicode boundary
		{p("/x/中医按摩/中医四大基础"), p("/x/中医按摩"), true},
	}
	for _, c := range cases {
		if got := isStrictDescendant(c.child, c.ancestor); got != c.want {
			t.Errorf("isStrictDescendant(%q,%q)=%v want %v", c.child, c.ancestor, got, c.want)
		}
	}
}

// TestSuppressNestedGroupsRealCase mirrors the reported bug: a sub-folder group
// nested entirely under an already-reported parent group must be dropped.
func TestSuppressNestedGroupsRealCase(t *testing.T) {
	base := p("/home/bbt/materials")
	parent := sizedGroup{
		hash: "parent", size: 41_000_000_000,
		paths: []string{
			p(base, "Materials_Main/Other/Art.Of.Healing/中医按摩"),
			p(base, "Other/Art.Of.Healing/中医按摩"),
		},
	}
	// Child paths are listed in the opposite order to prove coverage is
	// order-independent.
	child := sizedGroup{
		hash: "child", size: 26_000_000_000,
		paths: []string{
			p(base, "Other/Art.Of.Healing/中医按摩/中医四大基础"),
			p(base, "Materials_Main/Other/Art.Of.Healing/中医按摩/中医四大基础"),
		},
	}

	got := suppressNestedGroups([]sizedGroup{parent, child})
	if len(got) != 1 || got[0].hash != "parent" {
		t.Fatalf("expected only parent kept, got %d groups: %+v", len(got), got)
	}
}

// TestSuppressNestedGroupsStrict verifies the strict rule: a group is only
// suppressed when ALL its paths are covered by a SINGLE ancestor group. A group
// whose paths are covered by different ancestor groups must be kept.
func TestSuppressNestedGroupsStrict(t *testing.T) {
	a := sizedGroup{hash: "a", size: 100, paths: []string{p("/root/A"), p("/root/B")}}
	c := sizedGroup{hash: "c", size: 50, paths: []string{p("/root/C"), p("/root/D")}}
	// crossed: one path under A, the other under C -> not covered by a single group.
	crossed := sizedGroup{hash: "crossed", size: 10, paths: []string{p("/root/A/x"), p("/root/C/y")}}

	got := suppressNestedGroups([]sizedGroup{a, c, crossed})
	gotHashes := make([]string, len(got))
	for i, g := range got {
		gotHashes[i] = g.hash
	}
	want := []string{"a", "c", "crossed"}
	if !reflect.DeepEqual(gotHashes, want) {
		t.Fatalf("crossed group must survive strict rule; kept=%v want=%v", gotHashes, want)
	}
}

// TestSuppressNestedGroupsPartialOverlapKept ensures a group is NOT suppressed
// when only some of its paths are nested under an ancestor.
func TestSuppressNestedGroupsPartialOverlapKept(t *testing.T) {
	parent := sizedGroup{hash: "parent", size: 100, paths: []string{p("/root/A"), p("/root/B")}}
	partial := sizedGroup{hash: "partial", size: 10, paths: []string{p("/root/A/x"), p("/elsewhere/y")}}

	got := suppressNestedGroups([]sizedGroup{parent, partial})
	if len(got) != 2 {
		t.Fatalf("partial-overlap group must be kept; got %d groups: %+v", len(got), got)
	}
}
