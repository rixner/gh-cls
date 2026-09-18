//go:build linux

package cmd

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// renameNoReplace moves oldpath onto newpath and fails rather than replace
// anything already there.
//
// RENAME_NOREPLACE is what makes the guarantee the kernel's rather than a
// check's. Plain rename(2) refuses a non-empty directory but replaces an empty
// one in silence, and a directory can appear between collect's pre-check and
// the move; with the flag, a destination that exists at all fails with EEXIST.
// Collect can then not replace a directory even if its own pre-check was wrong
// or lost a race.
func renameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY):
		return fmt.Errorf("%w: %s", errTargetExists, newpath)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP):
		// The filesystem does not carry the flag (some network and union mounts).
		// Fall back rather than fail: the window reopens, which is stated.
		return renameChecked(oldpath, newpath)
	default:
		return fmt.Errorf("moving %s into place at %s: %w", oldpath, newpath, err)
	}
}
