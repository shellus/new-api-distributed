package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/app"
	"github.com/QuantumNous/new-api/model"
)

var validateChannels = flag.Bool("validate-channels", false, "validate edge channel YAML configs and exit")

func main() {
	flag.Parse()
	if *validateChannels {
		names, err := model.ValidateEdgeLocalChannelConfigs()
		if err != nil {
			fmt.Fprintln(os.Stderr, "edge channel config invalid: "+err.Error())
			os.Exit(1)
		}
		for _, name := range names {
			fmt.Println(name)
		}
		return
	}
	if err := app.Run(app.Config{Mode: common.RuntimeModeEdge}); err != nil {
		common.FatalLog(err)
	}
}
