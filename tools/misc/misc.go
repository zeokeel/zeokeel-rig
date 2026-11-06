// Package misc provides small self-contained tools that keep session-local
// state in memory: working_directory (navigate a filesystem root) and
// todo_write (the model's todo list).
//
// Both tools are stateful on purpose, but the state lives in the tool value (or
// its store), never in package variables: a host creates one tool per agent and
// the tool is safe for concurrent calls. State is process-local and is lost on
// restart, which matches how these tools are used (scratch space for one run).
package misc
