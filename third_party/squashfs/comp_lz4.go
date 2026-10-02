//go:build lz4

package squashfs

import (
	"sync"

	"github.com/pierrec/lz4/v4"
)

// ponytail: 1 MiB scratch per call from a pool; size it from the superblock if metadata-heavy walks show it in profiles.
var lz4Scratch = sync.Pool{New: func() any { b := make([]byte, 1<<20); return &b }}

// squashfs stores raw LZ4 blocks (no frame header), so the frame reader won't do.
func lz4Decompress(buf []byte) ([]byte, error) {
	p := lz4Scratch.Get().(*[]byte)
	defer lz4Scratch.Put(p)
	n, err := lz4.UncompressBlock(buf, *p)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), (*p)[:n]...), nil
}

func lz4Compress(buf []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(buf)))
	n, err := lz4.CompressBlock(buf, dst, nil)
	if err != nil || n == 0 { // n == 0: incompressible; nil makes the writer store it raw
		return nil, err
	}
	return dst[:n], nil
}

func init() {
	RegisterCompHandler(LZ4, &CompHandler{
		Decompress: lz4Decompress,
		Compress:   lz4Compress,
	})
}
