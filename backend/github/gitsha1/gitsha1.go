// Package gitsha1 implements the git blob object hash.
//
// Git stores files as blob objects whose identifier is the SHA-1 of
// the header "blob <size>\x00" followed by the raw content. This is
// the sha value returned by the GitHub contents and trees APIs, so it
// can be verified locally without downloading the file.
//
// Because the header contains the total size, the content length must
// be known before hashing starts. Reset sets the size back to
// unknown; the first Write or a Sum on an empty hash then computes
// the header from the content accumulated so far.
package gitsha1

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"hash"
)

const (
	// Size of a git blob hash in bytes (SHA-1)
	Size = sha1.Size
	// BlockSize of the hash in bytes (SHA-1)
	BlockSize = sha1.BlockSize
)

type digest struct {
	sha1       hash.Hash // the underlying hash, created lazily
	size       int64     // total content size, -1 when unknown
	written    int64     // content bytes written before the header was emitted
	headerDone bool      // whether the header has been written to sha1
	buf        []byte    // content buffered while size is unknown
}

// New returns a hash.Hash computing the git blob hash with unknown
// content size. If the total size is known up front, prefer NewSized
// which avoids buffering.
func New() hash.Hash {
	return &digest{size: -1}
}

// NewSized returns a hash.Hash computing the git blob hash for
// content of the given size
func NewSized(size int64) hash.Hash {
	d := &digest{size: size}
	d.writeHeader(size)
	return d
}

// SumData returns the git blob hash of data as a hex string
func SumData(data []byte) string {
	h := NewSized(int64(len(data)))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// Sum returns the raw checksum
func (d *digest) Sum(b []byte) []byte {
	d.finish()
	return d.sha1.Sum(b)
}

func (d *digest) Write(p []byte) (n int, err error) {
	if !d.headerDone && d.size < 0 {
		// Size unknown: buffer the content and defer the header
		d.buf = append(d.buf, p...)
		return len(p), nil
	}
	if !d.headerDone {
		d.writeHeader(d.size + d.written)
	}
	d.written += int64(len(p))
	return d.sha1.Write(p)
}

func (d *digest) Reset() {
	d.sha1 = nil
	d.size = -1
	d.written = 0
	d.headerDone = false
	d.buf = d.buf[:0]
}

func (d *digest) Size() int { return Size }

func (d *digest) BlockSize() int { return BlockSize }

// finish emits the header (from the buffered size if it was unknown)
// and flushes any buffered content
func (d *digest) finish() {
	if d.headerDone {
		return
	}
	if d.size < 0 {
		d.size = int64(len(d.buf))
	}
	d.writeHeader(d.size)
	if len(d.buf) > 0 {
		d.sha1.Write(d.buf)
		d.buf = nil
	}
}

func (d *digest) writeHeader(size int64) {
	if d.sha1 == nil {
		d.sha1 = sha1.New()
	}
	// 头部写入不会失败(内存哈希),忽略错误仅为了满足 errcheck
	_, _ = fmt.Fprintf(d.sha1, "blob %d\x00", size)
	d.headerDone = true
}
