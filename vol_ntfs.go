package main

import (
	"cmp"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"www.velocidex.com/golang/go-ntfs/parser"
)

// ntfsVol holds no lock of its own: go-ntfs guards its NTFSContext and MFT-entry LRU with
// mutexes, parsing is stateless over the reader, and a RangeReader only maps offsets onto the
// disk reader, which is our goroutine-safe chunkCache. So a read deep in a huge file (minutes on
// a 4 KiB-block squashfs, see the lazy block index) never blocks an open or a listing, and
// listings run in parallel. TestConcurrentReads covers it under -race.
type ntfsVol struct {
	ctx *parser.NTFSContext
}

func openNTFS(r *io.SectionReader) (fs.FS, string, error) {
	// No parser.NewPagedReader: it is a second cache with its own mutex, held across every read
	// into the backing store, which would serialize data reads behind each other again.
	ctx, err := parser.GetNTFSContext(r, 0)
	if err != nil {
		return nil, "", err
	}
	v := ntfsVol{ctx}
	return volFS{v}, v.label(), nil
}

// label reads $VOLUME_NAME (attribute 0x60) of $Volume (MFT entry 3).
func (v ntfsVol) label() string {
	vol, err := v.ctx.GetMFT(3)
	if err != nil {
		return ""
	}
	s, err := parser.OpenStream(v.ctx, vol, 0x60, parser.WILDCARD_STREAM_ID, parser.WILDCARD_STREAM_NAME)
	if err != nil {
		return ""
	}
	b := make([]byte, 256)
	n, _ := s.ReadAt(b, 0)
	u := make([]uint16, n/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func (v ntfsVol) entry(name string) (*parser.MFT_ENTRY, error) {
	root, err := v.ctx.GetMFT(5)
	if err != nil || name == "." {
		return root, err
	}
	return root.Open(v.ctx, name)
}

func ntfsInfo(name string, isDir bool, size int64, mtime, btime, atime time.Time) fileInfo {
	mode := uint32(0x8000)
	if isDir {
		mode = 0x4000
	}
	// ponytail: reparse points (symlinks/junctions) look like plain files or dirs.
	return newInfo(name, mode, size, mtime, btime, atime)
}

func (v ntfsVol) stat(name string) (fileInfo, error) {
	e, err := v.entry(name)
	if err != nil {
		return fileInfo{}, fmt.Errorf("%s: %w", err, fs.ErrNotExist)
	}
	// Stat returns one FileInfo per name (hard links) and per data stream;
	// the first is the unnamed stream under the primary name. Prefer the
	// entry carrying the name we were asked for.
	infos := parser.Stat(v.ctx, e)
	if len(infos) == 0 {
		if name == "." {
			return newInfo(".", 0x4000, 0, time.Time{}, time.Time{}, time.Time{}), nil
		}
		return fileInfo{}, fmt.Errorf("ntfs: no metadata for %q", name)
	}
	fi := infos[0]
	for _, i := range infos {
		if strings.EqualFold(i.Name, path.Base(name)) {
			fi = i
			break
		}
	}
	return ntfsInfo(path.Base(name), fi.IsDir, fi.Size, fi.Mtime, fi.Btime, fi.Atime), nil
}

// readDir lists the directory's own $I30 index, so a hard-linked file shows
// the name it has here (not one from another directory) and DOS 8.3 aliases
// are dropped. The 16 NTFS metafiles ($MFT, $Bitmap, ...) are hidden at the
// root like Windows does.
// ponytail: size and mtime come from the index entry, which NTFS updates lazily; stat() reads the MFT record and is exact.
func (v ntfsVol) readDir(name string) ([]fileInfo, error) {
	e, err := v.entry(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", err, fs.ErrNotExist)
	}
	var out []fileInfo
	seen := map[uint64]bool{}
	for _, rec := range e.Dir(v.ctx) {
		fn := rec.File()
		ref := rec.MftReference()
		if seen[ref] || !rec.IsValid() || fn.NameType().Name == "DOS" || (name == "." && ref < 16) {
			continue
		}
		seen[ref] = true
		const fileNameDirFlag = 0x10000000 // FILE_NAME.Flags bit go-ntfs doesn't name
		isDir := parser.ParseUint32(fn.Reader, fn.Profile.Off_FILE_NAME_Flags+fn.Offset)&fileNameDirFlag != 0
		out = append(out, ntfsInfo(fn.Name(), isDir, int64(fn.FilenameSize()), fn.File_modified().Time, fn.Created().Time, fn.File_accessed().Time))
	}
	return out, nil
}

func (v ntfsVol) open(name string) (io.ReaderAt, error) {
	e, err := v.entry(name)
	if err != nil {
		return nil, err
	}
	// The unnamed $DATA stream (Name "", so an ADS is never picked), every extent of it. Not
	// parser.GetAllVCNs/OpenStream for non-resident data: GetAllVCNs stops after 21 attribute
	// extents (a 1.4 GB compressed file has 121), and its compressed-run assembly (consumeRuns)
	// breaks once NTFS merges the sparse tails of neighbouring compression units into one run.
	var vcns []*parser.NTFS_ATTRIBUTE
	for _, a := range e.EnumerateAttributes(v.ctx) {
		if a.Type().Value == parser.ATTR_TYPE_DATA && a.Name() == "" {
			vcns = append(vcns, a)
		}
	}
	if len(vcns) == 0 {
		return nil, fmt.Errorf("%s: no data stream: %w", name, fs.ErrNotExist)
	}
	if vcns[0].IsResident() {
		return parser.OpenStream(v.ctx, e, parser.ATTR_TYPE_DATA, parser.WILDCARD_STREAM_ID, "")
	}
	start := func(a *parser.NTFS_ATTRIBUTE) uint64 { return a.Runlist_vcn_start() }
	slices.SortFunc(vcns, func(a, b *parser.NTFS_ATTRIBUTE) int { return cmp.Compare(start(a), start(b)) })
	vcns = slices.CompactFunc(vcns, func(a, b *parser.NTFS_ATTRIBUTE) bool { return start(a) == start(b) })
	return newNTFSStream(v.ctx, vcns), nil
}

// ntfsStream reads a non-resident $DATA stream from its run list (VCN -> LCN, sparse = zeros).
// A compressed stream is read per compression unit (16 clusters): in VCN space a unit is all
// sparse (zeros), all real (stored raw), or r real clusters holding an LZNT1 stream followed by
// sparse ones. The real clusters of a unit may be split over runs and one run may span units,
// so units are cut from the run map, never from the runs directly.
type ntfsStream struct {
	disk          io.ReaderAt
	cluster, unit int64 // bytes per cluster, clusters per compression unit
	size, init    int64
	compressed    bool
	runs          []ntfsRun // consecutive in VCN space

	mu      sync.Mutex // guards the decoded-unit slot
	curUnit int64
	cur     []byte
}

type ntfsRun struct{ vcn, lcn, n int64 } // lcn -1 = sparse

func newNTFSStream(ctx *parser.NTFSContext, vcns []*parser.NTFS_ATTRIBUTE) *ntfsStream {
	a := vcns[0] // VCN 0 carries the sizes and the unit size
	c := &ntfsStream{disk: ctx.DiskReader, cluster: ctx.ClusterSize, unit: 1 << a.Compression_unit_size(),
		size: int64(a.Actual_size()), init: int64(a.Initialized_size()), compressed: parser.IsCompressed(a.Flags()), curUnit: -1}
	if a.Compression_unit_size() == 0 {
		c.unit = 16
	}
	for _, at := range vcns {
		vcn := int64(at.Runlist_vcn_start())
		for _, r := range at.RunList() {
			lcn := r.Offset
			if r.RelativeUrnOffset == 0 {
				lcn = -1
			}
			c.runs = append(c.runs, ntfsRun{vcn, lcn, r.Length})
			vcn += r.Length
		}
	}
	return c
}

// mapped returns the piece of the run map that starts at vcn and ends at the next run boundary
// or at end; lcn -1 = sparse, also past the end of the run list.
func (c *ntfsStream) mapped(vcn, end int64) ntfsRun {
	i := sort.Search(len(c.runs), func(i int) bool { return c.runs[i].vcn+c.runs[i].n > vcn })
	if i == len(c.runs) || c.runs[i].vcn > vcn {
		return ntfsRun{vcn, -1, end - vcn}
	}
	r := c.runs[i]
	n := min(r.vcn+r.n, end) - vcn
	if r.lcn < 0 {
		return ntfsRun{vcn, -1, n}
	}
	return ntfsRun{vcn, r.lcn + vcn - r.vcn, n}
}

func (c *ntfsStream) ReadAt(p []byte, off int64) (int, error) {
	if off >= c.size {
		return 0, io.EOF
	}
	var err error
	if int64(len(p)) > c.size-off {
		p, err = p[:c.size-off], io.EOF
	}
	if c.compressed {
		if n, err := c.readUnits(p, off); err != nil {
			return n, err
		}
	} else {
		for n := 0; n < len(p); {
			pos := off + int64(n)
			vcn := pos / c.cluster
			r := c.mapped(vcn, vcn+1+int64(len(p)-n)/c.cluster)
			k := min(int64(len(p)-n), (r.vcn+r.n)*c.cluster-pos)
			if r.lcn < 0 {
				clear(p[n : n+int(k)])
			} else if _, rerr := c.disk.ReadAt(p[n:n+int(k)], r.lcn*c.cluster+pos%c.cluster); rerr != nil && rerr != io.EOF {
				return n, rerr
			}
			n += int(k)
		}
	}
	if c.init < off+int64(len(p)) { // past Initialized_size reads as zeros
		clear(p[max(c.init-off, 0):])
	}
	return len(p), err
}

func (c *ntfsStream) readUnits(p []byte, off int64) (int, error) {
	ub := c.unit * c.cluster
	c.mu.Lock()
	defer c.mu.Unlock()
	for n := 0; n < len(p); {
		pos := off + int64(n)
		u := pos / ub
		if u != c.curUnit {
			b, err := c.decode(u)
			if err != nil {
				return n, err
			}
			c.curUnit, c.cur = u, b
		}
		n += copy(p[n:], c.cur[pos-u*ub:])
	}
	return len(p), nil
}

// decode returns compression unit u, unit bytes long (shorter for the file's last unit).
// ponytail: one decoded unit per open file; an LRU of a few units if random reads on compressed files show up.
func (c *ntfsStream) decode(u int64) ([]byte, error) {
	ub := c.unit * c.cluster
	out := make([]byte, min(ub, c.size-u*ub))
	var real []ntfsRun // the unit's real clusters, until the first sparse one
	r, sparse := int64(0), false
	for vcn, end := u*c.unit, (u+1)*c.unit; vcn < end; {
		x := c.mapped(vcn, end)
		if x.lcn < 0 {
			sparse = true
		} else if sparse {
			return nil, fmt.Errorf("ntfs: compression unit %d: data after sparse clusters", u)
		} else {
			real = append(real, x)
			r += x.n
		}
		vcn += x.n
	}
	if r == 0 {
		return out, nil
	}
	buf := out // r == unit: stored raw
	if r < c.unit {
		buf = make([]byte, r*c.cluster)
	}
	at := int64(0)
	for _, x := range real {
		if at >= int64(len(buf)) {
			break
		}
		if _, err := c.disk.ReadAt(buf[at:min(at+x.n*c.cluster, int64(len(buf)))], x.lcn*c.cluster); err != nil && err != io.EOF {
			return nil, err
		}
		at += x.n * c.cluster
	}
	if r == c.unit {
		return out, nil
	}
	d, err := parser.LZNT1Decompress(buf)
	if err != nil {
		return nil, fmt.Errorf("ntfs: compression unit %d: %w", u, err)
	}
	copy(out, d)
	return out, nil
}
