package filesystem_fat32

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"

	filesystem "github.com/go-filesystems/interface"
)

// FormatConfig holds optional parameters for Format.
// All fields are optional; sensible defaults are used when left at their zero value.
type FormatConfig struct {
	// Label is the volume label stored in the BPB (trimmed to 11 bytes).
	Label string
	// VolumeID is the 32-bit volume serial number. A random value is generated when zero.
	VolumeID uint32
}

// Layout parameters for a freshly formatted FAT32 volume.
const (
	fmtBytesPerSector    = 512
	fmtSectorsPerCluster = 8  // 4 KiB clusters
	fmtReservedSectors   = 32 // includes boot sector + FSInfo + backup
	fmtFATCount          = 2
	fmtFSInfoSector      = 1
	fmtBackupBootSector  = 6
	fmtRootCluster       = 2
)

type formatFile interface {
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Close() error
}

var formatOpenFile = func(path string) (formatFile, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

var formatRandUint32 = func() uint32 {
	return rand.Uint32()
}

var formatOpenFS = Open

// hasNonSpace reports whether s contains any byte other than ASCII space,
// i.e. whether the (space-padded) label carries an actual volume name.
func hasNonSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			return true
		}
	}
	return false
}

// Format creates a new FAT32 filesystem in the file at path.
// The file is created (or truncated) and formatted. sizeBytes must be a
// multiple of the cluster size (4096) and large enough to hold the reserved
// area, two FATs, and at least one data cluster.
//
// On success the newly formatted filesystem is opened and returned; the
// caller must Close it when done.
func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error) {
	const clusterSize = fmtBytesPerSector * fmtSectorsPerCluster
	if sizeBytes%clusterSize != 0 {
		return nil, fmt.Errorf("fat32: format: size %d is not a multiple of cluster size %d",
			sizeBytes, clusterSize)
	}

	totalSectors := sizeBytes / fmtBytesPerSector
	// FAT size in sectors: enough entries for every cluster (4 bytes each).
	// Clusters = (total - reserved) / sectorsPerCluster, rounded up.
	dataSectors := totalSectors - fmtReservedSectors
	// Approximate FAT size: each FAT entry is 4 bytes; entries cover all data clusters + 2 reserved.
	// We iterate to converge FAT size (since FAT itself occupies space).
	fatSizeSectors := int64(1)
	for {
		clusterCount := (dataSectors - 2*fatSizeSectors) / fmtSectorsPerCluster
		needed := (clusterCount+2)*4/fmtBytesPerSector + 1
		if needed <= fatSizeSectors {
			break
		}
		fatSizeSectors = needed
	}

	minSectors := int64(fmtReservedSectors) + 2*fatSizeSectors + int64(fmtSectorsPerCluster)
	if totalSectors < minSectors {
		return nil, fmt.Errorf("fat32: format: size %d too small (minimum %d bytes)",
			sizeBytes, minSectors*fmtBytesPerSector)
	}

	f, err := formatOpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("fat32: format: %w", err)
	}
	if err := f.Truncate(sizeBytes); err != nil {
		f.Close()
		return nil, fmt.Errorf("fat32: format: truncate: %w", err)
	}

	volumeID := cfg.VolumeID
	if volumeID == 0 {
		volumeID = formatRandUint32()
		if volumeID == 0 {
			volumeID = 0x12345678
		}
	}

	label := cfg.Label
	if len(label) > 11 {
		label = label[:11]
	}
	for len(label) < 11 {
		label += " "
	}

	le := binary.LittleEndian

	// ── Boot sector (sector 0) ────────────────────────────────────────────────
	boot := make([]byte, fmtBytesPerSector)
	boot[0] = 0xEB
	boot[1] = 0x58
	boot[2] = 0x90
	copy(boot[3:11], []byte("MSDOS5.0"))
	le.PutUint16(boot[11:], fmtBytesPerSector)
	boot[13] = fmtSectorsPerCluster
	le.PutUint16(boot[14:], fmtReservedSectors)
	boot[16] = fmtFATCount
	// boot[17:19] root entry count = 0 (FAT32)
	// boot[19:21] total sectors 16-bit = 0 (use 32-bit field)
	boot[21] = 0xF8 // media descriptor
	// boot[22:24] FAT16 size = 0 (use FAT32 field)
	le.PutUint16(boot[24:], 63)  // sectors per track (dummy)
	le.PutUint16(boot[26:], 255) // number of heads (dummy)
	// boot[28:32] hidden sectors = 0
	le.PutUint32(boot[32:], uint32(totalSectors))
	le.PutUint32(boot[36:], uint32(fatSizeSectors))
	// boot[40:42] ext flags = 0 (both FATs active, mirrored)
	le.PutUint16(boot[42:], 0) // filesystem version 0.0
	le.PutUint32(boot[44:], fmtRootCluster)
	le.PutUint16(boot[48:], fmtFSInfoSector)
	le.PutUint16(boot[50:], fmtBackupBootSector)
	boot[64] = 0x80 // drive number
	boot[66] = 0x29 // extended boot signature
	le.PutUint32(boot[67:], volumeID)
	copy(boot[71:82], label)
	copy(boot[82:90], []byte("FAT32   "))
	boot[510] = 0x55
	boot[511] = 0xAA
	if _, err := f.WriteAt(boot, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("fat32: format: write boot sector: %w", err)
	}
	// Write backup boot sector.
	if _, err := f.WriteAt(boot, int64(fmtBackupBootSector)*fmtBytesPerSector); err != nil {
		f.Close()
		return nil, fmt.Errorf("fat32: format: write backup boot sector: %w", err)
	}

	// ── FSInfo sector ────────────────────────────────────────────────────────
	fsinfo := make([]byte, fmtBytesPerSector)
	le.PutUint32(fsinfo[0:], 0x41615252)   // lead signature
	le.PutUint32(fsinfo[484:], 0x61417272) // structure signature
	le.PutUint32(fsinfo[488:], 0xFFFFFFFF) // free count: unknown
	le.PutUint32(fsinfo[492:], 0xFFFFFFFF) // next free: unknown
	le.PutUint32(fsinfo[508:], 0xAA550000) // trail signature
	if _, err := f.WriteAt(fsinfo, int64(fmtFSInfoSector)*fmtBytesPerSector); err != nil {
		f.Close()
		return nil, fmt.Errorf("fat32: format: write FSInfo: %w", err)
	}

	// ── FAT regions ──────────────────────────────────────────────────────────
	// FAT entries 0 and 1 are reserved; entry 2 (root cluster) = EOC.
	fatEntry := make([]byte, 12)
	le.PutUint32(fatEntry[0:], 0x0FFFFFF8) // FAT[0]: media descriptor + 0xFFFFF
	le.PutUint32(fatEntry[4:], 0x0FFFFFFF) // FAT[1]: end-of-chain marker
	le.PutUint32(fatEntry[8:], 0x0FFFFFFF) // FAT[2] = root cluster: EOC

	for i := int64(0); i < fmtFATCount; i++ {
		fatOff := int64(fmtReservedSectors+i*fatSizeSectors) * fmtBytesPerSector
		if _, err := f.WriteAt(fatEntry, fatOff); err != nil {
			f.Close()
			return nil, fmt.Errorf("fat32: format: write FAT %d: %w", i, err)
		}
	}

	// ── Root-directory volume-label entry ────────────────────────────────────
	// When a label is set in the BPB, canonical fsck tools (dosfstools
	// fsck.fat >= 4.2) require a matching ATTR_VOLUME_ID (0x08) entry as the
	// first record of the root directory; a boot-sector label with no root
	// entry is reported as an error and makes `fsck -n` exit non-zero. Mirror
	// the space-padded BPB label into the root cluster so the volume is clean.
	if hasNonSpace(label) {
		dataOff := int64(fmtReservedSectors+fmtFATCount*fatSizeSectors) * fmtBytesPerSector
		rootOff := dataOff + int64(fmtRootCluster-2)*clusterSize
		var volEntry [dirEntrySize]byte
		copy(volEntry[0:11], label) // already space-padded to 11 bytes
		volEntry[11] = fatAttrVolumeID
		if _, err := f.WriteAt(volEntry[:], rootOff); err != nil {
			f.Close()
			return nil, fmt.Errorf("fat32: format: write volume-label entry: %w", err)
		}
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("fat32: format: close: %w", err)
	}

	return formatOpenFS(path, -1)
}
