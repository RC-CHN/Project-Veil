module veil.local/node

go 1.26.0

toolchain go1.26.7

require (
	golang.org/x/sys v0.47.0
	veil.local/core v0.0.0
)

replace veil.local/core => ../core
