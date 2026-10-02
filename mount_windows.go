//go:build windows

package main

import (
	"container/list"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/winfsp/go-winfsp"
	"github.com/winfsp/go-winfsp/gofs"
	"golang.org/x/sys/windows"
)

// mount gives every volume its own drive letter from Z: downwards and blocks
// until Ctrl-C / SIGTERM, then unmounts everything.
func mount(vols []volume, debug bool) error {
	used, err := windows.GetLogicalDrives()
	if err != nil {
		return fmt.Errorf("GetLogicalDrives: %w", err)
	}
	var mounted []*winfsp.FileSystem
	defer func() {
		for _, m := range mounted {
			m.Unmount()
		}
	}()
	letter := 'Z'
	for _, v := range vols {
		for letter >= 'D' && used&(1<<(letter-'A')) != 0 {
			letter--
		}
		if letter < 'D' {
			log.Printf("%s: no free drive letter left, skipped", v.name)
			continue
		}
		bb, err := gofs.NewOptions(&winFS{fsys: v.fsys, dirs: map[string]*list.Element{}, lru: list.New()}, gofs.WithCaseInsensitive(true),
			gofs.WithAttribReadOnlyTransMode(gofs.AttribReadOnlyBypass)) // the volume is read-only; don't stamp every file READONLY, copies would inherit it
		if err != nil {
			return err
		}
		if s, ok := bb.(winfsp.BehaviourSetVolumeLabel); ok && v.label != "" {
			_ = s.SetVolumeLabel(nil, v.label, &winfsp.FSP_FSCTL_VOLUME_INFO{}) // only stores the label
		}
		drive := string(letter) + ":"
		m, err := winfsp.Mount(bb, drive,
			winfsp.CaseSensitive(false), winfsp.CasePreserveNames(true),
			winfsp.FileSystemName(v.kind), winfsp.Debug(debug),
			winfsp.Attributes(winfsp.FspFSAttributeReadOnlyVolume))
		if err != nil {
			log.Printf("%s: mount %s: %v", v.name, drive, err)
			letter--
			continue
		}
		mounted = append(mounted, m)
		log.Printf("%s mounted at %s (%s %q)", v.name, drive, v.kind, v.label)
		letter--
	}
	if len(mounted) == 0 {
		return fmt.Errorf("nothing mounted (is WinFsp installed?)")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	log.Printf("read-only; press Ctrl-C to unmount")
	<-sig
	log.Printf("unmounting")
	return nil
}

// winFS adapts an fs.FS volume to gofs.FileSystem: read-only, with
// case-insensitive path resolution over cached directory listings.
type winFS struct {
	fsys fs.FS

	mu   sync.Mutex
	dirs map[string]*list.Element // dir path -> element of lru
	lru  *list.List               // of *dirListing, front = most recent
}

type dirListing struct {
	path   string
	infos  []fs.FileInfo
	byName map[string]fs.FileInfo
}

// ponytail: bounded by directory count, not bytes; a byte budget if huge directories eat RAM.
const maxCachedDirs = 1024

func (w *winFS) list(dir string) (*dirListing, error) {
	w.mu.Lock()
	if el, ok := w.dirs[dir]; ok {
		w.lru.MoveToFront(el)
		w.mu.Unlock()
		return el.Value.(*dirListing), nil
	}
	w.mu.Unlock()

	entries, err := fs.ReadDir(w.fsys, dir)
	if err != nil {
		return nil, err
	}
	d := &dirListing{path: dir, byName: make(map[string]fs.FileInfo, len(entries))}
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		d.infos = append(d.infos, fi)
		d.byName[fi.Name()] = fi
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.dirs[dir]; !ok {
		w.dirs[dir] = w.lru.PushFront(d)
		if w.lru.Len() > maxCachedDirs {
			delete(w.dirs, w.lru.Remove(w.lru.Back()).(*dirListing).path)
		}
	}
	return d, nil
}

// resolve maps a WinFsp path (`\Dir\file.TXT`) to the volume's real path,
// matching each component exactly first, then case-insensitively.
func (w *winFS) resolve(name string) (string, fs.FileInfo, error) {
	p := strings.Trim(strings.ReplaceAll(name, `\`, "/"), "/")
	if p == "" {
		fi, err := fs.Stat(w.fsys, ".")
		return ".", fi, err
	}
	cur := "."
	var fi fs.FileInfo
	for _, c := range strings.Split(p, "/") {
		if fi != nil && !fi.IsDir() {
			return "", nil, os.ErrNotExist
		}
		d, err := w.list(cur)
		if err != nil {
			return "", nil, err
		}
		fi = d.byName[c]
		if fi == nil {
			// ponytail: linear scan for a case-mismatched name; fold-keyed map if huge dirs make this slow.
			for _, e := range d.infos {
				if strings.EqualFold(e.Name(), c) {
					fi = e
					break
				}
			}
		}
		if fi == nil {
			return "", nil, os.ErrNotExist
		}
		cur = path.Join(cur, fi.Name())
	}
	return cur, fi, nil
}

const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_TRUNC | os.O_APPEND | os.O_EXCL

func (w *winFS) OpenFile(name string, flag int, perm os.FileMode) (gofs.File, error) {
	if flag&writeFlags != 0 {
		return nil, os.ErrPermission
	}
	p, fi, err := w.resolve(name)
	if err != nil {
		return nil, err
	}
	f := &winFile{w: w, path: p, info: fi}
	if fi.Mode().IsRegular() {
		file, err := w.fsys.Open(p)
		if err != nil {
			return nil, err
		}
		f.closer = file
		f.r, _ = file.(io.ReaderAt)
	}
	return f, nil
}

func (w *winFS) Stat(name string) (os.FileInfo, error) {
	_, fi, err := w.resolve(name)
	return fi, err
}

func (w *winFS) Mkdir(string, os.FileMode) error { return os.ErrPermission }
func (w *winFS) Rename(string, string) error     { return os.ErrPermission }
func (w *winFS) Remove(string) error             { return os.ErrPermission }

type winFile struct {
	w      *winFS
	path   string
	info   fs.FileInfo
	r      io.ReaderAt // nil for directories
	closer io.Closer
	off    int64
}

func (f *winFile) ReadAt(p []byte, off int64) (int, error) {
	if f.r == nil {
		return 0, io.EOF
	}
	return f.r.ReadAt(p, off)
}

func (f *winFile) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (f *winFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.info.Size()
	}
	if offset < 0 {
		return 0, os.ErrInvalid
	}
	f.off = offset
	return offset, nil
}

func (f *winFile) Readdir(count int) ([]os.FileInfo, error) {
	if !f.info.IsDir() {
		return nil, os.ErrInvalid
	}
	d, err := f.w.list(f.path)
	if err != nil {
		return nil, err
	}
	return d.infos, nil
}

func (f *winFile) Close() error {
	if f.closer != nil {
		return f.closer.Close()
	}
	return nil
}

func (f *winFile) Stat() (os.FileInfo, error)         { return f.info, nil }
func (f *winFile) Write([]byte) (int, error)          { return 0, os.ErrPermission }
func (f *winFile) WriteAt([]byte, int64) (int, error) { return 0, os.ErrPermission }
func (f *winFile) Sync() error                        { return nil }
func (f *winFile) Truncate(int64) error               { return os.ErrPermission }
