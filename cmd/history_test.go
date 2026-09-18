package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryModeRoundTrips(t *testing.T) {
	out := t.TempDir()
	if _, found, err := loadHistoryMode(out); err != nil || found {
		t.Fatalf("a directory with no setting should report none, got found=%v err=%v", found, err)
	}
	if err := saveHistoryMode(out, historyFull); err != nil {
		t.Fatalf("save: %v", err)
	}
	mode, found, err := loadHistoryMode(out)
	if err != nil || !found || mode != historyFull {
		t.Fatalf("expected the recorded setting back, got %q found=%v err=%v", mode, found, err)
	}
}

func TestHistoryModeRejectsAFileItCannotTrust(t *testing.T) {
	// The setting decides which fetch collect runs, and the wrong fetch destroys
	// history. A file that cannot be read for certain has to stop the run rather
	// than fall back to a default that might be the destructive one.
	for _, tc := range []struct {
		name, body string
		wants      []string
	}{
		{"malformed", "history: [not, a, string\n", []string{"fix or remove"}},
		{"an unknown setting", "history: everything\n", []string{"everything", "fix or remove"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			dir := filepath.Join(out, gitCLSDir)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, historyFileName), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}

			_, _, err := loadHistoryMode(out)
			if err == nil {
				t.Fatal("a setting that cannot be trusted should fail the run")
			}
			t.Log("\n" + err.Error())
			// The message has to name the file, since the fix is to edit it.
			if !strings.Contains(err.Error(), historyFileName) {
				t.Errorf("the error should name the file, got: %v", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error should mention %q, got: %v", want, err)
				}
			}
		})
	}
}

func TestResolveHistorySaysWhereTheSettingCameFrom(t *testing.T) {
	// The header prints this, and it is the only thing telling an instructor
	// whether the run is about to change a directory's setting or inherit one.
	for _, tc := range []struct {
		name        string
		recorded    historyMode // "" for a directory with no setting
		flag        string
		wantMode    historyMode
		wantSource  string
		wantChanged historyMode
	}{
		{name: "nothing recorded, no flag", wantMode: historySnapshot, wantSource: "the default"},
		{name: "recorded, no flag", recorded: historyFull, wantMode: historyFull, wantSource: "recorded for this directory"},
		{name: "flag on a fresh directory", flag: "full", wantMode: historyFull, wantSource: "set on this directory by this run"},
		{name: "flag agreeing with the record", recorded: historyFull, flag: "full", wantMode: historyFull, wantSource: "recorded for this directory"},
		{name: "flag changing the record", recorded: historyFull, flag: "snapshot", wantMode: historySnapshot,
			wantSource: "changed from full by this run", wantChanged: historyFull},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			if tc.recorded != "" {
				if err := saveHistoryMode(out, tc.recorded); err != nil {
					t.Fatal(err)
				}
			}
			mode, source, changed, err := resolveHistory(out, tc.flag)
			if err != nil {
				t.Fatalf("resolveHistory: %v", err)
			}
			if mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", mode, tc.wantMode)
			}
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if changed != tc.wantChanged {
				t.Errorf("changedFrom = %q, want %q", changed, tc.wantChanged)
			}
		})
	}
}

func TestResolveHistoryRejectsAnUnknownFlag(t *testing.T) {
	_, _, _, err := resolveHistory(t.TempDir(), "everything")
	if err == nil {
		t.Fatal("an unknown --history value should be refused")
	}
	for _, want := range []string{"everything", "snapshot", "full"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q, got: %v", want, err)
		}
	}
}
