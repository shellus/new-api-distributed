package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	auditservice "github.com/QuantumNous/new-api/service/audit"
	"github.com/gin-gonic/gin"
)

func TestAuditMiddlewareReportsRequestAndResponseByUserTokenAndIP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	events := make(chan auditservice.Event, 1)
	auditServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var event auditservice.Event
		if err := common.DecodeJson(r.Body, &event); err != nil {
			t.Fatalf("failed to decode audit event: %v", err)
		}
		events <- event
		w.WriteHeader(http.StatusAccepted)
	}))
	defer auditServer.Close()

	os.Setenv("AUDIT_ENDPOINT_URL", auditServer.URL)
	os.Setenv("AUDIT_API_KEY", "test-key")
	os.Setenv("AUDIT_TIMEOUT_SECONDS", "1")
	os.Setenv("AUDIT_MAX_BODY_BYTES", "1024")
	auditservice.InitFromEnv()
	defer func() {
		os.Unsetenv("AUDIT_ENDPOINT_URL")
		os.Unsetenv("AUDIT_API_KEY")
		os.Unsetenv("AUDIT_TIMEOUT_SECONDS")
		os.Unsetenv("AUDIT_MAX_BODY_BYTES")
		auditservice.InitFromEnv()
	}()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("id", 42)
		c.Set("username", "alice")
		c.Set("token_id", 12)
		c.Set("token_name", "prod-key")
		c.Next()
	})
	router.Use(Audit())
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.JSON(http.StatusCreated, gin.H{"message": "ok"})
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"prompt":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "audit-test")
	request.RemoteAddr = "203.0.113.10:12345"

	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("expected handler response status 201, got %d", recorder.Code)
	}

	select {
	case event := <-events:
		if event.User.ID != 42 || event.User.Username != "alice" {
			t.Fatalf("unexpected user dimensions: %+v", event.User)
		}
		if event.Key.ID != 12 || event.Key.Name != "prod-key" {
			t.Fatalf("unexpected key dimensions: %+v", event.Key)
		}
		if event.Client.IP != "203.0.113.10" {
			t.Fatalf("unexpected client ip: %q", event.Client.IP)
		}
		if event.Request.Content != `{"prompt":"hello"}` {
			t.Fatalf("unexpected request body: %q", event.Request.Content)
		}
		if !strings.Contains(event.Response.Content, `"message":"ok"`) {
			t.Fatalf("unexpected response body: %q", event.Response.Content)
		}
		if event.Response.StatusCode != http.StatusCreated {
			t.Fatalf("unexpected response status: %d", event.Response.StatusCode)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audit event")
	}
}
