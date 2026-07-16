package main

import (
	"embed"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/app"
	"github.com/QuantumNous/new-api/router"
)

//go:embed web/default/dist
var buildFS embed.FS

//go:embed web/default/dist/index.html
var indexPage []byte

//go:embed web/classic/dist
var classicBuildFS embed.FS

//go:embed web/classic/dist/index.html
var classicIndexPage []byte

func main() {
	err := app.Run(app.Config{
		Mode: common.RuntimeModeMaster,
		ThemeAssets: router.ThemeAssets{
			DefaultBuildFS:   buildFS,
			DefaultIndexPage: indexPage,
			ClassicBuildFS:   classicBuildFS,
			ClassicIndexPage: classicIndexPage,
		},
	})
	if err != nil {
		common.FatalLog(err)
	}
}
