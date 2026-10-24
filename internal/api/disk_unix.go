//go:build unix

package api

import "syscall"

// fsSpace returns the free and total bytes of the file system holding dir (0, 0 when unknown).
func fsSpace(dir string) (free, total uint64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize)
}
