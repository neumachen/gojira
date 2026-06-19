package buildinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestUnstampedDefaults locks in the un-stamped (development) reporting
// contract: until scripts/set_version.sh rewrites the package consts, the
// four exported functions must report the "dev" fallback shape.
func TestUnstampedDefaults(t *testing.T) {
	assert.Equal(t, "dev", Revision(), "Revision() must be the un-stamped commit default")
	assert.Equal(t, "dev", Version(), "Version() must fall back to commit when ref is empty")
	assert.Equal(t, "dev", FullVersion(), "FullVersion() must omit the @ separator when ref is empty")
	assert.Equal(t, "gojira/dev", UserAgent(), "UserAgent() must be gojira/<Version()>")
}

// TestVersionOf exercises the pure helper behind Version across the
// stamped/un-stamped branches. Because the exported Version reads
// package-level consts directly, this is where the branch coverage lives.
func TestVersionOf(t *testing.T) {
	assert.Equal(t, "dev", versionOf("", "dev"),
		"empty ref must fall back to commit")
	assert.Equal(t, "abc1234", versionOf("", "abc1234"),
		"empty ref must fall back to commit verbatim")
	assert.Equal(t, "v0.3.0", versionOf("v0.3.0", "abc1234"),
		"non-empty ref must take precedence over commit")
	assert.Equal(t, "main", versionOf("main", "abc1234"),
		"ref may be a branch name, not only a tag")
}

// TestFullVersionOf exercises the pure helper behind FullVersion. The
// stamped form is "<ref>@<commit>"; the un-stamped form is just "<commit>"
// with no separator.
func TestFullVersionOf(t *testing.T) {
	assert.Equal(t, "dev", fullVersionOf("", "dev"),
		"empty ref must produce bare commit with no @ separator")
	assert.Equal(t, "abc1234", fullVersionOf("", "abc1234"),
		"empty ref must produce bare commit verbatim")
	assert.Equal(t, "v0.3.0@abc1234", fullVersionOf("v0.3.0", "abc1234"),
		"stamped build must join ref and commit with @")
	assert.Equal(t, "main@abc1234", fullVersionOf("main", "abc1234"),
		"branch ref must also be joined with @")
}
