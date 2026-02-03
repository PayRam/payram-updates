package container

import (
	"context"
	"testing"

	"github.com/payram/payram-updater/internal/manifest"
)

func TestResolver_Resolve_EnvPriority(t *testing.T) {
	resolver := NewResolver("my-container", "docker", nil)
	manifestData := &manifest.Manifest{
		Defaults: manifest.Defaults{
			ContainerName: "manifest-container",
		},
	}

	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.Name != "my-container" {
		t.Errorf("expected 'my-container', got '%s'", resolved.Name)
	}

	if resolved.Source != SourceEnv {
		t.Errorf("expected source 'env', got '%s'", resolved.Source)
	}
}

func TestResolver_Resolve_ManifestFallback(t *testing.T) {
	resolver := NewResolver("", "docker", nil)
	manifestData := &manifest.Manifest{
		Defaults: manifest.Defaults{
			ContainerName: "manifest-container",
		},
	}

	resolved, err := resolver.Resolve(manifestData)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.Name != "manifest-container" {
		t.Errorf("expected 'manifest-container', got '%s'", resolved.Name)
	}

	if resolved.Source != SourceManifest {
		t.Errorf("expected source 'manifest', got '%s'", resolved.Source)
	}
}

func TestResolver_Resolve_NoContainerName(t *testing.T) {
	resolver := NewResolver("", "docker", nil)
	manifestData := &manifest.Manifest{
		Defaults: manifest.Defaults{
			ContainerName: "",
		},
	}

	_, err := resolver.Resolve(manifestData)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	resErr, ok := err.(*ResolutionError)
	if !ok {
		t.Fatalf("expected ResolutionError, got %T", err)
	}

	if resErr.GetFailureCode() != "CONTAINER_NAME_UNRESOLVED" {
		t.Errorf("expected failure code 'CONTAINER_NAME_UNRESOLVED', got '%s'", resErr.GetFailureCode())
	}
}

func TestResolver_Resolve_NilManifest(t *testing.T) {
	resolver := NewResolver("", "docker", nil)

	_, err := resolver.Resolve(nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	resErr, ok := err.(*ResolutionError)
	if !ok {
		t.Fatalf("expected ResolutionError, got %T", err)
	}

	if resErr.GetFailureCode() != "CONTAINER_NAME_UNRESOLVED" {
		t.Errorf("expected failure code 'CONTAINER_NAME_UNRESOLVED', got '%s'", resErr.GetFailureCode())
	}
}

func TestResolver_Resolve_EnvOverridesNilManifest(t *testing.T) {
	resolver := NewResolver("env-container", "docker", nil)

	resolved, err := resolver.Resolve(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.Name != "env-container" {
		t.Errorf("expected 'env-container', got '%s'", resolved.Name)
	}
}

func TestResolutionError_Error(t *testing.T) {
	err := &ResolutionError{
		FailureCode: "TEST_CODE",
		Message:     "Test message",
	}

	if err.Error() != "Test message" {
		t.Errorf("expected 'Test message', got '%s'", err.Error())
	}

	if err.GetFailureCode() != "TEST_CODE" {
		t.Errorf("expected 'TEST_CODE', got '%s'", err.GetFailureCode())
	}
}

func TestResolvedContainer_Values(t *testing.T) {
	tests := []struct {
		name   string
		source ResolutionSource
	}{
		{"env", SourceEnv},
		{"manifest", SourceManifest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := &ResolvedContainer{
				Name:   "test-container",
				Source: tt.source,
			}

			if rc.Name != "test-container" {
				t.Errorf("expected 'test-container', got '%s'", rc.Name)
			}

			if rc.Source != tt.source {
				t.Errorf("expected source '%s', got '%s'", tt.source, rc.Source)
			}
		})
	}
}

func TestResolver_ValidateExists_ContainerNotFound(t *testing.T) {
	// This test uses a non-existent container
	resolver := NewResolver("", "docker", nil)
	ctx := context.Background()

	err := resolver.ValidateExists(ctx, "definitely-does-not-exist-container-12345")
	if err == nil {
		t.Fatal("expected error for non-existent container")
	}

	resErr, ok := err.(*ResolutionError)
	if !ok {
		t.Fatalf("expected ResolutionError, got %T", err)
	}

	if resErr.GetFailureCode() != "CONTAINER_NOT_FOUND" {
		t.Errorf("expected failure code 'CONTAINER_NOT_FOUND', got '%s'", resErr.GetFailureCode())
	}
}
