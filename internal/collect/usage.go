package collect

import "syscall"

// diskUsage returns the total and used bytes of the filesystem holding path.
//
// Used is computed from Bfree rather than Bavail, matching df: the difference is
// the space reserved for root (5% by default on ext4), which is neither free to
// an ordinary process nor accounted as used. This is why a filesystem can report
// itself full while df still shows a few gigabytes of total capacity unused.
func diskUsage(path string) (total, used uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}

	// Bsize is uint32 on darwin and int64 on linux, so it is widened explicitly
	// rather than relying on either platform's native type.
	blockSize := uint64(st.Bsize)

	return st.Blocks * blockSize, (st.Blocks - st.Bfree) * blockSize, nil
}
