package main

import (
	"embed"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/app"
	"github.com/QuantumNous/new-api/router"
)

//go:embed web/dist
var buildFS embed.FS

//go:embed web/dist/index.html
var indexPage []byte

func main() {
	err := app.Run(app.Config{
		Mode: common.RuntimeModeMaster,
		WebAssets: router.WebAssets{
			BuildFS:   buildFS,
			IndexPage: indexPage,
		},
	})
	if err != nil {
		common.FatalLog(err)
	}
}
