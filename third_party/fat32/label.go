package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// MaxLabelLen is the on-disk size of the FAT32 volume label (BS_VolLab,
// boot-sector offset 71, 11 bytes). The historic FAT convention is to
// space-pad shorter labels and to keep the label uppercase ASCII; we
// preserve case and pad with spaces.
const MaxLabelLen = 11

// Boot-sector offsets of fields we touch here.
const (
	bsOffVolLab        = 71  // 11-byte label
	bsOffBackupSector  = 50  // uint16 backup-boot-sector index (sectors)
	bsOffBytesPerSect  = 11  // uint16
	bsOffBootSignature = 510 // [2]byte, must be 0x55 0xAA
)

// Compile-time assertion: fat32FS implements filesystem.Labeller.
var _ filesystem.Labeller = (*fat32FS)(nil)

// Label returns the current volume label, decoded from the BPB. Trailing
// spaces / NULs are trimmed.
func (fs *fat32FS) Label() string {
	return fs.info.VolumeLabel
}

// SetLabel writes a new volume label into the primary boot sector and
// the backup boot sector (if BPB.BackupBootSector is non-zero). The
// label is capped at 11 bytes and space-padded — the FAT convention.
// The in-memory Info is refreshed so subsequent Label() calls observe
// the new value without a reopen.
func (fs *fat32FS) SetLabel(label string) error {
	b := []byte(label)
	if len(b) > MaxLabelLen {
		return fmt.Errorf("fat32: label %q is %d bytes, exceeds maximum %d", label, len(b), MaxLabelLen)
	}
	// Pad with spaces to exactly 11 bytes.
	padded := make([]byte, MaxLabelLen)
	for i := range padded {
		padded[i] = ' '
	}
	copy(padded, b)

	if err := fs.writeLabelAt(padded, fs.partOffset); err != nil {
		return err
	}
	// Backup boot sector, if recorded.
	if fs.info.BackupBootSector != 0 {
		backupOff := fs.partOffset + int64(fs.info.BackupBootSector)*int64(fs.info.BytesPerSector)
		if err := fs.writeLabelAt(padded, backupOff); err != nil {
			return fmt.Errorf("fat32 SetLabel: backup boot sector: %w", err)
		}
	}

	// Keep the root-directory volume-label entry in sync with the BPB label.
	// Canonical fsck tools require the two to agree (a BPB label with no root
	// entry, or a stale root entry, is flagged as an error).
	if err := fs.syncVolumeLabelEntry(padded); err != nil {
		return fmt.Errorf("fat32 SetLabel: %w", err)
	}

	fs.info.VolumeLabel = strings.TrimRight(string(padded), " \x00")
	return nil
}

// syncVolumeLabelEntry rewrites (or creates) the ATTR_VOLUME_ID entry in the
// root directory so it matches the 11-byte, space-padded label. When the label
// is all-spaces (cleared), any existing volume-label entry is deleted.
func (fs *fat32FS) syncVolumeLabelEntry(padded []byte) error {
	buf, err := fs.readDirBuf(fs.info.RootCluster)
	if err != nil {
		return err
	}

	wantEmpty := true
	for _, c := range padded {
		if c != ' ' {
			wantEmpty = false
			break
		}
	}

	// Locate an existing volume-label entry, the first free slot, and stop at
	// end-of-directory (0x00) so a new entry lands in a valid position.
	existing := -1
	freeSlot := -1
	for off := 0; off+dirEntrySize <= len(buf); off += dirEntrySize {
		b0 := buf[off]
		if b0 == 0x00 {
			if freeSlot < 0 {
				freeSlot = off
			}
			break
		}
		if b0 == 0xE5 {
			if freeSlot < 0 {
				freeSlot = off
			}
			continue
		}
		attr := buf[off+11]
		if attr != fatAttrLongName && attr&fatAttrVolumeID != 0 {
			existing = off
			break
		}
	}

	switch {
	case wantEmpty:
		if existing < 0 {
			return nil // nothing to clear
		}
		buf[existing] = 0xE5 // mark deleted
	case existing >= 0:
		copy(buf[existing:existing+11], padded)
	default:
		if freeSlot < 0 {
			return fmt.Errorf("no free slot for volume-label entry")
		}
		for i := freeSlot; i < freeSlot+dirEntrySize; i++ {
			buf[i] = 0
		}
		copy(buf[freeSlot:freeSlot+11], padded)
		buf[freeSlot+11] = fatAttrVolumeID
	}
	return fs.writeDirBuf(fs.info.RootCluster, buf)
}

// writeLabelAt rewrites the 11-byte label slot inside the boot sector
// located at absOff. The caller is responsible for choosing primary vs
// backup. We re-read the whole sector first so unrelated fields and the
// boot signature are preserved.
func (fs *fat32FS) writeLabelAt(label []byte, absOff int64) error {
	buf := make([]byte, fs.info.BytesPerSector)
	if _, err := fs.f.ReadAt(buf, absOff); err != nil {
		return fmt.Errorf("fat32 SetLabel: read boot sector at 0x%x: %w", absOff, err)
	}
	if buf[bsOffBootSignature] != 0x55 || buf[bsOffBootSignature+1] != 0xAA {
		return fmt.Errorf("fat32 SetLabel: bad boot signature at 0x%x", absOff)
	}
	// Sanity-check bytes-per-sector matches what we already parsed —
	// catches accidental partial writes / wrong offset.
	if binary.LittleEndian.Uint16(buf[bsOffBytesPerSect:]) != fs.info.BytesPerSector {
		return fmt.Errorf("fat32 SetLabel: bytes-per-sector mismatch at 0x%x", absOff)
	}

	copy(buf[bsOffVolLab:bsOffVolLab+MaxLabelLen], label)
	if _, err := fs.f.WriteAt(buf, absOff); err != nil {
		return fmt.Errorf("fat32 SetLabel: write boot sector at 0x%x: %w", absOff, err)
	}
	return nil
}
