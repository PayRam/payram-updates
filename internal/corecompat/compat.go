package corecompat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/payram/payram-updater/internal/container"
)

const (
	legacyHealthMarker = "Welcome to Payram Core"
	maxResponseSize    = 1 * 1024 * 1024
)

// NormalizeVersion trims whitespace and a leading "v" prefix.
func NormalizeVersion(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "v")
	return value
}

// IsBeforeInit returns true when currentVersion is lower than initVersion.
func IsBeforeInit(currentVersion, initVersion string) (bool, error) {
	initVersion = NormalizeVersion(initVersion)
	if initVersion == "" {
		return false, nil
	}
	currentVersion = NormalizeVersion(currentVersion)
	if currentVersion == "" {
		return false, errors.New("current version is empty")
	}

	current, err := version.NewVersion(currentVersion)
	if err != nil {
		return false, fmt.Errorf("invalid current version %q: %w", currentVersion, err)
	}
	init, err := version.NewVersion(initVersion)
	if err != nil {
		return false, fmt.Errorf("invalid init version %q: %w", initVersion, err)
	}

	return current.LessThan(init), nil
}

// LegacyHealth checks the root endpoint for the legacy welcome marker.
func LegacyHealth(ctx context.Context, baseURL string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create legacy health request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("legacy health request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("legacy health status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		return fmt.Errorf("failed to read legacy health body: %w", err)
	}

	if !strings.Contains(string(body), legacyHealthMarker) {
		return fmt.Errorf("legacy health marker not found")
	}

	return nil
}

// VersionFromLabels extracts the version label from docker inspect.
func VersionFromLabels(ctx context.Context, dockerBin, containerName string) (string, error) {
	logger := log.New(io.Discard, "", 0)
	inspector := container.NewInspector(dockerBin, logger)
	state, err := inspector.ExtractRuntimeState(ctx, containerName)
	if err != nil {
		return "", err
	}

	if state.Labels == nil {
		return "", fmt.Errorf("no labels found on container")
	}
	versionLabel := strings.TrimSpace(state.Labels["org.opencontainers.image.version"])
	if versionLabel == "" {
		return "", fmt.Errorf("version label not found")
	}

	return versionLabel, nil
}
