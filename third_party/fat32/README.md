<p align="center"><img src="https://raw.githubusercontent.com/go-filesystems/brand/main/social/go-filesystems-fat32.png" alt="go-filesystems/fat32" width="720"></p>

# fat32

[![Go Reference](https://pkg.go.dev/badge/github.com/go-filesystems/fat32.svg)](https://pkg.go.dev/github.com/go-filesystems/fat32)
[![License: BSD-3-Clause](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)
[![CI](https://github.com/go-filesystems/fat32/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/fat32/actions/workflows/ci.yml)

Pure-Go read/write access to FAT32 filesystem images — no root privileges, no external tools, no CGO.

Supports bare filesystem images and MBR/GPT partitioned disks, full directory traversal, file mutation and filesystem creation.

## Support summary

| Feature | Status | Notes |
|---|---:|---|
| Open / Close | ✅ | Supports bare images and partitioned disks |
| Format | ✅ | Creates FAT32 images |
| ReadFile | ✅ | Full file reads supported |
| Read at an offset | ✅ | `OpenFile(path)` → `io.ReaderAt` + `Size()` (`filesystem.Opener` / `filesystem.File`) |
| Write at an offset | ✅ | `OpenFile(path)` → `io.WriterAt` + `Truncate` + `Sync` (`filesystem.WritableFile`) |
| WriteFile | ✅ | Full file writes supported |
| MkDir / Delete / Rename | ✅ | Directory operations supported |
| ReadLink / Symlinks | ⚠️ No | FAT32 has no POSIX symlinks |
| Volume label | ✅ | `Label` / `SetLabel` (`filesystem.Labeller`) |
| Truncate | ✅ | `Truncate(path, newSize)` (`filesystem.Truncater`) |
| Grow / Shrink / Resize | ✅ | `filesystem.Resizer` — `Resize` dispatches to `Grow` or `Shrink` |
| Partitioned images | ✅ | MBR/GPT supported |

## Limitations

- FAT32 has no POSIX symlinks, permissions, or ACLs.
- Filename charset and legacy constraints (8.3 compatibility concerns) may apply in some tooling contexts.
- Intended for test and tooling scenarios; not recommended as a production POSIX filesystem.

## Module

```text
github.com/go-filesystems/fat32
```

## Supported operations

| Operation    | Status         |
|--------------|----------------|
| Open / Close | ✅ implemented |
| Format       | ✅ implemented |
| Stat         | ✅ implemented |
| ListDir      | ✅ implemented |
| ReadFile     | ✅ implemented |
| OpenFile     | ✅ implemented (`filesystem.Opener`) |
| WriteAt      | ✅ implemented (`filesystem.WritableFile`, on the `File`) |
| WriteFile    | ✅ implemented |
| MkDir        | ✅ implemented |
| DeleteFile   | ✅ implemented |
| DeleteDir    | ✅ implemented (recursive) |
| Rename       | ✅ implemented |
| ReadLink     | ⚠️ stub — FAT32 has no symlinks |

## API

`Open` and `Format` return the plain `filesystem.Filesystem` interface (there
is no richer exported `FS` type) — FAT32-specific extras (`Info`,
`PartitionOffset`, volume label, truncate, resize) are reached by
type-asserting to the optional interfaces from
`github.com/go-filesystems/interface`, or to the package's own `Info` getter.

### Format / Open

```go
type FormatConfig struct {
    Label    string
    VolumeID uint32 // 0 = randomly generated
}

func Format(path string, sizeBytes int64, cfg FormatConfig) (filesystem.Filesystem, error)
func Open(imagePath string, partIndex int) (filesystem.Filesystem, error)
```

### Read / Write

The returned value's `Stat` / `ListDir` / `ReadFile` / `WriteFile` / `MkDir` /
`DeleteFile` / `DeleteDir` / `Rename` are exactly the
[`filesystem.Filesystem`](https://github.com/go-filesystems/interface)
contract — see that package's README for the full signatures.

### FAT32-specific extras (type-assert)

```go
// Read part of a file without materialising all of it: OpenFile resolves the
// FAT chain once (cluster numbers, not data) and then serves byte ranges, so a
// 4 KiB read out of a 4 GiB file costs 4 KiB. Required by anything that mounts
// or exports the volume.
if o, ok := fs.(filesystem.Opener); ok {
    f, err := o.OpenFile("/big.bin")
    if err != nil {
        return err
    }
    defer f.Close()
    buf := make([]byte, 4096)
    n, err := f.ReadAt(buf, 1<<30) // io.ReaderAt semantics, exactly
    _, _ = n, err
    _ = f.Size()                   // from metadata; reads nothing

    // ...and write part of one, in place. Without this a positional write can
    // only be ReadFile + splice + WriteFile: the whole file read, reallocated
    // and written back on every request, so writing a file in blocks is
    // quadratic. Measured over a real NFS mount, 2 MiB in 64 KiB blocks took
    // 23 s (90 kB/s) that way.
    if w, ok := f.(filesystem.WritableFile); ok {
        _, err = w.WriteAt(buf, 1<<30) // io.WriterAt semantics, exactly
        _ = w.Truncate(1 << 20)        // grow zero-fills; shrink frees clusters
        _ = w.Sync()                   // fsync(2) on a file-backed image
    }
}
if l, ok := fs.(filesystem.Labeller); ok {
    _ = l.SetLabel("MYVOL")
}
if t, ok := fs.(filesystem.Truncater); ok {
    _ = t.Truncate("/big.bin", 4096)
}
if r, ok := fs.(filesystem.Resizer); ok {
    _ = r.Resize(newSizeBytes) // dispatches to the package's Grow/Shrink
}
if i, ok := fs.(interface{ Info() Info }); ok {
    fmt.Println(i.Info().ClusterSize())
}
```

## Implements

This package implements the `filesystem.Filesystem` interface defined in
`github.com/go-filesystems/interface`. Callers can treat the value returned
by `Open`/`Format` as a `filesystem.Filesystem` to write generic tooling that
works across the other filesystem modules in this repository.

Example:

```go
import (
    filesystem "github.com/go-filesystems/interface"
    fsfat "github.com/go-filesystems/fat32"
)

f, _ := fsfat.Open("fat32.img", -1)
defer f.Close()
var fs filesystem.Filesystem = f
_, _ = fs.ListDir("/")
```
