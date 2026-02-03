package container

import (
	"testing"

	"github.com/payram/payram-updater/internal/manifest"
)

// TestReconcilePorts_KeepsAllInspectedPorts tests that inspected ports are never removed.
func TestReconcilePorts_KeepsAllInspectedPorts(t *testing.T) {
	inspected := []PortMapping{
		{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
		{HostPort: "8443", ContainerPort: "443", Protocol: "tcp"},
	}

	manifestPorts := []manifest.Port{} // No manifest ports

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcilePorts(inspected, manifestPorts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("Expected 2 ports (all inspected kept), got %d", len(result))
	}
}

// TestReconcilePorts_AddsManifestRequiredPorts tests adding missing manifest ports.
func TestReconcilePorts_AddsManifestRequiredPorts(t *testing.T) {
	inspected := []PortMapping{
		{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
	}

	manifestPorts := []manifest.Port{
		{Container: 443, Host: 8443, Protocol: "tcp"}, // New port
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcilePorts(inspected, manifestPorts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Expected 2 ports (1 inspected + 1 added), got %d", len(result))
	}

	// Check that original port is preserved
	if result[0].HostPort != "8080" {
		t.Errorf("Expected original port 8080, got %s", result[0].HostPort)
	}

	// Check that new port was added
	found := false
	for _, port := range result {
		if port.ContainerPort == "443" && port.HostPort == "8443" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected manifest-required port 443:8443 to be added")
	}
}

// TestReconcilePorts_KeepsExistingWhenManifestRequiresSame tests that existing port mapping is kept.
func TestReconcilePorts_KeepsExistingWhenManifestRequiresSame(t *testing.T) {
	inspected := []PortMapping{
		{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
	}

	manifestPorts := []manifest.Port{
		{Container: 80, Host: 9999, Protocol: "tcp"}, // Same container port, different host port
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcilePorts(inspected, manifestPorts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should only have 1 port (original), manifest requirement ignored
	if len(result) != 1 {
		t.Errorf("Expected 1 port (existing kept, no remap), got %d", len(result))
	}

	// Original mapping should be unchanged
	if result[0].HostPort != "8080" {
		t.Errorf("Expected original host port 8080, got %s (should not remap)", result[0].HostPort)
	}
}

// TestReconcilePorts_FailsOnHostPortConflict tests port conflict detection.
func TestReconcilePorts_FailsOnHostPortConflict(t *testing.T) {
	inspected := []PortMapping{
		{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
	}

	manifestPorts := []manifest.Port{
		{Container: 443, Host: 8080, Protocol: "tcp"}, // Conflicts with existing host port
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	_, err := reconciler.ReconcilePorts(inspected, manifestPorts)
	if err == nil {
		t.Fatal("Expected error for port conflict, got nil")
	}

	reconErr, ok := err.(*ReconciliationError)
	if !ok {
		t.Fatalf("Expected ReconciliationError, got %T", err)
	}

	if reconErr.FailureCode != "PORT_CONFLICT" {
		t.Errorf("Expected PORT_CONFLICT, got %s", reconErr.FailureCode)
	}
}

// TestReconcilePorts_DefaultsToTCP tests default protocol.
func TestReconcilePorts_DefaultsToTCP(t *testing.T) {
	inspected := []PortMapping{}

	manifestPorts := []manifest.Port{
		{Container: 80, Host: 8080}, // No protocol specified
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcilePorts(inspected, manifestPorts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("Expected 1 port, got %d", len(result))
	}

	if result[0].Protocol != "tcp" {
		t.Errorf("Expected default protocol 'tcp', got '%s'", result[0].Protocol)
	}
}

// TestReconcilePorts_UsesContainerPortAsHostPortIfNotSpecified tests default host port.
func TestReconcilePorts_UsesContainerPortAsHostPortIfNotSpecified(t *testing.T) {
	inspected := []PortMapping{}

	manifestPorts := []manifest.Port{
		{Container: 80}, // No host port specified
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcilePorts(inspected, manifestPorts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("Expected 1 port, got %d", len(result))
	}

	if result[0].HostPort != "80" {
		t.Errorf("Expected host port to default to '80', got '%s'", result[0].HostPort)
	}
}

// TestReconcileMounts_KeepsAllInspectedMounts tests that inspected mounts are never removed.
func TestReconcileMounts_KeepsAllInspectedMounts(t *testing.T) {
	inspected := []Mount{
		{Type: "volume", Source: "data-vol", Destination: "/data", RW: true},
		{Type: "bind", Source: "/host/config", Destination: "/config", RW: false},
	}

	manifestMounts := []manifest.Volume{} // No manifest mounts

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileMounts(inspected, manifestMounts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("Expected 2 mounts (all inspected kept), got %d", len(result))
	}
}

// TestReconcileMounts_AddsManifestRequiredMounts tests adding missing manifest mounts.
func TestReconcileMounts_AddsManifestRequiredMounts(t *testing.T) {
	inspected := []Mount{
		{Type: "volume", Source: "data-vol", Destination: "/data", RW: true},
	}

	manifestMounts := []manifest.Volume{
		{Destination: "/logs", Source: "", ReadOnly: false}, // New volume mount
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileMounts(inspected, manifestMounts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Expected 2 mounts (1 inspected + 1 added), got %d", len(result))
	}

	// Check that new mount was added
	found := false
	for _, mount := range result {
		if mount.Destination == "/logs" {
			found = true
			if mount.Type != "volume" {
				t.Errorf("Expected type 'volume', got '%s'", mount.Type)
			}
			if !mount.RW {
				t.Error("Expected RW to be true")
			}
			break
		}
	}
	if !found {
		t.Error("Expected manifest-required mount /logs to be added")
	}
}

// TestReconcileMounts_KeepsExistingWhenManifestRequiresSamePath tests existing mount is kept.
func TestReconcileMounts_KeepsExistingWhenManifestRequiresSamePath(t *testing.T) {
	inspected := []Mount{
		{Type: "volume", Source: "data-vol", Destination: "/data", RW: true},
	}

	manifestMounts := []manifest.Volume{
		{Destination: "/data", Source: "/other/path", ReadOnly: true}, // Same path, different config
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileMounts(inspected, manifestMounts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should only have 1 mount (original), manifest requirement ignored
	if len(result) != 1 {
		t.Errorf("Expected 1 mount (existing kept, no modification), got %d", len(result))
	}

	// Original mount should be unchanged
	if result[0].Type != "volume" || result[0].Source != "data-vol" || !result[0].RW {
		t.Error("Original mount was modified (should never change)")
	}
}

// TestReconcileMounts_BindMountVsVolume tests bind mount detection.
func TestReconcileMounts_BindMountVsVolume(t *testing.T) {
	inspected := []Mount{}

	manifestMounts := []manifest.Volume{
		{Destination: "/data", Source: "", ReadOnly: false},           // Volume (no host path)
		{Destination: "/config", Source: "/host/cfg", ReadOnly: true}, // Bind mount (has host path)
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileMounts(inspected, manifestMounts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Expected 2 mounts, got %d", len(result))
	}

	// Check volume
	volumeFound := false
	for _, mount := range result {
		if mount.Destination == "/data" {
			volumeFound = true
			if mount.Type != "volume" {
				t.Errorf("Expected type 'volume', got '%s'", mount.Type)
			}
			break
		}
	}
	if !volumeFound {
		t.Error("Expected volume mount /data")
	}

	// Check bind mount
	bindFound := false
	for _, mount := range result {
		if mount.Destination == "/config" {
			bindFound = true
			if mount.Type != "bind" {
				t.Errorf("Expected type 'bind', got '%s'", mount.Type)
			}
			if mount.Source != "/host/cfg" {
				t.Errorf("Expected source '/host/cfg', got '%s'", mount.Source)
			}
			break
		}
	}
	if !bindFound {
		t.Error("Expected bind mount /config")
	}
}

// TestReconcileMounts_ReadOnlyMode tests read-only mount mode.
func TestReconcileMounts_ReadOnlyMode(t *testing.T) {
	inspected := []Mount{}

	manifestMounts := []manifest.Volume{
		{Destination: "/readonly", ReadOnly: true},
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileMounts(inspected, manifestMounts)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("Expected 1 mount, got %d", len(result))
	}

	if result[0].RW {
		t.Error("Expected RW to be false for read-only mount")
	}

	if result[0].Mode != "ro" {
		t.Errorf("Expected mode 'ro', got '%s'", result[0].Mode)
	}
}

// TestReconcileEnv_KeepsAllInspectedEnvVars tests that inspected env vars are never removed.
func TestReconcileEnv_KeepsAllInspectedEnvVars(t *testing.T) {
	inspected := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"POSTGRES_HOST=db.example.com",
	}

	manifestEnv := map[string]string{} // No manifest env vars

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileEnv(inspected, manifestEnv)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 3 {
		t.Errorf("Expected 3 env vars (all inspected kept), got %d", len(result))
	}
}

// TestReconcileEnv_AddsManifestRequiredEnvVars tests adding missing manifest env vars.
func TestReconcileEnv_AddsManifestRequiredEnvVars(t *testing.T) {
	inspected := []string{
		"PATH=/usr/bin",
	}

	manifestEnv := map[string]string{
		"NEW_VAR": "new_value",
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileEnv(inspected, manifestEnv)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("Expected 2 env vars (1 inspected + 1 added), got %d", len(result))
	}

	// Check that new var was added
	found := false
	for _, env := range result {
		if env == "NEW_VAR=new_value" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Expected manifest-required env var NEW_VAR to be added")
	}
}

// TestReconcileEnv_NeverOverwritesExistingVars tests that existing values are preserved.
func TestReconcileEnv_NeverOverwritesExistingVars(t *testing.T) {
	inspected := []string{
		"DATABASE_URL=postgres://existing:secret@db:5432/prod",
		"AES_KEY=existing_secret_key_12345",
	}

	manifestEnv := map[string]string{
		"DATABASE_URL": "postgres://manifest:different@localhost/dev", // Try to override
		"AES_KEY":      "manifest_key_67890",                          // Try to override secret
		"NEW_VAR":      "new_value",                                   // New var - should be added
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileEnv(inspected, manifestEnv)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should have 3 vars: 2 original (unchanged) + 1 new
	if len(result) != 3 {
		t.Errorf("Expected 3 env vars, got %d", len(result))
	}

	// Check that original values were NOT overwritten
	dbUrlFound := false
	aesKeyFound := false
	newVarFound := false

	for _, env := range result {
		if env == "DATABASE_URL=postgres://existing:secret@db:5432/prod" {
			dbUrlFound = true
		}
		if env == "AES_KEY=existing_secret_key_12345" {
			aesKeyFound = true
		}
		if env == "NEW_VAR=new_value" {
			newVarFound = true
		}
		// Check that manifest values were NOT used
		if env == "DATABASE_URL=postgres://manifest:different@localhost/dev" {
			t.Error("DATABASE_URL was overwritten (should preserve existing value)")
		}
		if env == "AES_KEY=manifest_key_67890" {
			t.Error("AES_KEY was overwritten (should NEVER change secrets)")
		}
	}

	if !dbUrlFound {
		t.Error("Original DATABASE_URL not preserved")
	}
	if !aesKeyFound {
		t.Error("Original AES_KEY not preserved (secrets must never be modified)")
	}
	if !newVarFound {
		t.Error("New manifest env var not added")
	}
}

// TestReconcileEnv_EmptyValueIsValid tests that empty values are valid.
func TestReconcileEnv_EmptyValueIsValid(t *testing.T) {
	inspected := []string{
		"EMPTY_VAR=",
	}

	manifestEnv := map[string]string{
		"EMPTY_VAR": "try_to_set", // Try to override empty value
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.ReconcileEnv(inspected, manifestEnv)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should preserve the empty value
	found := false
	for _, env := range result {
		if env == "EMPTY_VAR=" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Empty value should be preserved")
	}
}

// TestReconcile_FullIntegration tests the full reconciliation flow.
func TestReconcile_FullIntegration(t *testing.T) {
	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: "8080", ContainerPort: "80", Protocol: "tcp"},
		},
		Mounts: []Mount{
			{Type: "volume", Source: "data", Destination: "/data", RW: true},
		},
		Env: []string{
			"EXISTING_VAR=existing_value",
		},
	}

	manifest := &manifest.Manifest{
		Defaults: manifest.Defaults{
			Ports: []manifest.Port{
				{Container: 443, Host: 8443}, // Add new port
			},
			Volumes: []manifest.Volume{
				{Destination: "/logs"}, // Add new mount
			},
			// Note: Environment not yet supported in manifest
		},
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.Reconcile(state, manifest)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Check counts
	if len(result.Ports) != 2 {
		t.Errorf("Expected 2 ports, got %d", len(result.Ports))
	}
	if len(result.Mounts) != 2 {
		t.Errorf("Expected 2 mounts, got %d", len(result.Mounts))
	}
	// Env should remain unchanged (1 existing, no manifest env vars yet)
	if len(result.Env) != 1 {
		t.Errorf("Expected 1 env var, got %d", len(result.Env))
	}

	// Check added counts
	if result.AddedPorts != 1 {
		t.Errorf("Expected 1 added port, got %d", result.AddedPorts)
	}
	if result.AddedMounts != 1 {
		t.Errorf("Expected 1 added mount, got %d", result.AddedMounts)
	}
	if result.AddedEnvs != 0 {
		t.Errorf("Expected 0 added env vars, got %d", result.AddedEnvs)
	}
}

// TestReconcile_NilManifest tests handling nil manifest.
func TestReconcile_NilManifest(t *testing.T) {
	state := &RuntimeState{
		Ports:  []PortMapping{{HostPort: "8080", ContainerPort: "80"}},
		Mounts: []Mount{{Destination: "/data"}},
		Env:    []string{"VAR=value"},
	}

	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	result, err := reconciler.Reconcile(state, nil)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should return state as-is
	if len(result.Ports) != 1 || len(result.Mounts) != 1 || len(result.Env) != 1 {
		t.Error("Expected state to be returned unchanged")
	}
}

// TestReconcile_NilState tests handling nil state.
func TestReconcile_NilState(t *testing.T) {
	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	_, err := reconciler.Reconcile(nil, &manifest.Manifest{})
	if err == nil {
		t.Fatal("Expected error for nil state, got nil")
	}
}

// TestNewReconciler tests constructor.
func TestNewReconciler(t *testing.T) {
	logger := &mockLogger{}
	reconciler := NewReconciler(logger)

	if reconciler == nil {
		t.Fatal("NewReconciler returned nil")
	}

	if reconciler.logger != logger {
		t.Error("Logger not set correctly")
	}
}

// TestReconciliationError tests error formatting.
func TestReconciliationError(t *testing.T) {
	err := &ReconciliationError{
		FailureCode: "TEST_ERROR",
		Message:     "This is a test error",
	}

	expected := "TEST_ERROR: This is a test error"
	if err.Error() != expected {
		t.Errorf("Expected '%s', got '%s'", expected, err.Error())
	}
}

// TestReconciledConfigurationStructure validates structure.
func TestReconciledConfigurationStructure(t *testing.T) {
	config := ReconciledConfiguration{
		Ports:       []PortMapping{{HostPort: "8080"}},
		Mounts:      []Mount{{Destination: "/data"}},
		Env:         []string{"VAR=value"},
		AddedPorts:  1,
		AddedMounts: 1,
		AddedEnvs:   1,
	}

	if len(config.Ports) != 1 || len(config.Mounts) != 1 || len(config.Env) != 1 {
		t.Error("ReconciledConfiguration fields should be accessible")
	}
}
