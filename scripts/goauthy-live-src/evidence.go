package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This records the checked-out sources observed by the fixture; the image ID is separately pinned.
func (f *fixture) sourceEvidence(repo string) error {
	head, e := f.command(repo, nil, nil, "git", "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	status, e := f.command(repo, nil, nil, "git", "status", "--porcelain")
	if e != nil {
		return e
	}
	files, e := f.command(repo, nil, nil, "git", "ls-files", "-c", "-o", "--exclude-standard", "-z")
	if e != nil {
		return e
	}
	names := strings.Split(string(files), "\x00")
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		if name == "" {
			continue
		}
		path := filepath.Join(repo, name)
		info, e := os.Lstat(path)
		if os.IsNotExist(e) {
			fmt.Fprintf(h, "deleted %s\n", name)
			continue
		}
		if e != nil {
			return e
		}
		fmt.Fprintf(h, "%s %s\n", name, info.Mode())
		if info.Mode()&os.ModeSymlink != 0 {
			link, e := os.Readlink(path)
			if e != nil {
				return e
			}
			fmt.Fprintln(h, link)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		file, e := os.Open(path)
		if e != nil {
			return e
		}
		_, e = io.Copy(h, file)
		closeErr := file.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
	}
	fmt.Printf("SOURCE %s HEAD=%s dirty=%t working-tree-sha256=%x\n", filepath.Base(repo), strings.TrimSpace(string(head)), len(status) > 0, h.Sum(nil))
	return nil
}
