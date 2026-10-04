package sevenzip

// Backup-Mounter addition (the only file not from upstream v1.6.5): exposes
// folder streams so a caller can keep its own decoder per solid folder.

import (
	"errors"
	"io"
)

// NumFolders returns the number of folders (solid blocks) in the archive.
func (z *Reader) NumFolders() int { return z.si.Folders() }

// FolderSize returns the unpacked size of folder i.
func (z *Reader) FolderSize(i int) int64 {
	return int64(z.si.unpackInfo.folder[i].unpackSize()) //nolint:gosec
}

// FolderReader returns the unpacked stream of folder i: forward-only Seek, Size() = unpacked size.
func (z *Reader) FolderReader(i int) (interface {
	io.ReadSeekCloser
	Size() int64
}, error) {
	fr, _, encrypted, err := z.folderReader(z.si, i)
	if err == nil && encrypted && z.p == "" {
		err = errors.New("encrypted, no password given")
	}
	if err != nil {
		return nil, &ReadError{Encrypted: encrypted, Err: err}
	}
	return fr, nil
}

// Folder reports which folder holds f's data and the byte offset inside that
// folder's unpacked stream; ok=false for directories and empty files.
func (f *File) Folder() (folder int, offset int64, ok bool) {
	return f.folder, f.offset, !f.isEmptyStream && !f.isEmptyFile
}
