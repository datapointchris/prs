package board

import "testing"

// A comment's own headings must sit under the heading that names the comment, or the
// Reader's path and its section jumps read them as the comment's siblings.
func TestNestedAtMovesEveryHeadingUnderTheSectionItIsWrittenIn(t *testing.T) {
	cases := []struct{ source, want string }{
		{"# Findings\n\ntext\n\n> ## Quoted\n\n```\n# not a heading\n```", "#### Findings\n\ntext\n\n> ##### Quoted\n\n```\n# not a heading\n```"},
		{"Thanks, two notes on the cache\nexpiry before this merges.\n---\n\n# Findings", "##### Thanks, two notes on the cache expiry before this merges.\n\n#### Findings"},
		{"#\n\ntext", "####\n\ntext"},
		{"#### Deep enough\n\nbody", "#### Deep enough\n\nbody"},
	}
	for _, c := range cases {
		if got := nestedAt(c.source, 4); got != c.want {
			t.Errorf("nestedAt(%q, 4) = %q, want %q", c.source, got, c.want)
		}
	}
}
