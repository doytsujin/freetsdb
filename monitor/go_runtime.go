package monitor

import (
	"runtime"

	"github.com/freetsdb/freetsdb/monitor/diagnostics"
)

// goRuntime captures Go runtime diagnostics
type goRuntime struct{}

func (g *goRuntime) Diagnostics() (*diagnostics.Diagnostics, error) {
	diagnostics := map[string]interface{}{
		"GOARCH":     runtime.GOARCH,
		"GOOS":       runtime.GOOS,
		"GOMAXPROCS": runtime.GOMAXPROCS(-1),
		"version":    runtime.Version(),
	}

	return DiagnosticsFromMap(diagnostics), nil
}
