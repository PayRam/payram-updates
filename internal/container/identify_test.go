package container

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIdentifyPayramCorePort_Success tests successful port identification.
func TestIdentifyPayramCorePort_Success(t *testing.T) {
	// Create test HTTP server that responds with welcome message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "<html><body><h1>%s</h1></body></html>", PayramCoreWelcomeMessage)
		}
	}))
	defer server.Close()

	// Extract port from server URL
	port := server.URL[len("http://127.0.0.1:"):]

	// Create runtime state with the test server port
	state := &RuntimeState{
		ID:   "test123",
		Name: "payram-core",
		Ports: []PortMapping{
			{
				HostPort:      port,
				ContainerPort: "80",
				Protocol:      "tcp",
			},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	identifiedPort, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identifiedPort.HostPort != port {
		t.Errorf("Expected port %s, got %s", port, identifiedPort.HostPort)
	}

	if identifiedPort.ContainerPort != "80" {
		t.Errorf("Expected container port '80', got '%s'", identifiedPort.ContainerPort)
	}

	if identifiedPort.Protocol != "tcp" {
		t.Errorf("Expected protocol 'tcp', got '%s'", identifiedPort.Protocol)
	}
}

// TestIdentifyPayramCorePort_MultiplePorts tests identification with multiple ports.
func TestIdentifyPayramCorePort_MultiplePorts(t *testing.T) {
	// Create wrong server (doesn't respond with welcome message)
	wrongServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "This is not Payram Core")
	}))
	defer wrongServer.Close()

	// Create correct server
	correctServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer correctServer.Close()

	wrongPort := wrongServer.URL[len("http://127.0.0.1:"):]
	correctPort := correctServer.URL[len("http://127.0.0.1:"):]

	// Create runtime state with both ports
	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: wrongPort, ContainerPort: "8080", Protocol: "tcp"},
			{HostPort: correctPort, ContainerPort: "80", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	identifiedPort, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should identify the correct port
	if identifiedPort.HostPort != correctPort {
		t.Errorf("Expected port %s, got %s", correctPort, identifiedPort.HostPort)
	}
}

// TestIdentifyPayramCorePort_NoMatchingPort tests when no port has the welcome message.
func TestIdentifyPayramCorePort_NoMatchingPort(t *testing.T) {
	// Create server without welcome message
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Some other service")
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	_, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	identErr, ok := err.(*IdentificationError)
	if !ok {
		t.Fatalf("Expected IdentificationError, got %T", err)
	}

	if identErr.FailureCode != "PAYRAM_CORE_PORT_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CORE_PORT_NOT_FOUND, got '%s'", identErr.FailureCode)
	}
}

// TestIdentifyPayramCorePort_NoPorts tests when container has no exposed ports.
func TestIdentifyPayramCorePort_NoPorts(t *testing.T) {
	state := &RuntimeState{
		Ports: []PortMapping{},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	_, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	identErr, ok := err.(*IdentificationError)
	if !ok {
		t.Fatalf("Expected IdentificationError, got %T", err)
	}

	if identErr.FailureCode != "PAYRAM_CORE_PORT_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CORE_PORT_NOT_FOUND, got '%s'", identErr.FailureCode)
	}
}

// TestIdentifyPayramCorePort_NilState tests with nil runtime state.
func TestIdentifyPayramCorePort_NilState(t *testing.T) {
	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	_, err := identifier.IdentifyPayramCorePort(context.Background(), nil)
	if err == nil {
		t.Fatal("Expected error for nil state, got nil")
	}

	if err.Error() != "runtime state is nil" {
		t.Errorf("Expected 'runtime state is nil', got '%s'", err.Error())
	}
}

// TestIdentifyPayramCorePort_ContextCancellation tests context cancellation.
func TestIdentifyPayramCorePort_ContextCancellation(t *testing.T) {
	// Create server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	// Create context that cancels immediately
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()

	_, err := identifier.IdentifyPayramCorePort(ctx, state)
	if err == nil {
		t.Fatal("Expected error due to context cancellation, got nil")
	}

	// Should fail because context expired before server responded
	identErr, ok := err.(*IdentificationError)
	if !ok {
		t.Fatalf("Expected IdentificationError, got %T: %v", err, err)
	}

	if identErr.FailureCode != "PAYRAM_CORE_PORT_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CORE_PORT_NOT_FOUND, got '%s'", identErr.FailureCode)
	}
}

// TestIdentifyPayramCorePort_NonTCPPort tests skipping non-TCP ports.
func TestIdentifyPayramCorePort_NonTCPPort(t *testing.T) {
	// Create server for TCP port
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: "5353", ContainerPort: "53", Protocol: "udp"}, // UDP port - should skip
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"},   // TCP port - should check
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	identifiedPort, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identifiedPort.HostPort != port {
		t.Errorf("Expected port %s, got %s", port, identifiedPort.HostPort)
	}
}

// TestIdentifyPayramCorePort_HTTPError tests when port returns HTTP error.
func TestIdentifyPayramCorePort_HTTPError(t *testing.T) {
	// Create server that returns 500 error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "Internal Server Error")
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	_, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err == nil {
		t.Fatal("Expected error, got nil")
	}

	// Should fail because response doesn't contain welcome message
	identErr, ok := err.(*IdentificationError)
	if !ok {
		t.Fatalf("Expected IdentificationError, got %T", err)
	}

	if identErr.FailureCode != "PAYRAM_CORE_PORT_NOT_FOUND" {
		t.Errorf("Expected PAYRAM_CORE_PORT_NOT_FOUND, got '%s'", identErr.FailureCode)
	}
}

// TestIdentifyPayramCorePort_PartialMatch tests when message is part of larger response.
func TestIdentifyPayramCorePort_PartialMatch(t *testing.T) {
	// Create server with welcome message embedded in HTML
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head><title>Payram</title></head>
<body>
  <div class="header">
    <h1>%s</h1>
    <p>Version 1.0.0</p>
  </div>
</body>
</html>
`, PayramCoreWelcomeMessage)
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	identifiedPort, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identifiedPort.HostPort != port {
		t.Errorf("Expected port %s, got %s", port, identifiedPort.HostPort)
	}
}

// TestIdentifyPayramCorePort_EmptyHostPort tests skipping empty host port.
func TestIdentifyPayramCorePort_EmptyHostPort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: "", ContainerPort: "80", Protocol: "tcp"},   // Empty - should skip
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"}, // Valid - should check
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	identifiedPort, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	if identifiedPort.HostPort != port {
		t.Errorf("Expected port %s, got %s", port, identifiedPort.HostPort)
	}
}

// TestCheckPort tests the internal checkPort method behavior.
func TestCheckPort(t *testing.T) {
	tests := []struct {
		name         string
		responseBody string
		responseCode int
		shouldMatch  bool
	}{
		{
			name:         "exact match",
			responseBody: PayramCoreWelcomeMessage,
			responseCode: http.StatusOK,
			shouldMatch:  true,
		},
		{
			name:         "case sensitive - should not match",
			responseBody: "welcome to payram core",
			responseCode: http.StatusOK,
			shouldMatch:  false,
		},
		{
			name:         "substring match",
			responseBody: fmt.Sprintf("Hello! %s is running.", PayramCoreWelcomeMessage),
			responseCode: http.StatusOK,
			shouldMatch:  true,
		},
		{
			name:         "no match",
			responseBody: "Different service",
			responseCode: http.StatusOK,
			shouldMatch:  false,
		},
		{
			name:         "404 with message",
			responseBody: PayramCoreWelcomeMessage,
			responseCode: http.StatusNotFound,
			shouldMatch:  true, // Still matches if message is present
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.responseCode)
				fmt.Fprint(w, tt.responseBody)
			}))
			defer server.Close()

			port := server.URL[len("http://127.0.0.1:"):]

			logger := &mockLogger{}
			identifier := NewPortIdentifier(logger)

			result := identifier.checkPort(context.Background(), port)
			if result != tt.shouldMatch {
				t.Errorf("Expected match=%v, got match=%v", tt.shouldMatch, result)
			}
		})
	}
}

// TestNewPortIdentifier validates constructor.
func TestNewPortIdentifier(t *testing.T) {
	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	if identifier == nil {
		t.Fatal("NewPortIdentifier returned nil")
	}

	if identifier.logger != logger {
		t.Error("Logger not set correctly")
	}

	if identifier.httpClient == nil {
		t.Fatal("HTTP client not initialized")
	}

	if identifier.httpClient.Timeout != PortIdentificationTimeout {
		t.Errorf("Expected timeout %v, got %v", PortIdentificationTimeout, identifier.httpClient.Timeout)
	}
}

// TestIdentificationError tests error formatting.
func TestIdentificationError(t *testing.T) {
	err := &IdentificationError{
		FailureCode: "TEST_ERROR",
		Message:     "This is a test error",
	}

	expected := "TEST_ERROR: This is a test error"
	if err.Error() != expected {
		t.Errorf("Expected '%s', got '%s'", expected, err.Error())
	}
}

// TestIdentifiedPortStructure validates the structure.
func TestIdentifiedPortStructure(t *testing.T) {
	port := IdentifiedPort{
		HostPort:      "8080",
		ContainerPort: "80",
		Protocol:      "tcp",
	}

	if port.HostPort != "8080" {
		t.Errorf("Expected HostPort '8080', got '%s'", port.HostPort)
	}

	if port.ContainerPort != "80" {
		t.Errorf("Expected ContainerPort '80', got '%s'", port.ContainerPort)
	}

	if port.Protocol != "tcp" {
		t.Errorf("Expected Protocol 'tcp', got '%s'", port.Protocol)
	}
}

// TestPayramCoreWelcomeMessage validates the constant.
func TestPayramCoreWelcomeMessage(t *testing.T) {
	expected := "Welcome to Payram Core"
	if PayramCoreWelcomeMessage != expected {
		t.Errorf("Expected '%s', got '%s'", expected, PayramCoreWelcomeMessage)
	}
}

// TestIdentifyPayramCorePort_FirstMatchWins tests that first matching port is returned.
func TestIdentifyPayramCorePort_FirstMatchWins(t *testing.T) {
	// Create two servers both with welcome message
	server1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer server1.Close()

	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer server2.Close()

	port1 := server1.URL[len("http://127.0.0.1:"):]
	port2 := server2.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: port1, ContainerPort: "80", Protocol: "tcp"},
			{HostPort: port2, ContainerPort: "8080", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	identifiedPort, err := identifier.IdentifyPayramCorePort(context.Background(), state)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}

	// Should return the first matching port
	if identifiedPort.HostPort != port1 {
		t.Errorf("Expected first port %s, got %s", port1, identifiedPort.HostPort)
	}
}

// Benchmark port identification
func BenchmarkIdentifyPayramCorePort(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, PayramCoreWelcomeMessage)
	}))
	defer server.Close()

	port := server.URL[len("http://127.0.0.1:"):]

	state := &RuntimeState{
		Ports: []PortMapping{
			{HostPort: port, ContainerPort: "80", Protocol: "tcp"},
		},
	}

	logger := &mockLogger{}
	identifier := NewPortIdentifier(logger)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = identifier.IdentifyPayramCorePort(context.Background(), state)
	}
}
