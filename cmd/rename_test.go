package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These run against the real filesystem. The unit tests reach renameNoReplace
// through the fake runner, which only re-keys a map, so nothing there would
// notice if the move started replacing directories instead of refusing them.
// This is the guarantee collect rests on: it never moves or deletes anything
// under --out, so a move onto an occupied name has to fail rather than resolve
// itself.

func TestRenameNoReplaceMovesOntoAFreeName(t *testing.T) {
	base := t.TempDir()
	src := filepath.Join(base, "staging")
	dst := filepath.Join(base, "ada")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("collected\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := renameNoReplace(src, dst); err != nil {
		t.Fatalf("a free name should accept the move: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dst, "f")); err != nil || string(body) != "collected\n" {
		t.Errorf("the clone should have arrived intact, got %q %v", body, err)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Error("the staging directory should be gone once it has been moved")
	}
}

func TestRenameNoReplaceRefusesAnythingAlreadyThere(t *testing.T) {
	// The empty-directory case is the one that matters most: plain rename(2)
	// replaces an empty target silently, so a check alone would let collect
	// destroy a directory someone had made.
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dst string)
	}{
		{"an empty directory", func(t *testing.T, dst string) {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory holding work", func(t *testing.T, dst string) {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dst, "notes.txt"), []byte("grader\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a plain file", func(t *testing.T, dst string) {
			if err := os.WriteFile(dst, []byte("grader\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			src := filepath.Join(base, "staging")
			dst := filepath.Join(base, "ada")
			if err := os.MkdirAll(src, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, "f"), []byte("collected\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, dst)

			err := renameNoReplace(src, dst)
			if !errors.Is(err, errTargetExists) {
				t.Fatalf("the move should be refused as an occupied target, got %v", err)
			}
			// Both sides survive: collect reports and leaves the decision alone.
			if _, statErr := os.Stat(filepath.Join(src, "f")); statErr != nil {
				t.Errorf("the staged clone should still be there after a refusal: %v", statErr)
			}
			if _, statErr := os.Lstat(dst); statErr != nil {
				t.Errorf("the target should be untouched after a refusal: %v", statErr)
			}
		})
	}
}
