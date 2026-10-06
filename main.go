// Command backup-mounter mounts every filesystem inside a disk image (raw,
// VHD/VHDX, bare partition, any of those inside a squashfs backup, or a raw/VHD
// image compressed with gzip or multi-block xz, an Acronis True Image .tib
// partition backup, or a qcow2 or VMDK virtual disk) as a
// read-only Windows drive (via WinFsp), without extracting anything. A tar
// (plain, .gz or multi-block .xz) or 7z archive is mounted as one drive, as is
// an EROFS image (or the disk images inside it, with --image).
package main

import (
	"bytes"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	rtdebug "runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/KarpelesLab/squashfs"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func main() {
	cacheFlag := flag.String("cache", "4G", "RAM cache budget (e.g. 8G, 512M)")
	aheadFlag := flag.String("readahead", "16M", "decode-ahead window per file for squashfs/.tib/erofs (0 = off); more keeps more cores busy")
	var images multiFlag
	flag.Var(&images, "image", "disk image inside the squashfs/erofs (repeatable; squashfs default: auto-detect, erofs default: the erofs itself as a folder)")
	walk := flag.Bool("walk", false, "print path, size and sha256 of every file instead of mounting")
	debug := flag.Bool("debug", false, "verbose WinFsp logging (the WinFsp request trace goes to --log when set, else to the debugger channel)")
	logPath := flag.String("log", "", "append the log to this file instead of the console (a console blocks writers while text is selected)")
	flag.DurationVar(&watchdogLimit, "watchdog", time.Minute, "unmount and exit when a request has been stuck this long (0 = never); the log gets a goroutine dump first")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] backup.squashfs|disk.vhd|disk.vhdx|disk.img|disk.img.gz|disk.img.xz|backup.tib|disk.qcow2|disk.vmdk|backup.tar[.gz|.xz]|backup.7z|backup.erofs\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	budget, err := parseSize(*cacheFlag)
	if err != nil {
		log.Fatalf("--cache: %v", err)
	}
	if readAheadBytes, err = parseSize(*aheadFlag); err != nil {
		log.Fatalf("--readahead: %v", err)
	}
	if *logPath != "" {
		if logFile, err = os.OpenFile(*logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err != nil {
			log.Fatalf("--log: %v", err)
		}
		fmt.Fprintf(os.Stderr, "logging to %s\n", *logPath)
		log.SetOutput(logFile)
		log.Printf("backup-mounter %v", os.Args[1:])
	}
	// The caches are the live heap; by default Go's GC lets the heap reach twice that before
	// collecting (an 8G cache showed as 18 GB RSS). 25% keeps RSS near 1.25 x live; unlike a memory
	// limit it cannot make the GC run nonstop when the live set is larger than expected.
	// Cache data is pointer-free, so collecting often is cheap.
	rtdebug.SetGCPercent(25)
	vols, err := openStack(flag.Arg(0), budget, images)
	if err != nil {
		log.Fatal(err)
	}
	if len(vols) == 0 {
		log.Fatal("no readable filesystems found")
	}
	for _, v := range vols {
		log.Printf("%s: %s, label %q, %d MiB", v.name, v.kind, v.label, v.size>>20)
	}
	if *debug {
		go memLog(budget) // walk and mount alike
	}
	if *walk {
		for _, v := range vols {
			if err := walkVolume(os.Stdout, v); err != nil {
				log.Fatalf("%s: %v", v.name, err)
			}
		}
		return
	}
	if err := mount(vols, *debug); err != nil {
		log.Fatal(err)
	}
}

// openStack: file -> raw chunk cache -> squashfs -> disk images ->
// decompressed chunk cache -> VHD/VHDX unwrap -> partitions -> volumes.
// A file that is not a squashfs is treated as the disk image itself, after
// gzip/xz decompression (compressed.go) when it is one of those; a 7z or a
// (decompressed) tar becomes a single archive volume instead (archive.go, sevenzip.go),
// an EROFS one folder volume or its --image files (erofs.go, erofs_z.go).
func openStack(path string, budget int64, images []string) ([]volume, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var magic [6]byte
	if n, _ := f.ReadAt(magic[:], 0); string(magic[:min(n, 4)]) != "hsqs" { // squashfs little-endian magic
		if isErofs(f) {
			return openErofs(path, f, st, budget, images)
		}
		if len(images) > 0 {
			return nil, fmt.Errorf("%s: not a squashfs or erofs, --image does not apply", path)
		}
		if bytes.HasPrefix(magic[:n], sevenMagic) {
			v, err := open7z(path, f, st, budget)
			if err != nil {
				return nil, err
			}
			return []volume{v}, nil
		}
		r, size, ok, err := openCompressed(path, f, st, budget)
		if err != nil {
			return nil, err
		}
		var src io.ReaderAt = r // where a tar index pass reads headers: the bare file when uncompressed
		if !ok {
			r, size, src = newChunkCache(f, st.Size(), 4<<20, budget), st.Size(), f
		}
		if isTar(r) {
			v, err := openTar(path, st, src, r, size)
			if err != nil {
				return nil, err
			}
			return []volume{v}, nil
		}
		return openVolumes(filepath.Base(path), r, size), nil
	}
	sq, err := squashfs.New(newChunkCache(f, st.Size(), 4<<20, budget/8*7))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	imgs, err := findImages(filepath.Base(path), sq, images)
	if err != nil {
		return nil, err
	}
	if len(imgs) == 0 {
		return nil, fmt.Errorf("%s: no disk images found (use --image)", path)
	}
	var vols []volume
	for _, img := range imgs {
		size := int64(img.ino.Size)
		r := newChunkCache(img.ino, size, int64(sq.BlockSize), budget/8/int64(len(imgs))).withAhead()
		vols = append(vols, openVolumes(img.name, r, size)...)
	}
	return vols, nil
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch strings.ToUpper(s[len(s)-min(len(s), 1):]) {
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

// walkVolume prints "<volume>/<path> <size> <sha256>" for every regular file.
func walkVolume(w io.Writer, v volume) error {
	return fs.WalkDir(v.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := v.fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		_, err = fmt.Fprintf(w, "%s/%s %d %x\n", v.name, p, n, h.Sum(nil))
		return err
	})
}

// memLog (--debug) reports every 10 s what the Go runtime holds (heap live, total from the OS
// split into heap / stacks / runtime structures), goroutines, WinFsp reads in progress and the
// caches, so a memory complaint can be told apart from GC headroom, OS caches or kernel memory.
// A tripwire polls the cheap runtime/metrics total every 250 ms: the first time it exceeds twice
// the cache budget by over 1 GiB it writes a heap profile next to the log (heap-<unix>.pprof,
// read with `go tool pprof -top -sample_index=alloc_space`) and logs every goroutine's stack, so
// a sudden file-sized allocation is caught in the act even if the process then stalls.
func memLog(budget int64) {
	var ms runtime.MemStats
	sample := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	tripped := false
	tick := time.NewTicker(250 * time.Millisecond)
	for i := 0; ; i++ {
		<-tick.C
		metrics.Read(sample)
		if total := int64(sample[0].Value.Uint64()); !tripped && total > 2*budget+1<<30 {
			tripped = true
			name := fmt.Sprintf("heap-%d.pprof", time.Now().Unix())
			if logFile != nil {
				name = filepath.Join(filepath.Dir(logFile.Name()), name)
			}
			if f, err := os.Create(name); err == nil {
				pprof.Lookup("heap").WriteTo(f, 0)
				f.Close()
			}
			buf := make([]byte, 1<<20)
			log.Printf("mem: runtime total %d MiB is far above the %d MiB budget: heap profile %s; goroutines:\n%s",
				total>>20, budget>>20, name, buf[:runtime.Stack(buf, true)])
		}
		if i%40 != 0 { // the full line every 10 s
			continue
		}
		runtime.ReadMemStats(&ms)
		other := ms.Sys - ms.HeapSys - ms.StackSys
		log.Printf("mem: heap %d MiB live, %d MiB from OS (heap %d, stacks %d, runtime %d, %d released), %d GCs, %d goroutines, %d reads in progress; caches:%s",
			ms.HeapAlloc>>20, ms.Sys>>20, ms.HeapSys>>20, ms.StackSys>>20, other>>20, ms.HeapReleased>>20, ms.NumGC, runtime.NumGoroutine(), activeReads.Load(), cacheReport())
	}
}
