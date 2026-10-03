package main

import (
	"io"
	"log"

	"github.com/klauspost/compress/zstd"
	"github.com/lima-vm/go-qcow2reader/image/qcow2"
)

// zlib (deflate) clusters are built into go-qcow2reader; zstd needs a decoder registered.
func init() {
	qcow2.SetDecompressor(qcow2.CompressionTypeZstd, func(r io.Reader) (io.ReadCloser, error) {
		d, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1)) // one cluster per decoder: no worker goroutines
		if err != nil {
			return nil, err
		}
		return zstdCloser{d}, nil
	})
}

type zstdCloser struct{ *zstd.Decoder }

func (z zstdCloser) Close() error { z.Decoder.Close(); return nil }

// openQcow2 opens a standalone qcow2 image. raw is not an *os.File, so a backing
// file can never be resolved and Readable reports "unsupported backing file".
// ponytail: no backing chains; pass an image.OpenWithType and the *os.File if overlays turn up.
func openQcow2(name string, raw io.ReaderAt) (io.ReaderAt, int64, error) {
	img, err := qcow2.Open(raw, nil)
	if err == nil {
		err = img.Readable() // backing file, encryption, unknown compression or incompatible feature
	}
	if err != nil {
		return nil, 0, err
	}
	comp := qcow2.CompressionTypeZlib
	if img.HeaderFieldsAdditional != nil {
		comp = img.CompressionType
	}
	log.Printf("%s: qcow2 v%d, %d MiB virtual, compression %s", name, img.Version, img.Size()>>20, comp)
	return img, img.Size(), nil
}
