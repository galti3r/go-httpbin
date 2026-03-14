// Package main implements the go-httpbin command line tool.
package main

import (
	"os"

	"github.com/galti3r/go-httpbin/v3/httpbin/cmd"
)

func main() {
	os.Exit(cmd.Main())
}
