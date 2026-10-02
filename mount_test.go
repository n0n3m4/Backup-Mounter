//go:build !windows

package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestFixtures runs the whole stack against testdata/make_fixtures.sh output:
// BM_FIXTURES=/path/to/dir go test -tags xz,zstd ./...
func TestFixtures(t *testing.T) {
	dir := os.Getenv("BM_FIXTURES")
	if dir == "" {
		t.Skip("BM_FIXTURES not set; run testdata/make_fixtures.sh DIR first")
	}
	defer func(s int64) { indexSpan = s }(indexSpan)
	indexSpan = 1 << 20                                  // 37 checkpoints in the 200 MiB fixture (zero runs make multi-MiB deflate blocks)
	os.Remove(filepath.Join(dir, "mbr/disk.img.gz.idx")) // first open builds it, the second reuses it
	want := readManifest(t, filepath.Join(dir, "manifest.sha256"))
	want["link"] = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // ponytail symlink: empty file
	st, err := os.Stat(filepath.Join(dir, "manifest.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(st.ModTime().Year(), st.ModTime().Month(), st.ModTime().Day(), 0, 0, 0, 0, time.Local) // FAT access time is date-only, local

	for _, tc := range []struct {
		file  string
		kinds []string
	}{
		{"gpt-zstd.squashfs", []string{"ext4", "btrfs", "fat32", "ntfs", "exfat"}},
		{"gpt-lz4.squashfs", []string{"ext4", "btrfs", "fat32", "ntfs", "exfat"}},
		{"mbr-gzip.squashfs", []string{"ext4", "ntfs", "fat32", "ext4"}}, // 2 logical partitions, then the bare image
		{"gpt-dyn.vhd", []string{"ext4", "btrfs", "fat32", "ntfs", "exfat"}},
		{"gpt-fixed.vhd", []string{"ext4", "btrfs", "fat32", "ntfs", "exfat"}},
		{"gpt.vhdx", []string{"ext4", "btrfs", "fat32", "ntfs", "exfat"}},
		{"mbr/disk.img", []string{"ext4", "ntfs", "fat32"}},                                                 // raw MBR disk as the top-level file
		{"mbr/bare-ext4.img", []string{"ext4"}},                                                             // single partition as the top-level file
		{"mbr/disk.img.gz", []string{"ext4", "ntfs", "fat32"}},                                              // gzip checkpoints; the 32 MiB run evicts and re-seeks
		{"mbr/disk.img.xz", []string{"ext4", "ntfs", "fat32"}},                                              // 25 xz blocks
		{"vhd-zstd.squashfs", []string{"ext4", "btrfs", "fat32", "ntfs", "exfat", "ext4", "ntfs", "fat32"}}, // gpt.vhdx (386M) then mbr-dyn.vhd (200M)
		{"manifest.sha256", nil}, // neither squashfs nor image: no volumes, no error
	} {
		for _, budget := range []int64{256 << 20, 32 << 20} { // 32 MiB: ~10x smaller than the image, forces eviction
			vols, err := openStack(filepath.Join(dir, tc.file), budget, nil)
			if err != nil {
				t.Fatalf("%s: %v", tc.file, err)
			}
			var kinds []string
			for _, v := range vols {
				kinds = append(kinds, v.kind)
			}
			if !slices.Equal(kinds, tc.kinds) {
				t.Fatalf("%s: kinds %v, want %v", tc.file, kinds, tc.kinds)
			}
			for _, v := range vols {
				if want := map[string]string{"ext4": "ext4vol", "btrfs": "btrfsvol", "fat32": "FATVOL", "ntfs": "ntfsvol", "exfat": "EXFATVOL"}[v.kind]; v.label != want {
					t.Errorf("%s: label %q, want %q", v.name, v.label, want)
				}
				checkTimes(t, v, since)
				root, err := fs.ReadDir(v.fsys, ".")
				if err != nil {
					t.Fatalf("%s: root listing: %v", v.name, err)
				}
				switch v.kind {
				case "ntfs":
					var names []string
					for _, e := range root {
						names = append(names, e.Name())
					}
					if !slices.Equal(names, []string{"hello.txt", "random.bin"}) { // metafiles hidden
						t.Errorf("%s: root = %v", v.name, names)
					}
					checkFiles(t, v, map[string]string{"hello.txt": want["hello.txt"], "random.bin": want["dir/random.bin"]})
				case "ext4", "btrfs", "fat32", "exfat":
					checkTree(t, v, filepath.Join(dir, "src"), want)
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "mbr/disk.img.gz.idx")); err != nil {
		t.Errorf("gzip index not written next to the archive: %v", err)
	}
}

func TestSingleBlockXZ(t *testing.T) {
	dir := os.Getenv("BM_FIXTURES")
	if dir == "" {
		t.Skip("BM_FIXTURES not set")
	}
	if _, err := openStack(filepath.Join(dir, "mbr/single.img.xz"), 32<<20, nil); err == nil || !strings.Contains(err.Error(), "single-block") {
		t.Fatalf("err = %v, want single-block xz error", err)
	}
}

func readManifest(t *testing.T, path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		sum, name, _ := strings.Cut(line, "  ")
		m[strings.TrimPrefix(name, "./")] = sum
	}
	return m
}

// checkTree compares every directory listing with the source tree and every
// file hash (via walkVolume) with the manifest.
func checkTree(t *testing.T, v volume, src string, want map[string]string) {
	filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		var exp, got []string
		srcEntries, _ := os.ReadDir(p)
		for _, e := range srcEntries {
			exp = append(exp, e.Name())
		}
		entries, err := fs.ReadDir(v.fsys, filepath.ToSlash(rel))
		if err != nil {
			t.Errorf("%s: ReadDir(%s): %v", v.name, rel, err)
			return nil
		}
		for _, e := range entries {
			if e.Name() != "lost+found" {
				got = append(got, e.Name())
			}
		}
		if !slices.Equal(got, exp) {
			t.Errorf("%s: ReadDir(%s) = %v, want %v", v.name, rel, got, exp)
		}
		return nil
	})

	checkFiles(t, v, want)
}

// checkFiles hashes every file via walkVolume and again with odd-sized ReadAt.
func checkFiles(t *testing.T, v volume, want map[string]string) {
	var out bytes.Buffer
	if err := walkVolume(&out, v); err != nil {
		t.Fatalf("%s: walk: %v", v.name, err)
	}
	got := map[string]string{}
	for sc := bufio.NewScanner(&out); sc.Scan(); {
		f := strings.Fields(sc.Text()) // "<vol>/<path> <size> <sha>"; the unicode name contains a space
		name := strings.TrimPrefix(strings.Join(f[:len(f)-2], " "), v.name+"/")
		got[name] = f[len(f)-1]
	}
	for name, sum := range want {
		if got[name] != sum {
			t.Errorf("%s: %s sha256 %q, want %q", v.name, name, got[name], sum)
		}
		// Re-read with an odd ReadAt size so reads straddle extent/block boundaries.
		f, err := v.fsys.Open(name)
		if err != nil {
			t.Fatalf("%s: open %s: %v", v.name, name, err)
		}
		h := sha256.New()
		buf := make([]byte, 77777)
		for off := int64(0); ; {
			n, err := f.(io.ReaderAt).ReadAt(buf, off)
			h.Write(buf[:n])
			off += int64(n)
			if err != nil {
				break
			}
		}
		if hex.EncodeToString(h.Sum(nil)) != sum {
			t.Errorf("%s: %s: odd-sized ReadAt gives a different sha256", v.name, name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: walked %d files, want %d", v.name, len(got), len(want))
	}
}

// checkTimes requires every entry's modify, create and access time (from both
// ReadDir and Stat) to fall inside the fixture build window: a zero time, a
// misdecoded one (1754, 1980) or a wrong time zone all land outside it.
func checkTimes(t *testing.T, v volume, since time.Time) {
	now := time.Now().Add(5 * time.Second)
	fs.WalkDir(v.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("%s: walk %s: %v", v.name, p, err)
			return nil
		}
		if p == "." {
			return nil // FAT roots have no directory entry, hence no times
		}
		listed, _ := d.Info()
		stated, err := fs.Stat(v.fsys, p)
		if err != nil {
			t.Errorf("%s: stat %s: %v", v.name, p, err)
			return nil
		}
		for _, fi := range []fs.FileInfo{listed, stated} {
			i := fi.(fileInfo)
			for what, ts := range map[string]time.Time{"mtime": i.mtime, "btime": i.btime, "atime": i.atime} {
				if ts.Before(since) || ts.After(now) {
					t.Errorf("%s: %s: %s %v outside [%v, %v]", v.name, p, what, ts, since, now)
				}
			}
		}
		return nil
	})
}
