package scanner

import (
	"io/fs"
	"os"
	"path/filepath"
)

// walkFollow is filepath.WalkDir that follows symbolic links (and Windows
// junctions) to folders and files, so a library folder that is itself a
// link, or has linked folders inside it, is scanned like a real one.
// Paths stay as they are under start; links aren't resolved in them, so a
// file keeps the same library path whatever its link points at. A folder
// whose real location was already walked is skipped, so a link loop ends.
//
// fn is called as WalkDir calls it: once for each entry with a nil error,
// and once more for a folder that couldn't be read, with that error. A link
// that can't be followed is passed with its error. For an entry reached
// through a link, d describes what the link points at.
func walkFollow(start string, fn fs.WalkDirFunc) error {
	info, err := os.Stat(start)
	if err != nil {
		return fn(start, nil, err)
	}
	seen := map[string]bool{}
	err = walkEntry(start, fs.FileInfoToDirEntry(info), seen, fn)
	if err == filepath.SkipDir || err == filepath.SkipAll {
		return nil
	}
	return err
}

func walkEntry(path string, d fs.DirEntry, seen map[string]bool, fn fs.WalkDirFunc) error {
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if err == filepath.SkipDir && d.IsDir() {
			return nil
		}
		return err
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		if abs, err := filepath.Abs(real); err == nil {
			real = abs
		}
		if seen[real] {
			return nil
		}
		seen[real] = true
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		if err := fn(path, d, err); err != nil {
			if err == filepath.SkipDir {
				return nil
			}
			return err
		}
		return nil
	}
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if e.Type()&fs.ModeSymlink != 0 || e.Type()&fs.ModeIrregular != 0 {
			// A link (or a junction, which Go may report as irregular):
			// describe what it points at.
			info, err := os.Stat(child)
			if err != nil {
				if err := fn(child, e, err); err != nil && err != filepath.SkipDir {
					return err
				}
				continue
			}
			e = fs.FileInfoToDirEntry(info)
		}
		if err := walkEntry(child, e, seen, fn); err != nil {
			if err == filepath.SkipDir {
				// Skipping a file means "skip the rest of this folder".
				if !e.IsDir() {
					return nil
				}
				continue
			}
			return err
		}
	}
	return nil
}
