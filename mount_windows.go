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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
	if debug && logFile != nil {
		// go-winfsp's procs are value copies resolved when the DLL loads; DebugLogSetHandle before
		// the first Mount would panic on an unresolved copy, so load the DLL explicitly first.
		if err := winfsp.LoadWinFSP(); err != nil {
			return fmt.Errorf("load WinFsp: %w", err)
		}
		if err := winfsp.DebugLogSetHandle(syscall.Handle(logFile.Fd())); err != nil {
			log.Printf("WinFsp debug log to --log file: %v", err)
		}
	}
	go readWatchdog()
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

// sysInfo hands gofs separate creation and access times: it takes them only
// from a Win32FileAttributeData, otherwise every stamp is ModTime.
func sysInfo(fi fileInfo) any {
	ft := func(t time.Time) syscall.Filetime { return syscall.NsecToFiletime(t.UnixNano()) }
	return &syscall.Win32FileAttributeData{CreationTime: ft(fi.btime), LastAccessTime: ft(fi.atime), LastWriteTime: ft(fi.mtime)}
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
	defer track("open", name, 0, 0)()
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
	defer track("stat", name, 0, 0)()
	_, fi, err := w.resolve(name)
	return fi, err
}

func (w *winFS) Mkdir(string, os.FileMode) error { return os.ErrPermission }
func (w *winFS) Rename(string, string) error     { return os.ErrPermission }
func (w *winFS) Remove(string) error             { return os.ErrPermission }

// readTrack is one WinFsp request in progress (open, stat, readdir, read), for readWatchdog.
type readTrack struct {
	op    string
	path  string
	off   int64
	n     int
	start time.Time
}

var (
	readSeq  atomic.Int64
	inFlight sync.Map // id -> *readTrack
)

// track registers a request in progress; call the returned func when it completes.
func track(op, path string, off int64, n int) func() {
	id := readSeq.Add(1)
	inFlight.Store(id, &readTrack{op, path, off, n, time.Now()})
	return func() { inFlight.Delete(id) }
}

// mounted lists the live WinFsp volumes, for the Ctrl-C path and the watchdog.
var mounted []*winfsp.FileSystem

// readWatchdog logs every request that has been in progress for over 10 s, with a dump of all
// goroutine stacks (every 30 s at most), so a frozen mount leaves evidence of where it stuck:
// our code, a decoder, or the source file. A read stuck past watchdogLimit unmounts and exits:
// WinFsp fails the pending requests when its process goes away, which frees Explorer and
// everything queued behind it.
func readWatchdog() {
	var lastDump time.Time
	for range time.Tick(10 * time.Second) {
		stuck, oldest := 0, time.Duration(0)
		inFlight.Range(func(_, v any) bool {
			if t := v.(*readTrack); time.Since(t.start) > 10*time.Second {
				stuck++
				oldest = max(oldest, time.Since(t.start))
				log.Printf("stuck %s: %s @%d +%d for %s", t.op, t.path, t.off, t.n, time.Since(t.start).Round(time.Second))
			}
			return true
		})
		if stuck > 0 && (time.Since(lastDump) > 30*time.Second || oldest > watchdogLimit) {
			lastDump = time.Now()
			buf := make([]byte, 1<<20)
			log.Printf("goroutine dump (%d stuck reads):\n%s", stuck, buf[:runtime.Stack(buf, true)])
		}
		if watchdogLimit > 0 && oldest > watchdogLimit {
			log.Printf("watchdog: a read has been stuck for %s (--watchdog %s): unmounting and exiting", oldest.Round(time.Second), watchdogLimit)
			done := make(chan struct{})
			go func() {
				for _, m := range mounted {
					m.Unmount()
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				log.Printf("watchdog: unmount did not finish in 10 s, exiting anyway")
			}
			if logFile != nil {
				logFile.Sync()
			}
			os.Exit(3)
		}
	}
}

type winFile struct {
	w      *winFS
	path   string
	info   fs.FileInfo
	r      io.ReaderAt // nil for directories
	closer io.Closer
	off    int64
}

// ReadAt serves a WinFsp read. A read that takes seconds stalls the IRP (and Explorer with it):
// log it, so a hang can be traced to a slow source or a stuck decoder. WinFsp has one dispatcher
// thread per core; when all of them sit in slow reads the kernel queue grows and every program
// touching the volume hangs, so the --debug line also reports how many reads are in progress.
func (f *winFile) ReadAt(p []byte, off int64) (int, error) {
	if f.r == nil {
		return 0, io.EOF
	}
	start := time.Now()
	activeReads.Add(1)
	done := track("read", f.path, off, len(p))
	n, err := f.r.ReadAt(p, off)
	done()
	activeReads.Add(-1)
	if d := time.Since(start); d > 2*time.Second {
		log.Printf("slow read: %s @%d +%d took %s (err %v)", f.path, off, len(p), d.Round(time.Millisecond), err)
	}
	return n, err
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
	defer track("readdir", f.path, 0, 0)()
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
