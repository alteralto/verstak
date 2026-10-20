// Package workplace holds what the agent reads at its workbench: its instructions. They are
// copied into every workspace, so the agent loads them itself and the start prompt stays short.
package workplace

import "embed"

//go:embed *.md
var Files embed.FS
