//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"veil-service/local"
	"veil-service/platform/openwrt/rpc"
)

func main() {
	if err := rpc.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, local.Call); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
