package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// errTargetExists reports that a rename was refused because something was
// already at the destination. Collect never moves or deletes anything under
// --out, so this is always a refusal to report and never a case to clear.
var errTargetExists = errors.New("a directory is already there")

// renameChecked is the portable fallback for renameNoReplace: look, then move.
//
// It cannot give the same guarantee. Plain rename(2) replaces an empty
// directory silently, so a directory appearing between the check and the move
// is still lost, and nothing but the kernel can close that window. On Linux the
// build-tagged implementation uses RENAME_NOREPLACE instead and has no window
// at all.
func renameChecked(oldpath, newpath string) error {
	if _, err := os.Lstat(newpath); err == nil {
		return fmt.Errorf("%w: %s", errTargetExists, newpath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking %s before moving a new clone into place: %w", newpath, err)
	}
	return os.Rename(oldpath, newpath)
}
