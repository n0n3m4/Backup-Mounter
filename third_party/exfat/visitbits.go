package filesystem_exfat

import (
	"fmt"

	"github.com/go-volumes/safeio"
)

// visitBits replaces safeio.VisitSet (a map, ~700 B per entry with growth) for cluster-chain
// walks: one bit per cluster, grown on demand, so opening a 200 GB file costs megabytes, not a
// gigabyte of garbage per open. backup-mounter addition.
type visitBits struct{ bits []uint64 }

// Check records id and returns safeio.ErrCycle when it was seen before.
func (v *visitBits) Check(id uint64) error {
	w := id / 64
	if w >= uint64(len(v.bits)) {
		grown := make([]uint64, max(2*len(v.bits), int(w)+1))
		copy(grown, v.bits)
		v.bits = grown
	}
	if v.bits[w]&(1<<(id%64)) != 0 {
		return fmt.Errorf("%w: cluster %d revisited", safeio.ErrCycle, id)
	}
	v.bits[w] |= 1 << (id % 64)
	return nil
}
