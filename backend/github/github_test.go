// Test github filesystem interface
package github_test

import (
	"testing"

	"github.com/rclone/rclone/backend/github"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestIntegration runs integration tests against the remote.
//
// Requires a real repository configured as TestGithub: and write access
// to a branch (ref). The tests create real commits in the repository,
// so a dedicated throwaway repository should be used.
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestGithub:",
		NilObject:  (*github.Object)(nil),
	})
}
