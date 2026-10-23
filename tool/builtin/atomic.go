package builtin

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// defaultFileMode is the mode of files the tools create, before the umask.
const defaultFileMode fs.FileMode = 0o644

// fileChange is a change to one file within os.Root.
type fileChange struct {
	rel string
	// data is the new content, unless remove is set.
	data   []byte
	remove bool
	// perm is the mode of the new content, or 0 for the default of new
	// files.
	perm fs.FileMode
	// The state to restore if a later change of the same commit fails:
	// whether the file existed, and its content, mode and, for a symbolic
	// link, target.
	existed bool
	old     []byte
	oldPerm fs.FileMode
	link    string
}

// statChange returns a change that writes data to rel, recording the
// current state of the file. old is the file's content if the caller has
// read it, which a commit of several changes needs to restore the file.
func statChange(root *os.Root, rel string, data, old []byte) (fileChange, error) {
	c := fileChange{rel: rel, data: data, old: old}
	lst, err := root.Lstat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c, nil
	case err != nil:
		return c, err
	}
	if lst.Mode()&fs.ModeSymlink != 0 {
		if c.link, err = root.Readlink(rel); err != nil {
			return c, err
		}
	}
	st, err := root.Stat(rel)
	if err != nil {
		return c, err
	}
	if st.IsDir() {
		return c, fmt.Errorf("%s is a directory", filepath.ToSlash(rel))
	}
	c.existed, c.perm, c.oldPerm = true, st.Mode().Perm(), st.Mode().Perm()
	return c, nil
}

// commitHook, when set by tests, runs before each step of a commit and
// fails it with the error it returns.
var commitHook func(rel string) error

// commit stages regular-file writes in their target directories, then
// publishes the writes before applying removals. If a step fails, it attempts
// to undo earlier steps and joins any rollback errors. Every change in a
// multi-file commit must record the previous state needed for rollback.
//
// Symbolic links are written through in place; renames replace hard links
// with new files. Per-file atomicity follows os.Rename, and the changes are
// not isolated from concurrent readers or writers. Parent directories are
// created as needed; cleanup on failure is best effort.
func commit(root *os.Root, changes []fileChange) error {
	var dirs []string // directories created, parents first
	temps := map[int]string{}
	cleanup := func() {
		for _, tmp := range temps {
			_ = root.Remove(tmp)
		}
		for _, dir := range slices.Backward(dirs) {
			_ = root.Remove(dir)
		}
	}
	for i, c := range changes {
		if c.remove {
			continue
		}
		created, err := mkdirs(root, filepath.Dir(c.rel))
		dirs = append(dirs, created...)
		if err != nil {
			cleanup()
			return err
		}
		if c.link != "" {
			continue
		}
		tmp, err := stageTemp(root, c.rel, c.data, c.perm)
		if err != nil {
			cleanup()
			return err
		}
		temps[i] = tmp
	}

	var undo []func() error
	step := func(c fileChange, run func() error, revert func() error) error {
		if commitHook != nil {
			if err := commitHook(c.rel); err != nil {
				return err
			}
		}
		if err := run(); err != nil {
			return err
		}
		undo = append(undo, revert)
		return nil
	}
	restore := func(c fileChange) func() error {
		return func() error { return c.undo(root) }
	}
	var err error
	for _, removals := range []bool{false, true} {
		for i, c := range changes {
			if err != nil || c.remove != removals {
				continue
			}
			switch {
			case c.remove:
				err = step(c, func() error { return root.Remove(c.rel) }, restore(c))
			case c.link != "":
				err = step(c, func() error { return root.WriteFile(c.rel, c.data, 0) }, restore(c))
			default:
				err = step(c, func() error { return root.Rename(temps[i], c.rel) }, restore(c))
				if err == nil {
					delete(temps, i)
				}
			}
			if err != nil {
				err = fmt.Errorf("%s: %w", filepath.ToSlash(c.rel), err)
			}
		}
	}
	if err == nil {
		return nil
	}
	for _, revert := range slices.Backward(undo) {
		if uerr := revert(); uerr != nil {
			err = errors.Join(err, fmt.Errorf("restoring a file failed: %w", uerr))
		}
	}
	cleanup()
	return err
}

// undo restores the recorded state of the file of an applied change.
func (c fileChange) undo(root *os.Root) error {
	switch {
	case !c.existed:
		return root.Remove(c.rel)
	case c.link != "" && c.remove:
		return root.Symlink(c.link, c.rel)
	case c.link != "":
		return root.WriteFile(c.rel, c.old, 0)
	}
	return writeAtomic(root, c.rel, c.old, c.oldPerm)
}

// mkdirs creates dir and its missing parents, returning those it created,
// parents first.
func mkdirs(root *os.Root, dir string) ([]string, error) {
	var missing []string
	for d := dir; d != "." && d != string(filepath.Separator) && d != ""; d = filepath.Dir(d) {
		if _, err := root.Lstat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
	}
	var created []string
	for _, d := range slices.Backward(missing) {
		switch err := root.Mkdir(d, 0o755); {
		case err == nil:
			created = append(created, d)
		case !errors.Is(err, fs.ErrExist):
			return created, err
		}
	}
	return created, nil
}

// stageTemp writes data to a new temporary file next to rel and returns its
// name. perm, if not 0, is set exactly, regardless of the umask.
func stageTemp(root *os.Root, rel string, data []byte, perm fs.FileMode) (string, error) {
	dir, base := filepath.Split(rel)
	for range 100 {
		name := filepath.Join(dir, "."+base+".tmp-"+strconv.FormatUint(rand.Uint64(), 36))
		f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, cmpPerm(perm))
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.Write(data)
		if err == nil && perm != 0 {
			err = f.Chmod(perm)
		}
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = root.Remove(name)
			return "", err
		}
		return name, nil
	}
	return "", fmt.Errorf("cannot create a temporary file for %s", filepath.ToSlash(rel))
}

func cmpPerm(perm fs.FileMode) fs.FileMode {
	if perm == 0 {
		return defaultFileMode
	}
	return perm
}

// writeAtomic replaces rel with data through a temporary file.
func writeAtomic(root *os.Root, rel string, data []byte, perm fs.FileMode) error {
	tmp, err := stageTemp(root, rel, data, perm)
	if err != nil {
		return err
	}
	if err := root.Rename(tmp, rel); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
