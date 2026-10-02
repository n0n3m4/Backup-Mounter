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
)

// TestFixtures runs the whole stack against testdata/make_fixtures.sh output:
// BM_FIXTURES=/path/to/dir go test -tags xz,zstd ./...
func TestFixtures(t *testing.T) {
	dir := os.Getenv("BM_FIXTURES")
	if dir == "" {
		t.Skip("BM_FIXTURES not set; run testdata/make_fixtures.sh DIR first")
	}
	want := readManifest(t, filepath.Join(dir, "manifest.sha256"))
	want["link"] = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // ponytail symlink: empty file

	for _, tc := range []struct {
		file  string
		kinds []string
	}{
		{"gpt-zstd.squashfs", []string{"ext4", "btrfs", "fat32", "ntfs"}},
		{"mbr-gzip.squashfs", []string{"ext4", "ntfs", "fat32", "ext4"}}, // 2 logical partitions, then the bare image
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
				if want := map[string]string{"ext4": "ext4vol", "btrfs": "btrfsvol", "fat32": "FATVOL", "ntfs": "ntfsvol"}[v.kind]; v.label != want {
					t.Errorf("%s: label %q, want %q", v.name, v.label, want)
				}
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
				case "ext4", "btrfs":
					checkTree(t, v, filepath.Join(dir, "src"), want)
				}
			}
		}
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
