package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestEdgeTextBoundaryAllowsSupportedChatAndResponses(t *testing.T) {
	for _, test := range []struct {
		path string
		body string
	}{
		{path: "/v1/chat/completions", body: `{"model":"gpt-test","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"stream":true}`},
		{path: "/v1/responses", body: `{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"tools":[{"type":"function","name":"lookup"}],"stream":true}`},
	} {
		recorder := runEdgeTextBoundaryRequest(test.path, test.body)
		assert.Equal(t, http.StatusNoContent, recorder.Code, recorder.Body.String())
	}
}

func TestEdgeTextBoundaryRejectsUnaccountedFeatures(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "chat web search", path: "/v1/chat/completions", body: `{"model":"gpt-test","messages":[],"web_search_options":{}}`},
		{name: "chat image", path: "/v1/chat/completions", body: `{"model":"gpt-test","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`},
		{name: "chat audio modality", path: "/v1/chat/completions", body: `{"model":"gpt-test","messages":[],"modalities":["text","audio"]}`},
		{name: "chat built-in tool", path: "/v1/chat/completions", body: `{"model":"gpt-test","messages":[],"tools":[{"type":"web_search"}]}`},
		{name: "chat search preview model", path: "/v1/chat/completions", body: `{"model":"gpt-4o-search-preview","messages":[]}`},
		{name: "responses web search", path: "/v1/responses", body: `{"model":"gpt-test","input":"hello","tools":[{"type":"web_search"}]}`},
		{name: "responses unknown built-in tool", path: "/v1/responses", body: `{"model":"gpt-test","input":"hello","tools":[{"type":"code_interpreter"}]}`},
		{name: "responses file", path: "/v1/responses", body: `{"model":"gpt-test","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`},
		{name: "responses computer screenshot", path: "/v1/responses", body: `{"model":"gpt-test","input":[{"role":"tool","content":[{"type":"computer_screenshot","image_url":"data:image/png;base64,AA=="}]}]}`},
		{name: "responses background", path: "/v1/responses", body: `{"model":"gpt-test","input":"hello","background":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := runEdgeTextBoundaryRequest(test.path, test.body)
			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Contains(t, recorder.Body.String(), string(errUnsupportedEdgeTextFeature.Error()))
		})
	}
}

func runEdgeTextBoundaryRequest(path string, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST(path, EdgeTextBoundary(), func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", gin.MIMEJSON)
	engine.ServeHTTP(recorder, request)
	return recorder
}
