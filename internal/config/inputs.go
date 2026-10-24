package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InputsHash fingerprints everything in a container that decides what its image contains:
// every file in the container directory except container.yaml (Dockerfile, COPY sources,
// .dockerignore, with their executable bits), the build args of the flavor, the flavor name,
// and depends_on (it decides the PARENT args). Settings that only steer the scheduler or the
// tests (intervals, keep, tests, refresh_cmd) are deliberately left out: changing them does
// not make an image stale.
//
// The daemon stores the hash of every build and rebuilds at once when the hash of the
// current commit differs, so an image never lags behind git.
func InputsHash(containerDir string, c *Container, flavor string) (string, error) {
	h := sha256.New()
	var files []string
	err := filepath.WalkDir(containerDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(containerDir, p)
		if err != nil {
			return err
		}
		if rel == ContainerFile {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, rel := range files {
		full := filepath.Join(containerDir, rel)
		info, err := os.Stat(full)
		if err != nil {
			return "", err
		}
		b, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(b)
		fmt.Fprintf(h, "file %s %o %x\n", filepath.ToSlash(rel), info.Mode().Perm()&0o111, sum)
	}
	fmt.Fprintf(h, "flavor %s\n", flavor)
	args := c.Flavors[flavor].Args
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "arg %s=%s\n", k, args[k])
	}
	deps := append([]string(nil), c.DependsOn...)
	sort.Strings(deps)
	for _, d := range deps {
		fmt.Fprintf(h, "parent %s\n", d)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RefreshHash fingerprints a container's refresh_cmd. A rebuild verifies the command and records
// this value; a different command later makes the image "not verified" and triggers a rebuild,
// which verifies it. No command at all has the fixed fingerprint "none".
func RefreshHash(refreshCmd string) string {
	cmd := strings.TrimSpace(refreshCmd)
	if cmd == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(cmd))
	return hex.EncodeToString(sum[:])
}
