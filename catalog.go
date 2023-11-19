// This tool takes the most recent files from src and copies that to dst.
// $ time go run catalog.go --src=/tank/photos/ --dst=/media/keisuke/PHOTOS_A/
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	src = flag.String("src", "", "")
	dst = flag.String("dst", "", "")
)

// stat returns the capacity of the storage corresponding to dir.
func stat(dir string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(dir, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Blocks) * stat.Bsize, nil
}

type file struct {
	dir     string
	base    string
	size    int64
	modTime time.Time
}

func (f *file) path() string {
	return filepath.Join(f.dir, f.base)
}

func scan(dir string) ([]*file, error) {
	var files []*file
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		i, err := d.Info()
		if err != nil {
			return err
		}
		relPath := path[len(dir):]
		files = append(files, &file{
			dir:     filepath.Dir(relPath),
			base:    filepath.Base(relPath),
			size:    i.Size(),
			modTime: i.ModTime(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, err
}

func mostRecent(files []*file, cap int64) []*file {
	slices.SortFunc(files, func(a, b *file) int {
		return b.modTime.Compare(a.modTime)
	})
	var totalSize int64
	var ret []*file
	for _, f := range files {
		if (totalSize+f.size)*20 > cap*19 { // 95%
			break
		}
		totalSize += f.size
		ret = append(ret, f)
	}
	log.Printf("Total size to be kept: %d (cap: %d)\n", totalSize, cap)
	return ret
}

// This function is currently unsed.
func duplicates(files []*file) {
	type key struct {
		base string
		size int64
	}
	sm := make(map[key]int64)
	dm := make(map[key][]string)
	for _, f := range files {
		k := key{f.base, f.size}
		sm[k]++
		dm[k] = append(dm[k], f.dir)
	}
	var totalDuplicateSize int64
	for k, v := range sm {
		if v == 1 {
			continue
		}
		/*
			fmt.Printf("Duplicate: %s %d (%d copies)\n", k.base, k.size, v)
			for _, d := range dm[k] {
				fmt.Println("-", d)
			}
		*/
		totalDuplicateSize += k.size * (v - 1)
	}
	fmt.Printf("Total duplicate size: %d\n", totalDuplicateSize)
}

func compare(src, dst []*file) (add, sub []*file) {
	sm := make(map[string]bool)
	dm := make(map[string]bool)
	for _, f := range src {
		sm[f.path()] = true
	}
	for _, f := range dst {
		dm[f.path()] = true
	}

	for _, f := range src {
		if !dm[f.path()] {
			add = append(add, f)
		}
	}
	for _, f := range dst {
		if !sm[f.path()] {
			sub = append(sub, f)
		}
	}
	return
}

func removeEmptyDirs(dir string) error {
	// Process directories in the opposite order as WalkDir so that we can
	// recursively delete empty directories in one path.
	var dirs []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		// https://stackoverflow.com/questions/30697324/how-to-check-if-directory-on-path-is-empty
		empty, err := func() (bool, error) {
			f, err := os.Open(dirs[i])
			if err != nil {
				return false, err
			}
			defer f.Close()
			_, err = f.Readdirnames(1)
			if err == io.EOF {
				return true, nil
			}
			return false, err // Either not empty or error, suits both cases
		}()
		if err != nil {
			return err
		}
		if empty {
			fmt.Printf("deleting empty dir %s\n", dirs[i])
			if err := os.Remove(dirs[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

func updateDirAttributes() error {
	return filepath.WalkDir(*src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}

		si, err := d.Info()
		if err != nil {
			return err
		}

		relPath := path[len(*src):]
		dstPath := filepath.Join(*dst, relPath)
		di, err := os.Stat(dstPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			} else {
				return err
			}
		}

		if ss, ok := si.Sys().(*syscall.Stat_t); ok {
			if ds, ok := di.Sys().(*syscall.Stat_t); ok {
				if d := si.ModTime().Sub(di.ModTime()); d < -time.Second || time.Second < d { // approx equal?
					toTime := func(t syscall.Timespec) time.Time {
						return time.Unix(t.Sec, t.Nsec)
					}
					atim := toTime(ss.Atim)
					mtim := toTime(ss.Mtim)
					fmt.Printf("chtimes %s (atim:%s=>%s, mtim:%s=>%s)\n",
						relPath, toTime(ds.Atim), atim, toTime(ds.Mtim), mtim)
					if err := os.Chtimes(dstPath, atim, mtim); err != nil {
						return err
					}
				}
				// TODO: Figure out if I want to do this. There are many source
				// directories with weird mode, and it feels I might as well use 0755
				// everywhere.
				if false {
					if si.Mode() != di.Mode() {
						fmt.Printf("chmod %s (%s => %s)\n", relPath, di.Mode(), si.Mode())
						if false {
							if err := os.Chmod(dstPath, si.Mode()); err != nil {
								return err
							}
						}
					}
				}
			}
		}

		return nil
	})
}

func run() error {
	files, err := scan(*src)
	if err != nil {
		return err
	}
	cap, err := stat(*dst)
	if err != nil {
		return err
	}
	srcFiles := mostRecent(files, cap)
	dstFiles, err := scan(*dst)
	if err != nil {
		return err
	}
	add, sub := compare(srcFiles, dstFiles)

	for _, f := range sub {
		path := filepath.Join(*dst, f.path())
		fmt.Printf("deleting %s\n", path)
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if err := removeEmptyDirs(*dst); err != nil {
		return err
	}

	file, err := os.CreateTemp("", "*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	for _, f := range add {
		fmt.Fprintln(file, f.path())
	}
	cmd := exec.Command("rsync", "-Pav", "--mkpath", "--files-from="+file.Name(), *src, *dst)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	if err := updateDirAttributes(); err != nil {
		return err
	}

	return nil
}

func main() {
	flag.Parse()
	*src = filepath.Clean(*src)
	*dst = filepath.Clean(*dst)
	if err := run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
