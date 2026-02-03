// Package container provides manifest reconciliation for additive-only configuration overlay.
package container

import (
	"fmt"

	"github.com/payram/payram-updater/internal/manifest"
)

/*
MANIFEST AS ADDITIVE OVERLAY

The manifest serves as an ADDITIVE overlay only. It specifies minimum required
configuration but NEVER removes, remaps, or overrides existing container state.

MANIFEST MAY DEFINE:
  - Required ports (added if missing)
  - Required mounts (added if missing)
  - Required environment variables (added if missing)
  - Migration metadata
  - Manual-upgrade breakpoints

MANIFEST MUST NEVER:
  - Remove existing ports
  - Remap existing host ports
  - Override existing environment values
  - Change existing mounts
  - Modify network configuration
  - Alter restart policy

RECONCILIATION PRINCIPLE:
  Inspected Runtime State + Manifest Requirements = Final Configuration

  Where "+" means:
  - Keep all existing configuration from runtime inspection
  - Add manifest requirements that are missing
  - Never remove or change existing values

SPECIAL CASES:
  - Secrets (AES_KEY, DB credentials): Never generated or modified
  - Port conflicts: Fail if manifest requires a port whose host port is unavailable
  - Mount paths: Fail if manifest requires a path that conflicts with existing mount
*/

// Reconciler handles additive reconciliation of manifest requirements with runtime state.
type Reconciler struct {
	logger Logger
}

// NewReconciler creates a new manifest reconciler.
func NewReconciler(logger Logger) *Reconciler {
	return &Reconciler{
		logger: logger,
	}
}

// ReconciledConfiguration represents the final configuration after reconciliation.
type ReconciledConfiguration struct {
	// Ports is the union of inspected ports + manifest-required ports
	Ports []PortMapping

	// Mounts is the union of inspected mounts + manifest-required mounts
	Mounts []Mount

	// Env is the union of inspected env + manifest-required env (no overwrites)
	Env []string

	// Metadata about what was added
	AddedPorts  int
	AddedMounts int
	AddedEnvs   int
}

// ReconcilePorts implements D2 - Port reconciliation logic.
//
// ADDITIVE ONLY: Starts with all inspected ports, adds manifest-required ports if missing.
//
// Process:
//  1. Start with all ports from runtime inspection
//  2. For each manifest-required port:
//     - If container port already exposed → keep existing mapping
//     - If missing → add using manifest specification
//     - If host port conflict → return error
//  3. Never remove or remap existing ports
//
// Returns error if manifest requires a host port that conflicts with existing mapping.
func (r *Reconciler) ReconcilePorts(inspected []PortMapping, manifestPorts []manifest.Port) ([]PortMapping, error) {
	if len(manifestPorts) == 0 {
		r.logger.Printf("No manifest ports to reconcile, keeping %d inspected ports", len(inspected))
		return inspected, nil
	}

	r.logger.Printf("Reconciling ports: %d inspected, %d manifest-required", len(inspected), len(manifestPorts))

	// Start with all inspected ports (never remove)
	result := make([]PortMapping, len(inspected))
	copy(result, inspected)

	// Track which container ports are already exposed
	exposedContainerPorts := make(map[string]bool)
	usedHostPorts := make(map[string]bool)

	for _, port := range inspected {
		key := fmt.Sprintf("%s/%s", port.ContainerPort, port.Protocol)
		exposedContainerPorts[key] = true
		if port.HostPort != "" {
			usedHostPorts[port.HostPort] = true
		}
	}

	// Add missing manifest-required ports
	added := 0
	for _, manifestPort := range manifestPorts {
		protocol := manifestPort.Protocol
		if protocol == "" {
			protocol = "tcp"
		}

		containerPort := fmt.Sprintf("%d", manifestPort.Container)
		containerPortKey := fmt.Sprintf("%s/%s", containerPort, protocol)

		// If container port already exposed, keep existing mapping
		if exposedContainerPorts[containerPortKey] {
			r.logger.Printf("Port %s already exposed, keeping existing mapping", containerPortKey)
			continue
		}

		// Check if host port is available
		var hostPort string
		if manifestPort.Host > 0 {
			hostPort = fmt.Sprintf("%d", manifestPort.Host)
		} else {
			hostPort = containerPort // Use same port if not specified
		}

		if usedHostPorts[hostPort] {
			return nil, &ReconciliationError{
				FailureCode: "PORT_CONFLICT",
				Message:     fmt.Sprintf("Manifest requires host port %s but it's already used", hostPort),
			}
		}

		// Add the missing port
		newPort := PortMapping{
			HostIP:        "0.0.0.0", // Default bind to all interfaces
			HostPort:      hostPort,
			ContainerPort: containerPort,
			Protocol:      protocol,
		}

		result = append(result, newPort)
		usedHostPorts[hostPort] = true
		added++
		r.logger.Printf("Added manifest-required port: %s:%s->%s/%s",
			newPort.HostIP, newPort.HostPort, newPort.ContainerPort, newPort.Protocol)
	}

	r.logger.Printf("Port reconciliation complete: %d total ports (%d added)", len(result), added)
	return result, nil
}

// ReconcileMounts implements D3 - Mount reconciliation logic.
//
// ADDITIVE ONLY: Starts with all inspected mounts, adds manifest-required mounts if missing.
//
// Process:
//  1. Start with all mounts from runtime inspection
//  2. For each manifest-required mount:
//     - If container path exists → keep existing mount (no modification)
//     - If missing → add mount
//  3. Never modify or delete existing mounts
//
// Returns error if manifest requires a container path that conflicts with existing mount.
func (r *Reconciler) ReconcileMounts(inspected []Mount, manifestMounts []manifest.Volume) ([]Mount, error) {
	if len(manifestMounts) == 0 {
		r.logger.Printf("No manifest mounts to reconcile, keeping %d inspected mounts", len(inspected))
		return inspected, nil
	}

	r.logger.Printf("Reconciling mounts: %d inspected, %d manifest-required", len(inspected), len(manifestMounts))

	// Start with all inspected mounts (never remove)
	result := make([]Mount, len(inspected))
	copy(result, inspected)

	// Track which container paths are already mounted
	mountedPaths := make(map[string]bool)
	for _, mount := range inspected {
		mountedPaths[mount.Destination] = true
	}

	// Add missing manifest-required mounts
	added := 0
	for _, manifestMount := range manifestMounts {
		// If container path already mounted, keep existing mount
		if mountedPaths[manifestMount.Destination] {
			r.logger.Printf("Container path %s already mounted, keeping existing mount", manifestMount.Destination)
			continue
		}

		// Determine mount type
		mountType := "volume"
		if manifestMount.Source != "" {
			mountType = "bind"
		}

		// Determine read-write mode
		isRW := true
		mode := "rw"
		if manifestMount.ReadOnly {
			isRW = false
			mode = "ro"
		}

		// Add the missing mount
		newMount := Mount{
			Type:        mountType,
			Source:      manifestMount.Source, // Empty for volumes
			Destination: manifestMount.Destination,
			Mode:        mode,
			RW:          isRW,
		}

		result = append(result, newMount)
		added++
		r.logger.Printf("Added manifest-required mount: %s -> %s (%s, %s)",
			newMount.Source, newMount.Destination, newMount.Type, newMount.Mode)
	}

	r.logger.Printf("Mount reconciliation complete: %d total mounts (%d added)", len(result), added)
	return result, nil
}

// ReconcileEnv implements D4 - Environment variable reconciliation logic.
//
// ADDITIVE ONLY: Preserves all inspected env vars, adds manifest-required vars only if missing.
//
// Process:
//  1. Start with ALL environment variables from runtime inspection
//  2. For each manifest-required env var:
//     - If variable exists → keep existing value (NEVER overwrite)
//     - If missing → add with manifest value
//  3. Never overwrite or remove existing values
//
// SPECIAL CASE - SECRETS:
//   - Never generate or modify: AES_KEY, POSTGRES_PASSWORD, etc.
//   - If secrets exist in container, they are preserved
//   - If secrets are missing and manifest doesn't provide them, that's acceptable
//     (the application will handle missing secrets at runtime)
func (r *Reconciler) ReconcileEnv(inspected []string, manifestEnv map[string]string) ([]string, error) {
	if len(manifestEnv) == 0 {
		r.logger.Printf("No manifest env vars to reconcile, keeping %d inspected env vars", len(inspected))
		return inspected, nil
	}

	r.logger.Printf("Reconciling env vars: %d inspected, %d manifest-required", len(inspected), len(manifestEnv))

	// Start with all inspected env vars (never remove)
	result := make([]string, len(inspected))
	copy(result, inspected)

	// Parse existing env vars into a map
	existingEnv := make(map[string]string)
	for _, env := range inspected {
		// Parse "KEY=VALUE" format
		for i := 0; i < len(env); i++ {
			if env[i] == '=' {
				key := env[:i]
				value := env[i+1:]
				existingEnv[key] = value
				break
			}
		}
	}

	// Add missing manifest-required env vars (never overwrite)
	added := 0
	for key, value := range manifestEnv {
		if _, exists := existingEnv[key]; exists {
			r.logger.Printf("Env var %s already exists, preserving existing value (not overwriting)", key)
			continue
		}

		// Add the missing env var
		newEnv := fmt.Sprintf("%s=%s", key, value)
		result = append(result, newEnv)
		added++
		r.logger.Printf("Added manifest-required env var: %s", key)
	}

	r.logger.Printf("Env reconciliation complete: %d total env vars (%d added)", len(result), added)
	return result, nil
}

// Reconcile performs full configuration reconciliation.
//
// This is a convenience method that calls all reconciliation functions and returns
// a complete configuration ready for docker run.
func (r *Reconciler) Reconcile(state *RuntimeState, manifest *manifest.Manifest) (*ReconciledConfiguration, error) {
	r.logger.Printf("Starting full configuration reconciliation")

	if state == nil {
		return nil, fmt.Errorf("runtime state is nil")
	}

	if manifest == nil {
		r.logger.Printf("No manifest provided, using inspected configuration as-is")
		return &ReconciledConfiguration{
			Ports:  state.Ports,
			Mounts: state.Mounts,
			Env:    state.Env,
		}, nil
	}

	// Reconcile ports
	ports, err := r.ReconcilePorts(state.Ports, manifest.Defaults.Ports)
	if err != nil {
		return nil, fmt.Errorf("port reconciliation failed: %w", err)
	}

	// Reconcile mounts
	mounts, err := r.ReconcileMounts(state.Mounts, manifest.Defaults.Volumes)
	if err != nil {
		return nil, fmt.Errorf("mount reconciliation failed: %w", err)
	}

	// Reconcile environment variables (manifest doesn't have env yet, use empty map)
	env, err := r.ReconcileEnv(state.Env, nil)
	if err != nil {
		return nil, fmt.Errorf("env reconciliation failed: %w", err)
	}

	config := &ReconciledConfiguration{
		Ports:       ports,
		Mounts:      mounts,
		Env:         env,
		AddedPorts:  len(ports) - len(state.Ports),
		AddedMounts: len(mounts) - len(state.Mounts),
		AddedEnvs:   len(env) - len(state.Env),
	}

	r.logger.Printf("Reconciliation complete: added %d ports, %d mounts, %d env vars",
		config.AddedPorts, config.AddedMounts, config.AddedEnvs)

	return config, nil
}

// ReconciliationError represents an error during configuration reconciliation.
type ReconciliationError struct {
	FailureCode string
	Message     string
}

func (e *ReconciliationError) Error() string {
	return fmt.Sprintf("%s: %s", e.FailureCode, e.Message)
}
