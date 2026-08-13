// Package proto holds the versioned internal contracts between services.
//
// Generation runs through buf, which is itself a Go tool directive in go.mod:
// no system protoc, no extra download in CI, and the output is reproducible
// from the module graph.
package proto

//go:generate go tool buf generate
