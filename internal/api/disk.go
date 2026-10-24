package api

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"time"
)

// DiskDTO is the disk space the daemon's storage takes and what is left on its file system.
type DiskDTO struct {
	// RegistryBytes is the size of the registry storage (blobs are shared between images, so
	// this is what removing everything would free at most).
	RegistryBytes int64 `json:"registry_bytes"`
	// FreeBytes and TotalBytes describe the file system the storage lives on.
	FreeBytes  uint64 `json:"free_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

// diskUsage measures dir. Walking a large registry is slow, so the size is cached for a
// while: `status --watch` asks every second. Free space is cheap and always fresh.
type diskUsage struct {
	dir string
	ttl time.Duration

	mu     sync.Mutex
	at     time.Time
	cached int64
}

func newDiskUsage(dir string) *diskUsage {
	if dir == "" {
		return nil
	}
	return &diskUsage{dir: dir, ttl: 30 * time.Second}
}

func (u *diskUsage) measure(now time.Time) *DiskDTO {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	if u.at.IsZero() || now.Sub(u.at) >= u.ttl {
		u.cached, u.at = dirSize(u.dir), now
	}
	size := u.cached
	u.mu.Unlock()
	d := &DiskDTO{RegistryBytes: size}
	d.FreeBytes, d.TotalBytes = fsSpace(u.dir)
	return d
}

// dirSize adds up the sizes of the regular files below dir; unreadable entries count as 0.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// HumanBytes formats a size in binary units, e.g. "3.2 GiB".
func HumanBytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, unit := float64(n), ""
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		f /= 1024
		unit = u
		if f < 1024 {
			break
		}
	}
	return fmt.Sprintf("%.1f %s", f, unit)
}

// Summary is the one-line text of the disk usage, shared by the CLI and the web page.
func (d DiskDTO) Summary() string {
	s := "registry " + HumanBytes(uint64(d.RegistryBytes))
	if d.TotalBytes > 0 {
		s += fmt.Sprintf(", %s free of %s", HumanBytes(d.FreeBytes), HumanBytes(d.TotalBytes))
	}
	return s
}
