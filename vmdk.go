package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// VMDK (VMware Virtual Disk Format 5.0): either one hosted sparse extent ("KDMV" at 0:
// monolithicSparse, streamOptimized) or a text descriptor naming extent files next to it
// (monolithicFlat, twoGbMaxExtent*, ESXi -flat.vmdk). Delta disks (snapshots) are refused.

const vmdkDescMagic = "# Disk DescriptorFile"

// openVMDK opens path (whose contents raw holds) as a VMDK and returns the virtual disk.
func openVMDK(path string, raw io.ReaderAt, size int64) (io.ReaderAt, int64, error) {
	name := filepath.Base(path)
	var magic [4]byte
	raw.ReadAt(magic[:], 0)
	if string(magic[:]) == "KDMV" {
		s, err := openSparseExtent(raw, size)
		if err != nil {
			return nil, 0, err
		}
		kind := "monolithicSparse"
		if s.descSize > 0 && s.descSize < 1<<20 { // embedded descriptor: only checked for a parent
			b := make([]byte, s.descSize)
			if _, err := raw.ReadAt(b, s.descOff); err != nil {
				return nil, 0, fmt.Errorf("vmdk descriptor: %w", err)
			}
			d, err := parseVMDKDescriptor(b)
			if err != nil {
				return nil, 0, err
			}
			if d.createType != "" {
				kind = d.createType
			}
		}
		log.Printf("%s: vmdk %s, 1 extent, %d MiB virtual", name, kind, s.size>>20)
		return s, s.size, nil
	}

	if size > 1<<20 {
		return nil, 0, errors.New("vmdk descriptor larger than 1 MiB")
	}
	b := make([]byte, size)
	if _, err := raw.ReadAt(b, 0); err != nil {
		return nil, 0, err
	}
	d, err := parseVMDKDescriptor(b)
	if err != nil {
		return nil, 0, err
	}
	if len(d.extents) == 0 {
		return nil, 0, errors.New("vmdk descriptor lists no extents")
	}
	disk := &vmdkDisk{}
	for _, e := range d.extents {
		x := vmdkExtent{start: disk.size, size: e.sectors * 512}
		if e.kind == "ZERO" {
			x.r = zeroReader{}
		} else {
			// ponytail: extent files are read uncached (the top chunkCache covers repeats); none are ever closed.
			f, err := os.Open(filepath.Join(filepath.Dir(path), e.file))
			if err != nil {
				return nil, 0, fmt.Errorf("vmdk extent: %w", err)
			}
			switch e.kind {
			case "FLAT", "VMFS":
				x.r = io.NewSectionReader(f, e.offset*512, x.size)
			case "SPARSE":
				st, err := f.Stat()
				if err != nil {
					return nil, 0, err
				}
				s, err := openSparseExtent(f, st.Size())
				if err != nil {
					return nil, 0, fmt.Errorf("%s: %w", e.file, err)
				}
				if s.size != x.size {
					return nil, 0, fmt.Errorf("%s: capacity %d sectors, descriptor says %d", e.file, s.size/512, e.sectors)
				}
				x.r = s
			}
		}
		disk.ext = append(disk.ext, x)
		disk.size += x.size
	}
	log.Printf("%s: vmdk %s, %d extents, %d MiB virtual", name, d.createType, len(disk.ext), disk.size>>20)
	return disk, disk.size, nil
}

type vmdkDescriptor struct {
	createType string
	extents    []vmdkExtentLine
}

type vmdkExtentLine struct {
	sectors, offset int64
	kind, file      string
}

// parseVMDKDescriptor reads createType and the extent lines
// (`RW 4192256 SPARSE "disk-s001.vmdk"`, `RW 8388608 FLAT "disk-flat.vmdk" 0`, `RW 1024 ZERO`)
// and refuses delta disks.
func parseVMDKDescriptor(b []byte) (*vmdkDescriptor, error) {
	d := &vmdkDescriptor{}
	for _, line := range strings.Split(string(bytes.TrimRight(b, "\x00")), "\n") {
		line = strings.TrimSpace(line)
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)
			switch {
			case k == "createType":
				d.createType = v
			case k == "parentFileNameHint", k == "parentCID" && !strings.EqualFold(v, "ffffffff"):
				return nil, errors.New("VMDK delta/snapshot disk, parent not supported")
			}
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 || (f[0] != "RW" && f[0] != "RDONLY" && f[0] != "NOACCESS") {
			continue
		}
		sectors, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil || sectors < 0 || sectors > 1<<40 {
			return nil, fmt.Errorf("vmdk extent line %q: bad sector count", line)
		}
		e := vmdkExtentLine{sectors: sectors, kind: f[2]}
		switch e.kind {
		case "ZERO":
		case "FLAT", "VMFS", "SPARSE":
			// the file name is quoted and may contain spaces; an optional sector offset follows it
			_, rest, _ := strings.Cut(line, `"`)
			file, tail, ok := strings.Cut(rest, `"`)
			if !ok || file == "" {
				return nil, fmt.Errorf("vmdk extent line %q: no file name", line)
			}
			e.file = file
			if t := strings.TrimSpace(tail); t != "" && e.kind != "SPARSE" {
				if e.offset, err = strconv.ParseInt(t, 10, 64); err != nil || e.offset < 0 {
					return nil, fmt.Errorf("vmdk extent line %q: bad offset", line)
				}
			}
		default: // VMFSSPARSE (COWD), SESPARSE, VMFSRDM, ...
			return nil, fmt.Errorf("vmdk extent type %s not supported", e.kind)
		}
		d.extents = append(d.extents, e)
	}
	return d, nil
}

// vmdkDisk concatenates the descriptor's extents.
type vmdkDisk struct {
	ext  []vmdkExtent
	size int64
}

type vmdkExtent struct {
	start, size int64
	r           io.ReaderAt
}

func (d *vmdkDisk) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= d.size {
		return 0, io.EOF
	}
	i := sort.Search(len(d.ext), func(i int) bool { return d.ext[i].start+d.ext[i].size > off })
	for ; n < len(p) && i < len(d.ext); i++ {
		e := d.ext[i]
		pos := off + int64(n)
		k := min(int64(len(p)-n), e.start+e.size-pos)
		m, err := e.r.ReadAt(p[n:n+int(k)], pos-e.start)
		n += m
		if m < int(k) {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF // an extent file shorter than the descriptor says
			}
			return n, err
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

type zeroReader struct{}

func (zeroReader) ReadAt(p []byte, off int64) (int, error) { clear(p); return len(p), nil }

// sparseExtent is a hosted sparse extent: a grain directory of grain tables of
// grain table entries (sector of the grain; 0 = unallocated, 1 = zero grain).
type sparseExtent struct {
	r                 io.ReaderAt
	size, grain       int64 // bytes
	gtes              int64 // entries per grain table
	gd                []uint32
	compressed        bool
	descOff, descSize int64
}

func openSparseExtent(r io.ReaderAt, fileSize int64) (*sparseExtent, error) {
	var h [512]byte
	if _, err := r.ReadAt(h[:], 0); err != nil {
		return nil, fmt.Errorf("vmdk header: %w", err)
	}
	le := binary.LittleEndian
	if string(h[:4]) != "KDMV" {
		return nil, errors.New("not a VMDK sparse extent (no KDMV magic)")
	}
	if le.Uint64(h[56:]) == ^uint64(0) { // streamOptimized: the real header is the footer at size-1024
		if _, err := r.ReadAt(h[:], fileSize-1024); err != nil || string(h[:4]) != "KDMV" {
			return nil, fmt.Errorf("vmdk streamOptimized footer not found (truncated file?): %v", err)
		}
	}
	version, flags := le.Uint32(h[4:]), le.Uint32(h[8:])
	capacity, grain, gtes := le.Uint64(h[12:]), le.Uint64(h[20:]), uint64(le.Uint32(h[44:]))
	gdOff, alg := le.Uint64(h[56:]), le.Uint16(h[77:])
	switch {
	case version < 1 || version > 3:
		return nil, fmt.Errorf("vmdk sparse version %d not supported", version)
	case grain == 0 || grain > 1<<16 || gtes == 0 || gtes > 1<<16 || capacity > 1<<40:
		return nil, fmt.Errorf("vmdk header: bad geometry (capacity %d, grain %d, %d GTEs/GT)", capacity, grain, gtes)
	case flags&(1<<16) != 0 && alg != 1:
		return nil, fmt.Errorf("vmdk compression algorithm %d not supported", alg)
	case gdOff == ^uint64(0) || gdOff == 0:
		return nil, errors.New("vmdk grain directory missing")
	}
	s := &sparseExtent{r: r, size: int64(capacity) * 512, grain: int64(grain) * 512, gtes: int64(gtes),
		compressed: flags&(1<<16) != 0, descOff: int64(le.Uint64(h[28:])) * 512, descSize: int64(le.Uint64(h[36:])) * 512}
	n := (int64(capacity) + int64(grain*gtes) - 1) / int64(grain*gtes)
	b := make([]byte, n*4)
	if _, err := r.ReadAt(b, int64(gdOff)*512); err != nil {
		return nil, fmt.Errorf("vmdk grain directory: %w", err)
	}
	s.gd = make([]uint32, n)
	binary.Decode(b, le, s.gd)
	return s, nil
}

func (s *sparseExtent) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	var gt []uint32 // GTEs of grains g0.. within one grain table, loaded per read
	var g0 int64
	for n < len(p) && off+int64(n) < s.size {
		pos := off + int64(n)
		g, in := pos/s.grain, pos%s.grain
		k := min(int64(len(p)-n), s.grain-in, s.size-pos)
		if g < g0 || g >= g0+int64(len(gt)) {
			if gt, err = s.loadGT(g, (off+int64(len(p))-1)/s.grain); err != nil {
				return n, err
			}
			g0 = g
		}
		sect := int64(gt[g-g0])
		dst := p[n : n+int(k)]
		switch {
		case sect <= 1: // unallocated or zero grain
			clear(dst)
		case !s.compressed:
			if _, err := s.r.ReadAt(dst, sect*512+in); err != nil {
				return n, fmt.Errorf("vmdk grain %d: %w", g, err)
			}
		default:
			if err := s.readCompressed(dst, g, sect, in); err != nil {
				return n, err
			}
		}
		n += int(k)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// loadGT returns the GTEs from grain g up to last, cut at the end of g's grain table.
func (s *sparseExtent) loadGT(g, last int64) ([]uint32, error) {
	t := g / s.gtes
	cnt := min(last-g+1, (t+1)*s.gtes-g)
	gt := make([]uint32, cnt)
	if t >= int64(len(s.gd)) || s.gd[t] == 0 { // no grain table: all unallocated
		return gt, nil
	}
	b := make([]byte, cnt*4)
	if _, err := s.r.ReadAt(b, int64(s.gd[t])*512+(g%s.gtes)*4); err != nil {
		return nil, fmt.Errorf("vmdk grain table %d: %w", t, err)
	}
	binary.Decode(b, binary.LittleEndian, gt)
	return gt, nil
}

// readCompressed inflates grain g (a marker `u64 lba, u32 size` then zlib data at sect)
// far enough to fill dst from offset in.
// ponytail: inflates the grain prefix on every call; the decompressed chunkCache above makes repeats rare.
func (s *sparseExtent) readCompressed(dst []byte, g, sect, in int64) error {
	var m [12]byte
	if _, err := s.r.ReadAt(m[:], sect*512); err != nil {
		return fmt.Errorf("vmdk grain %d marker: %w", g, err)
	}
	lba, csize := binary.LittleEndian.Uint64(m[:]), int64(binary.LittleEndian.Uint32(m[8:]))
	if lba != uint64(g*s.grain/512) || csize > 2*s.grain+1024 {
		return fmt.Errorf("vmdk grain %d: bad marker (lba %d, size %d)", g, lba, csize)
	}
	zr, err := zlib.NewReader(io.NewSectionReader(s.r, sect*512+12, csize))
	if err != nil {
		return fmt.Errorf("vmdk grain %d: %w", g, err)
	}
	if _, err := io.CopyN(io.Discard, zr, in); err != nil {
		return fmt.Errorf("vmdk grain %d: %w", g, err)
	}
	if _, err := io.ReadFull(zr, dst); err != nil {
		return fmt.Errorf("vmdk grain %d: %w", g, err)
	}
	return nil
}
