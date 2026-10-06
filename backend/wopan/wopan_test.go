// Test WoPan filesystem interface
package wopan_test

import (
	"testing"

	"github.com/rclone/rclone/backend/wopan"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestIntegration runs integration tests against the remote
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: "TestWopan:",
		NilObject:  (*wopan.Object)(nil),
	})
}
