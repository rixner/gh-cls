//go:build !linux

package cmd

// renameNoReplace moves oldpath onto newpath, failing rather than replace
// anything already there.
//
// Only Linux offers a rename that refuses an existing destination outright
// (RENAME_NOREPLACE). Everywhere else this is a check followed by a move, which
// leaves a window in which an empty directory created in between is replaced
// silently. macOS has renameatx_np with RENAME_EXCL and Windows needs its own
// handling; both are worth doing if collect is ever run there in earnest.
func renameNoReplace(oldpath, newpath string) error {
	return renameChecked(oldpath, newpath)
}
