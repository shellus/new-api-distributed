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

func TestAuditMiddlewareReportsConversationEvidence(t *testing.T) {
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
	os.Setenv("AUDIT_MAX_BODY_BYTES", "4096")
	auditservice.InitFromEnv()
	defer func() {
		os.Unsetenv("AUDIT_ENDPOINT_URL")
		os.Unsetenv("AUDIT_API_KEY")
		os.Unsetenv("AUDIT_TIMEOUT_SECONDS")
		os.Unsetenv("AUDIT_MAX_BODY_BYTES")
		auditservice.InitFromEnv()
	}()

	router := gin.New()
	router.Use(Audit())
	router.POST("/v1/responses", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": "resp-current", "object": "response"})
	})

	body := `{"conversation_id":"conv-body","session_id":"body-session","previous_response_id":"resp-prev","prompt_cache_key":"cache-key","metadata":{"conversation_id":"meta-conv","user_id":"user_x_account__session_11111111-2222-3333-4444-555555555555","session_id":"meta-session"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Session_id", "codex-session")
	request.Header.Set("X-Amp-Thread-Id", "T-amp-thread")
	request.Header.Set("X-Session-ID", "explicit-session")
	request.Header.Set("X-Client-Request-Id", "client-request")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	select {
	case event := <-events:
		candidates := event.Conversation.Candidates
		for key, expected := range map[string]string{
			"header.session_id":             "codex-session",
			"header.x_amp_thread_id":        "T-amp-thread",
			"header.x_session_id":           "explicit-session",
			"header.x_client_request_id":    "client-request",
			"body.conversation_id":          "conv-body",
			"body.session_id":               "body-session",
			"body.previous_response_id":     "resp-prev",
			"body.prompt_cache_key":         "cache-key",
			"body.metadata.conversation_id": "meta-conv",
			"body.metadata.user_id":         "user_x_account__session_11111111-2222-3333-4444-555555555555",
			"body.metadata.user_id_session": "11111111-2222-3333-4444-555555555555",
			"body.metadata.session_id":      "meta-session",
			"response.id":                   "resp-current",
		} {
			if candidates[key] != expected {
				t.Fatalf("expected conversation candidate %s=%q, got %q in %+v", key, expected, candidates[key], candidates)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audit event")
	}
}

func TestAuditMiddlewareReportsConversationMessageIDFromSSE(t *testing.T) {
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
	os.Setenv("AUDIT_MAX_BODY_BYTES", "4096")
	auditservice.InitFromEnv()
	defer func() {
		os.Unsetenv("AUDIT_ENDPOINT_URL")
		os.Unsetenv("AUDIT_API_KEY")
		os.Unsetenv("AUDIT_TIMEOUT_SECONDS")
		os.Unsetenv("AUDIT_MAX_BODY_BYTES")
		auditservice.InitFromEnv()
	}()

	router := gin.New()
	router.Use(Audit())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("event: message_start\n")
		_, _ = c.Writer.WriteString("data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stream\"}}\n\n")
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	select {
	case event := <-events:
		if event.Conversation.Candidates["response.message_id"] != "msg-stream" {
			t.Fatalf("expected SSE response.message_id evidence, got %+v", event.Conversation.Candidates)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audit event")
	}
}

func TestAuditMiddlewareReportsConversationResponseIDFromSSE(t *testing.T) {
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
	os.Setenv("AUDIT_MAX_BODY_BYTES", "4096")
	auditservice.InitFromEnv()
	defer func() {
		os.Unsetenv("AUDIT_ENDPOINT_URL")
		os.Unsetenv("AUDIT_API_KEY")
		os.Unsetenv("AUDIT_TIMEOUT_SECONDS")
		os.Unsetenv("AUDIT_MAX_BODY_BYTES")
		auditservice.InitFromEnv()
	}()

	router := gin.New()
	router.Use(Audit())
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		_, _ = c.Writer.WriteString("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream\"}}\n\n")
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, request)

	select {
	case event := <-events:
		if event.Conversation.Candidates["response.id"] != "resp-stream" {
			t.Fatalf("expected SSE response.id evidence, got %+v", event.Conversation.Candidates)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for audit event")
	}
}
