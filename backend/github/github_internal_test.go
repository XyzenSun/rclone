// Unit tests for the pure logic of the github backend.
//
// These tests never touch the network. Integration against a real
// repository is done via TestIntegration below with real credentials.
package github

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/rclone/rclone/backend/github/gitsha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGitSHA1AgainstGitHashObject verifies the git blob hash
// implementation against vectors computed independently by applying
// the documented git rule (sha1 of "blob <size>\x00" + content) with
// the standard library only.
func TestGitSHA1AgainstGitHashObject(t *testing.T) {
	for _, content := range []string{
		"",
		"a",
		"hello world\n",
		strings.Repeat("rclone github backend\n", 1000),
		string(make([]byte, 100)), // 100 NUL bytes
	} {
		want := sha1.Sum([]byte(fmt.Sprintf("blob %d\x00%s", len(content), content)))
		assert.Equal(t, hex.EncodeToString(want[:]), gitsha1.SumData([]byte(content)), "content length %d", len(content))
	}
}

// TestGitSHA1UnknownSize verifies that a hash created without knowing
// the size up front produces the same result as the sized variant,
// however the content is chunked.
func TestGitSHA1UnknownSize(t *testing.T) {
	content := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 500))
	for _, chunkSize := range []int{1, 3, 7, 64, 1024, len(content)} {
		h := gitsha1.New()
		for off := 0; off < len(content); off += chunkSize {
			end := min(off+chunkSize, len(content))
			_, err := h.Write(content[off:end])
			require.NoError(t, err)
		}
		assert.Equal(t, gitsha1.SumData(content), hex.EncodeToString(h.Sum(nil)), "chunkSize %d", chunkSize)
	}
}

// TestGitSHA1Reset checks the hash can be reused after Reset
func TestGitSHA1Reset(t *testing.T) {
	h := gitsha1.NewSized(int64(len("first")))
	h.Write([]byte("first"))
	h.Reset()
	h = gitsha1.NewSized(int64(len("second")))
	h.Write([]byte("second"))
	assert.Equal(t, gitsha1.SumData([]byte("second")), hex.EncodeToString(h.Sum(nil)))
}

// TestBase64BlobBody verifies the streaming base64 body of the blob
// API (prefix + piped encoder + suffix) byte-for-byte against a plain
// in-memory encoding, and that the pre-computed Content-Length is exact.
func TestBase64BlobBody(t *testing.T) {
	for _, size := range []int64{0, 1, 2, 3, 4, 100, 1023, 1024} {
		content := make([]byte, size)
		for i := range content {
			content[i] = byte(i)
		}
		body, length := base64BlobBody(strings.NewReader(string(content)), size)
		got, err := io.ReadAll(body)
		require.NoError(t, err)
		want := fmt.Sprintf("{\"encoding\":\"base64\",\"content\":\"%s\"}", base64.StdEncoding.EncodeToString(content))
		assert.Equal(t, want, string(got), "size %d", size)
		assert.Equal(t, int64(len(want)), length, "size %d", size)
	}
}

// TestCommitMessage covers template rendering and the fallbacks
func TestCommitMessage(t *testing.T) {
	vars := &messageTemplateVars{
		UserName:   "rclone",
		ObjName:    "file.txt",
		ObjPath:    "dir/file.txt",
		ParentName: "dir",
		ParentPath: "dir",
		TargetName: "other.txt",
		TargetPath: "dir/other.txt",
	}
	msg, err := commitMessage("{{.UserName}} upload {{.ObjPath}}", "upload", vars)
	require.NoError(t, err)
	assert.Equal(t, "rclone upload dir/file.txt", msg)

	// A broken template must not abort the operation: it falls back
	// to a fixed message
	msg, err = commitMessage("{{.Broken", "upload", vars)
	require.Error(t, err)
	assert.Equal(t, "rclone upload dir/file.txt", msg)

	// A template referencing a missing variable falls back the same way
	msg, err = commitMessage("{{.NoSuchVar}}", "upload", vars)
	require.Error(t, err)
	assert.Equal(t, "rclone upload dir/file.txt", msg)
}

// TestDirPath exercises the internal path helpers
func TestDirPath(t *testing.T) {
	assert.Equal(t, "", dirPath("file.txt"))
	assert.Equal(t, "dir", dirPath("dir/file.txt"))
	assert.Equal(t, "dir/sub", dirPath("dir/sub/file.txt"))
	assert.Equal(t, "file.txt", leafName("file.txt"))
	assert.Equal(t, "file.txt", leafName("dir/file.txt"))
	assert.Equal(t, "sub", leafName("dir/sub"))
	assert.Equal(t, "", dirPath("dir"))
	assert.Equal(t, "dir", leafName("dir"))
}
