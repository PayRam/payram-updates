package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureToken_CreatesARootOnlyTokenOnceAndKeepsIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")

	first, err := EnsureToken(dir)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if len(first) != 64 {
		t.Fatalf("token %q, want 32 random bytes in hex", first)
	}
	info, err := os.Stat(filepath.Join(dir, tokenFileName))
	if err != nil {
		t.Fatalf("token file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token file mode %o, want 600 (root only)", info.Mode().Perm())
	}

	// A restarted daemon keeps the token, so a CLI command that read it
	// before the restart still works.
	again, err := EnsureToken(dir)
	if err != nil || again != first {
		t.Fatalf("second EnsureToken = %q, %v; want the same token", again, err)
	}
	read, err := ReadToken(dir)
	if err != nil || read != first {
		t.Fatalf("ReadToken = %q, %v; want the daemon's token", read, err)
	}
}

func TestTokenMatches(t *testing.T) {
	if !TokenMatches("abc", "abc\n") {
		t.Error("the token read from the file (with its newline) must match")
	}
	for _, got := range []string{"", "abd", "ab"} {
		if TokenMatches("abc", got) {
			t.Errorf("TokenMatches(abc, %q) = true", got)
		}
	}
	if TokenMatches("", "") {
		t.Error("a daemon without a token must never grant manual mode")
	}
}

func TestReadToken_MissingFileIsAnError(t *testing.T) {
	if _, err := ReadToken(t.TempDir()); err == nil {
		t.Fatal("ReadToken without a token file must fail")
	}
}
