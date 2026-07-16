package common

import (
	"fmt"
	"sync"
)

type RuntimeMode string

const (
	RuntimeModeMaster RuntimeMode = "master"
	RuntimeModeEdge   RuntimeMode = "edge"
)

var (
	runtimeModeMu sync.RWMutex
	runtimeMode   = RuntimeModeMaster
)

func SetRuntimeMode(mode RuntimeMode) error {
	switch mode {
	case RuntimeModeMaster, RuntimeModeEdge:
		runtimeModeMu.Lock()
		runtimeMode = mode
		runtimeModeMu.Unlock()
		return nil
	default:
		return fmt.Errorf("unsupported runtime mode %q", mode)
	}
}

func CurrentRuntimeMode() RuntimeMode {
	runtimeModeMu.RLock()
	defer runtimeModeMu.RUnlock()
	return runtimeMode
}

func IsEdgeMode() bool {
	return CurrentRuntimeMode() == RuntimeModeEdge
}
