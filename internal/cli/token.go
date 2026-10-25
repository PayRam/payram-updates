package cli

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TokenHeader carries the CLI token on requests to the daemon.
const TokenHeader = "X-Payram-Updater-Token"

// tokenFileName is the token's file in the updater's state directory, which
// only root can read: a request that carries the token was made by root.
const tokenFileName = "cli-token"

// ErrTokenUnreadable is returned when this user cannot read the token (the
// command was not run as root).
var ErrTokenUnreadable = errors.New("the updater's CLI token is readable by root only; run the command with sudo")

// EnsureToken returns the daemon's CLI token, creating it the first time
// (mode 0600 in stateDir). Manual mode skips the release policy's checks, so
// the daemon grants it only to requests that carry this token, that is to
// root's command line, not to any program on the server that can reach
// 127.0.0.1.
func EnsureToken(stateDir string) (string, error) {
	if token, err := ReadToken(stateDir); err == nil {
		return token, nil
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", fmt.Errorf("could not create %s: %w", stateDir, err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("could not generate the CLI token: %w", err)
	}
	token := hex.EncodeToString(raw)
	path := tokenPath(stateDir)
	tmp, err := os.CreateTemp(stateDir, ".cli-token-*")
	if err != nil {
		return "", fmt.Errorf("could not write the CLI token: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return "", fmt.Errorf("could not restrict the CLI token: %w", err)
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		tmp.Close()
		return "", fmt.Errorf("could not write the CLI token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("could not write the CLI token: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("could not write the CLI token: %w", err)
	}
	return token, nil
}

// ReadToken reads the CLI token from stateDir.
func ReadToken(stateDir string) (string, error) {
	data, err := os.ReadFile(tokenPath(stateDir))
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", ErrTokenUnreadable
		}
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("the CLI token in %s is empty", tokenPath(stateDir))
	}
	return token, nil
}

// TokenMatches reports whether a request's token is the daemon's.
func TokenMatches(want, got string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(got))) == 1
}

func tokenPath(stateDir string) string {
	return filepath.Join(stateDir, tokenFileName)
}
