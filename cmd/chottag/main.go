package main

import (
	"os"

	"github.com/HaiNNT/c-hottag/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[0], os.Args[1:], os.Stdout, os.Stderr)) }
