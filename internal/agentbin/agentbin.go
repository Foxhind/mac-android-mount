// Package agentbin embeds the cross-compiled device agents. The binaries are
// produced by `make agents` into bin/ before the host program is built.
package agentbin

import (
	"embed"
	"fmt"
)

//go:embed bin/*
var files embed.FS

// Get returns the agent binary for a GOARCH value (arm, arm64, 386, amd64).
func Get(arch string) ([]byte, error) {
	b, err := files.ReadFile("bin/droidfs-agent-" + arch)
	if err != nil {
		return nil, fmt.Errorf("no embedded agent for %s (run `make agents`)", arch)
	}
	return b, nil
}
