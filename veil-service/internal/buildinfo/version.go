// Package buildinfo exposes the release identity supplied by the build tool.
package buildinfo

import "runtime"

var Version = "dev"

func String() string { return "Veil " + Version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")" }
