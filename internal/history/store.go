package history

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Event represents a history entry.
type Event struct {
	ID        string            `json:"id"`
	Timestamp string            `json:"timestamp"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Message   string            `json:"message,omitempty"`
	Data      map[string]string `json:"data,omitempty"`
}

// Store persists history events to a JSONL file.
type Store struct {
	path string
}

// NewStore creates a history store for the given state directory.
func NewStore(stateDir string) *Store {
	return &Store{path: filepath.Join(stateDir, "history.jsonl")}
}

// Append adds a history event.
func (s *Store) Append(event Event) error {
	if s == nil {
		return nil
	}

	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	if event.ID == "" {
		event.ID = fmt.Sprintf("evt-%d", time.Now().UnixNano())
	}

	if err := os.MkdirAll(filepath.Dir(s.path), 0755); err != nil {
		return fmt.Errorf("failed to create history directory: %w", err)
	}

	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open history file: %w", err)
	}
	defer f.Close()

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("failed to marshal history event: %w", err)
	}

	if _, err := f.WriteString(string(data) + "\n"); err != nil {
		return fmt.Errorf("failed to write history event: %w", err)
	}

	return nil
}

// List returns history events filtered by type and status, newest first.
func (s *Store) List(limit int, typeFilter, statusFilter string) ([]Event, error) {
	if s == nil {
		return []Event{}, nil
	}

	if limit <= 0 {
		limit = 100
	}

	file, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, fmt.Errorf("failed to read history file: %w", err)
	}
	defer file.Close()

	var events []Event
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var evt Event
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue
		}

		if typeFilter != "" && !strings.EqualFold(evt.Type, typeFilter) {
			continue
		}

		if statusFilter != "" && !strings.EqualFold(evt.Status, statusFilter) {
			continue
		}

		events = append(events, evt)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to scan history file: %w", err)
	}

	if len(events) > limit {
		events = events[len(events)-limit:]
	}

	// Reverse to newest first
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}

	return events, nil
}
