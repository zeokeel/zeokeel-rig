// Package procattr prepares the platform attributes of the child processes this
// library starts.
//
// On Windows the desktop host is a GUI-subsystem process. A console child it
// starts (cmd, PowerShell, a background job, an MCP or LSP server) can get a
// console window of its own, which users see as a black window popping up for
// every tool call. HideWindow makes that window start hidden; elsewhere it does
// nothing, because a child there owns no window.
//
// Every exec.Command/exec.CommandContext site must call HideWindow on its
// command before starting it; coverage_test.go enforces that.
package procattr
