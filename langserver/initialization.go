package langserver

import "github.com/saibing/bingo/pkg/lsp"

// This file contains Go-specific extensions to LSP types.
//
// The Go language server MUST NOT rely on these extensions for
// standalone operation on the local file system. (VSCode has no way
// of including these fields.)

// InitializationOptions are the options supported by go-langserver. It is the
// Config struct, but each field is optional.
type InitializationOptions struct {
	// DisableFuncSnippet is an optional version of Config.DisableFuncSnippet
	DisableFuncSnippet *bool `json:"disableFuncSnippet"`

	// DiagnosticsEnabled enables handling of diagnostics
	//
	// Defaults to false if not specified.
	DiagnosticsDisabled *bool `json:"diagnosticsDisabled"`

	// UseGlobalCache enable global cache when hover, reference, definition. Can be overridden by InitializationOptions.
	//
	// Defaults to false if not specified
	UseGlobalCache *bool `json:"useGlobalCache"`

	// MaxParallelism is an optional version of Config.MaxParallelism
	MaxParallelism *int `json:"maxParallelism"`
}

type InitializeParams struct {
	lsp.InitializeParams

	InitializationOptions *InitializationOptions `json:"initializationOptions,omitempty"`

	// TODO these should be InitializationOptions
	// RootImportPath is the root Go import path for this
	// workspace. For example,
	// "golang.org/x/tools" is the root import
	// path for "github.com/golang/tools".
	RootImportPath string
}
