package squashfs

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestBlockListStraddle: a 1-byte symlink inode before the file misaligns its block list, so
// every metadata block boundary splits an entry; random reads must still match. backup-mounter.
func TestBlockListStraddle(t *testing.T) {
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.Mkdir(src, 0o755)
	want := make([]byte, 20<<20+123) // 5121 entries at 4 KiB: 3 metadata blocks
	rand.New(rand.NewSource(1)).Read(want[:6<<20])
	if err := os.Symlink("x", filepath.Join(src, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "b"), want, 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "s.sqfs")
	if out, err := exec.Command("mksquashfs", src, img, "-b", "4K", "-noappend", "-quiet", "-no-progress", "-no-fragments").CombinedOutput(); err != nil {
		t.Fatalf("mksquashfs: %v %s", err, out)
	}
	sb, err := Open(img)
	if err != nil {
		t.Fatal(err)
	}
	ino, err := sb.FindInode("b", false)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(2))
	got := make([]byte, len(want))
	for k := 0; k < 500; k++ {
		off := rng.Intn(len(want))
		n := min(rng.Intn(20000)+1, len(want)-off)
		if _, err := ino.ReadAt(got[off:off+n], int64(off)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got[off:off+n], want[off:off+n]) {
			t.Fatalf("mismatch at %d+%d", off, n)
		}
	}
	split := 0
	for _, c := range ino.blocks.ck[1:] {
		if c.skip != 0 {
			split++
		}
	}
	if len(ino.blocks.ck) < 3 || split == 0 {
		t.Fatalf("want >= 3 checkpoints with split entries, got %+v", ino.blocks.ck)
	}
}
