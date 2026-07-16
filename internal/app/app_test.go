package app

import (
	"testing"

	"github.com/QuantumNous/new-api/router"

	"github.com/stretchr/testify/assert"
)

func TestInjectAnalyticsUpdatesBothThemesWithoutMutatingInputs(t *testing.T) {
	t.Setenv("UMAMI_WEBSITE_ID", "umami-site")
	t.Setenv("UMAMI_SCRIPT_URL", "https://analytics.example.test/script.js")
	t.Setenv("GOOGLE_ANALYTICS_ID", "G-TEST")

	defaultPage := []byte("<head><!--umami-->\n<!--Google Analytics-->\n</head>")
	classicPage := []byte("<head><!--umami-->\n<!--Google Analytics-->\n</head>")
	assets := router.ThemeAssets{
		DefaultIndexPage: defaultPage,
		ClassicIndexPage: classicPage,
	}

	result := injectAnalytics(assets)

	for _, page := range [][]byte{result.DefaultIndexPage, result.ClassicIndexPage} {
		assert.Contains(t, string(page), `src="https://analytics.example.test/script.js"`)
		assert.Contains(t, string(page), `data-website-id="umami-site"`)
		assert.Contains(t, string(page), `gtag('config', 'G-TEST')`)
		assert.Contains(t, string(page), "<!--Umami QuantumNous-->")
		assert.Contains(t, string(page), "<!--Google Analytics QuantumNous-->")
	}
	assert.Equal(t, "<head><!--umami-->\n<!--Google Analytics-->\n</head>", string(defaultPage))
	assert.Equal(t, "<head><!--umami-->\n<!--Google Analytics-->\n</head>", string(classicPage))
}

func TestInjectAnalyticsUsesDefaultUmamiURL(t *testing.T) {
	t.Setenv("UMAMI_WEBSITE_ID", "umami-site")
	t.Setenv("UMAMI_SCRIPT_URL", "")
	t.Setenv("GOOGLE_ANALYTICS_ID", "")

	result := injectAnalytics(router.ThemeAssets{
		DefaultIndexPage: []byte("<!--umami-->\n<!--Google Analytics-->\n"),
		ClassicIndexPage: []byte("<!--umami-->\n<!--Google Analytics-->\n"),
	})

	for _, page := range [][]byte{result.DefaultIndexPage, result.ClassicIndexPage} {
		assert.Contains(t, string(page), `src="https://analytics.umami.is/script.js"`)
		assert.NotContains(t, string(page), "<!--umami-->")
		assert.NotContains(t, string(page), "<!--Google Analytics-->\n")
	}
}
