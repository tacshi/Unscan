//go:build e2e

package main

// Lets another process drive the app, as mygo-automation does for an agent
// checking a change: built with -tags e2e, it listens on the socket that
// MYGO_AUTOMATION names, and does nothing without it. Builds without the
// inspector, as releases are, leave it out whatever their tags.
import _ "github.com/egoist/mygo/plugins/automation"
