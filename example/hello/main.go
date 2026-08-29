package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

func main() {
	fmt.Printf("hello from a native %s/%s workload\n", runtime.GOOS, runtime.GOARCH)
	fmt.Printf("pid=%d hostname=%s\n", os.Getpid(), hostname())
	fmt.Printf("args=%q\n", os.Args[1:])
	fmt.Printf("cwd=%s\n", must(os.Getwd()))
}

func hostname() string {
	host, err := os.Hostname()
	if err != nil {
		return "<error: " + err.Error() + ">"
	}
	return host
}

func must(value string, err error) string {
	if err != nil {
		return "<error: " + strings.TrimSpace(err.Error()) + ">"
	}
	return value
}
