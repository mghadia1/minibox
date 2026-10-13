//go:build !linux

// The probe runs only inside a container, which is Linux-only; this stub
// lets the package compile on other systems (the Mac's `go test ./...`).
package main

func main() {}
