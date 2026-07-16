package main

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/app"
)

func main() {
	if err := app.Run(app.Config{Mode: common.RuntimeModeEdge}); err != nil {
		common.FatalLog(err)
	}
}
