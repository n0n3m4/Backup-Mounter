module github.com/n0n3m4/backup-mounter

go 1.26.5

replace github.com/go-filesystems/btrfs => ./third_party/btrfs

replace github.com/go-filesystems/exfat => ./third_party/exfat

replace github.com/go-filesystems/fat32 => ./third_party/fat32

require (
	github.com/KarpelesLab/squashfs v1.2.0
	github.com/aoiflux/libext v0.3.0
	github.com/aoiflux/libvhdi v0.3.0
	github.com/go-filesystems/btrfs v0.1.0
	github.com/go-filesystems/exfat v0.3.1-0.20260924072827-4792096f9a7f
	github.com/go-filesystems/fat32 v0.4.0
	github.com/go-filesystems/interface v0.3.0
	github.com/go-volumes/gpt v0.2.0
	github.com/unxed/xz v0.1.47
	github.com/winfsp/go-winfsp v1.0.4
	golang.org/x/sys v0.48.0
	www.velocidex.com/golang/go-ntfs v0.2.1
)

require (
	github.com/Velocidex/json v0.0.0-20220224052537-92f3c0326e5a // indirect
	github.com/Velocidex/ordereddict v0.0.0-20230909174157-2aa49cc5d11d // indirect
	github.com/Velocidex/yaml/v2 v2.2.8 // indirect
	github.com/anchore/go-lzo v0.1.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-volumes/safeio v0.0.0-20260831125406-d8f54b2890d4 // indirect
	github.com/hanwen/go-fuse/v2 v2.1.0 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.32 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rogpeppe/go-internal v1.16.0 // indirect
	github.com/ulikunitz/xz v0.5.10 // indirect
)

replace github.com/KarpelesLab/squashfs => ./third_party/squashfs
