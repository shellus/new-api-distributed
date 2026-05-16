package audit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
)

func TestReporterPostsAuditEventWithBearerKey(t *testing.T) {
	var gotAuth string
	var gotContentType string
	var gotEvent Event

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := common.DecodeJson(r.Body, &gotEvent); err != nil {
			t.Fatalf("failed to decode audit event: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	reporter := NewReporter(Config{
		EndpointURL: server.URL,
		APIKey:      "audit-key",
		Timeout:     time.Second,
	}, server.Client())

	err := reporter.Report(Event{
		Version:   ProtocolVersion,
		Event:     EventRequestResponse,
		RequestID: "req-1",
		User:      UserInfo{ID: 7, Username: "alice"},
		Key:       KeyInfo{ID: 9, Name: "prod-key"},
		Client:    ClientInfo{IP: "203.0.113.10"},
		Request:   Body{Content: "hello"},
		Response:  Body{Content: "world"},
	})
	if err != nil {
		t.Fatalf("expected report to succeed, got %v", err)
	}

	if gotAuth != "Bearer audit-key" {
		t.Fatalf("expected bearer auth header, got %q", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Fatalf("expected json content type, got %q", gotContentType)
	}
	if gotEvent.User.Username != "alice" || gotEvent.Key.Name != "prod-key" || gotEvent.Client.IP != "203.0.113.10" {
		t.Fatalf("unexpected dimensions in event: %+v", gotEvent)
	}
	if gotEvent.Request.Content != "hello" || gotEvent.Response.Content != "world" {
		t.Fatalf("unexpected body content in event: %+v", gotEvent)
	}
}

func TestCaptureBufferTruncatesWithoutDroppingOriginalWrite(t *testing.T) {
	buffer := NewCaptureBuffer(5)

	buffer.Write([]byte("hello"))
	buffer.Write([]byte(" world"))

	body := buffer.Body("text/plain")
	if body.Content != "hello" {
		t.Fatalf("expected captured content to be truncated, got %q", body.Content)
	}
	if !body.Truncated {
		t.Fatalf("expected body to be marked truncated")
	}
	if body.SizeBytes != 11 {
		t.Fatalf("expected original size 11, got %d", body.SizeBytes)
	}
}
