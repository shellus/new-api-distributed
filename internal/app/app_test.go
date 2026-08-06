package app

import (
	"testing"

	"github.com/QuantumNous/new-api/router"

	"github.com/stretchr/testify/assert"
)

func TestInjectAnalyticsUpdatesWebAssetsWithoutMutatingInput(t *testing.T) {
	t.Setenv("UMAMI_WEBSITE_ID", "umami-site")
	t.Setenv("UMAMI_SCRIPT_URL", "https://analytics.example.test/script.js")
	t.Setenv("GOOGLE_ANALYTICS_ID", "G-TEST")

	indexPage := []byte("<head><!--umami-->\n<!--Google Analytics-->\n</head>")
	assets := router.WebAssets{
		IndexPage: indexPage,
	}

	result := injectAnalytics(assets)

	assert.Contains(t, string(result.IndexPage), `src="https://analytics.example.test/script.js"`)
	assert.Contains(t, string(result.IndexPage), `data-website-id="umami-site"`)
	assert.Contains(t, string(result.IndexPage), `gtag('config', 'G-TEST')`)
	assert.Contains(t, string(result.IndexPage), "<!--Umami QuantumNous-->")
	assert.Contains(t, string(result.IndexPage), "<!--Google Analytics QuantumNous-->")
	assert.Equal(t, "<head><!--umami-->\n<!--Google Analytics-->\n</head>", string(indexPage))
}

func TestInjectAnalyticsUsesDefaultUmamiURL(t *testing.T) {
	t.Setenv("UMAMI_WEBSITE_ID", "umami-site")
	t.Setenv("UMAMI_SCRIPT_URL", "")
	t.Setenv("GOOGLE_ANALYTICS_ID", "")

	result := injectAnalytics(router.WebAssets{
		IndexPage: []byte("<!--umami-->\n<!--Google Analytics-->\n"),
	})

	assert.Contains(t, string(result.IndexPage), `src="https://analytics.umami.is/script.js"`)
	assert.NotContains(t, string(result.IndexPage), "<!--umami-->")
	assert.NotContains(t, string(result.IndexPage), "<!--Google Analytics-->\n")
}
