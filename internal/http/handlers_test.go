package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleHealth(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		wantStatusCode int
		wantResponse   *HealthResponse
	}{
		{
			name:           "GET request returns ok",
			method:         http.MethodGet,
			wantStatusCode: http.StatusOK,
			wantResponse:   &HealthResponse{Status: "ok"},
		},
		{
			name:           "POST request returns method not allowed",
			method:         http.MethodPost,
			wantStatusCode: http.StatusMethodNotAllowed,
			wantResponse:   nil,
		},
		{
			name:           "PUT request returns method not allowed",
			method:         http.MethodPut,
			wantStatusCode: http.StatusMethodNotAllowed,
			wantResponse:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/health", nil)
			w := httptest.NewRecorder()

			handler := HandleHealth()
			handler(w, req)

			resp := w.Result()
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatusCode {
				t.Errorf("expected status %d, got %d", tt.wantStatusCode, resp.StatusCode)
			}

			if tt.wantResponse != nil {
				var got HealthResponse
				if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
					t.Fatalf("failed to decode response: %v", err)
				}

				if got.Status != tt.wantResponse.Status {
					t.Errorf("expected status %q, got %q", tt.wantResponse.Status, got.Status)
				}

				contentType := resp.Header.Get("Content-Type")
				if contentType != "application/json" {
					t.Errorf("expected Content-Type 'application/json', got %q", contentType)
				}
			}
		})
	}
}
