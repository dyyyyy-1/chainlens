package decoder

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const testABIResponse = `{"abi":[{"type":"event","name":"Ping","anonymous":false,"inputs":[
	{"name":"who","type":"address","indexed":true}]}]}`

func TestSourcifyFetchABI(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fields"); got != "abi" {
			t.Errorf("fields = %q, want abi", got)
		}
		if r.URL.Path != "/1/0x1234567890123456789012345678901234567890" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(testABIResponse))
	}))
	defer server.Close()

	client := newTestSourcifyClient(server.URL)

	abiJSON, err := client.FetchABI(context.Background(), 1, testAddress)
	if err != nil {
		t.Fatalf("FetchABI: %v", err)
	}

	decoder, err := New(abiJSON)
	if err != nil {
		t.Fatalf("New from fetched abi: %v", err)
	}
	if len(decoder.byTopic) != 1 {
		t.Fatalf("indexed events = %d, want 1", len(decoder.byTopic))
	}
}

func TestSourcifyNotVerified(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestSourcifyClient(server.URL)

	if _, err := client.FetchABI(context.Background(), 1, testAddress); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("err = %v, want ErrNotVerified", err)
	}
}

func TestSourcifyNullABI(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"abi":null}`))
	}))
	defer server.Close()

	client := newTestSourcifyClient(server.URL)

	if _, err := client.FetchABI(context.Background(), 1, testAddress); !errors.Is(err, ErrNotVerified) {
		t.Fatalf("err = %v, want ErrNotVerified", err)
	}
}

func TestSourcifyRetriesServerError(t *testing.T) {
	t.Parallel()

	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(testABIResponse))
	}))
	defer server.Close()

	client := newTestSourcifyClient(server.URL)

	if _, err := client.FetchABI(context.Background(), 1, testAddress); err != nil {
		t.Fatalf("FetchABI after retry: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestSourcifyGivesUpAfterRetries(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	client := newTestSourcifyClient(server.URL)

	if _, err := client.FetchABI(context.Background(), 1, testAddress); err == nil {
		t.Fatal("FetchABI succeeded, want error")
	}
}

// newTestSourcifyClient 指向本地 httptest 服务，避免测试依赖外网。
func newTestSourcifyClient(baseURL string) *SourcifyClient {
	client := NewSourcifyClient(5 * time.Second)
	client.BaseURL = baseURL
	return client
}
