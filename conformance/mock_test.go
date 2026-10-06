package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const mockBaseURL = "http://127.0.0.1:4010"

// mockClient talks to the conformance mock's control API (runner contract, section 3).
type mockClient struct {
	http *http.Client
}

type mockState struct {
	PendingHolds int64    `json:"pendingHolds"`
	NextHoldAt   *float64 `json:"nextHoldAt"`
	Now          float64  `json:"now"`
}

func newMockClient() *mockClient {
	return &mockClient{http: &http.Client{Timeout: 30 * time.Second}}
}

func (m *mockClient) do(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader = http.NoBody
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, mockBaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s answered %d: %s", method, path, resp.StatusCode, data)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	return nil
}

func (m *mockClient) scenarios(ctx context.Context) ([]string, error) {
	var list struct {
		Scenarios []string `json:"scenarios"`
	}
	err := m.do(ctx, http.MethodGet, "/__scenarios", nil, &list)
	return list.Scenarios, err
}

func (m *mockClient) scenario(ctx context.Context, name string) (scenarioFile, error) {
	var file scenarioFile
	err := m.do(ctx, http.MethodGet, "/__files/scenarios/"+name+".json", nil, &file)
	return file, err
}

func (m *mockClient) load(ctx context.Context, name string) error {
	return m.do(ctx, http.MethodPost, "/__scenario", map[string]string{"name": name}, nil)
}

func (m *mockClient) mark(ctx context.Context) error {
	return m.do(ctx, http.MethodPost, "/__mark", nil, nil)
}

func (m *mockClient) state(ctx context.Context) (mockState, error) {
	var state mockState
	err := m.do(ctx, http.MethodGet, "/__state", nil, &state)
	return state, err
}

// advance moves the mock clock by exactly by and returns the clock it reports, in mock seconds.
func (m *mockClient) advance(ctx context.Context, by time.Duration) (float64, error) {
	var reply struct {
		Now float64 `json:"now"`
	}
	err := m.do(ctx, http.MethodPost, "/__clock", map[string]float64{"advance": by.Seconds()}, &reply)
	return reply.Now, err
}

// file downloads one file the mock serves (scenarios, fixtures, hints).
func (m *mockClient) file(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mockBaseURL+path, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d", path, resp.StatusCode)
	}
	return data, nil
}

type verdict struct {
	OK       bool     `json:"ok"`
	Failures []string `json:"failures"`
}

func (m *mockClient) verify(ctx context.Context) (verdict, error) {
	var v verdict
	err := m.do(ctx, http.MethodGet, "/__verify", nil, &v)
	return v, err
}
