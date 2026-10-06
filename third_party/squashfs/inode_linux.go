//go:build linux && fuse

package squashfs

import (
	"github.com/hanwen/go-fuse/v2/fuse"
)

func (i *Inode) FillAttr(attr *fuse.Attr) error {
	attr.Size = i.Size
	attr.Blocks = uint64(i.blocks.len()) + 1 // backup-mounter: lazy block index
	attr.Mode = modeToUnix(i.Mode())
	attr.Nlink = i.NLink // 1 required
	attr.Rdev = 1
	attr.Blksize = i.sb.BlockSize
	attr.Atime = uint64(i.ModTime)
	attr.Mtime = uint64(i.ModTime)
	attr.Ctime = uint64(i.ModTime)
	// fill uid/gid based on idtable
	if len(i.sb.idTable) >= int(i.UidIdx) {
		attr.Uid = i.sb.idTable[i.UidIdx]
	}
	if len(i.sb.idTable) >= int(i.GidIdx) {
		attr.Gid = i.sb.idTable[i.GidIdx]
	}
	return nil
}
