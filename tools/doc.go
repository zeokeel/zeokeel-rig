// Package tools is the tool registry of ZeoKeel Rig: it turns a set of
// root-package (rig) Tool implementations into the registry an agent invokes
// (name-based lookup, deterministic model specs, concurrency-safe snapshots).
//
// Tool implementations live in subpackages so a consumer links only the tools
// it uses:
//
//	tools/builtin   file + shell tools under a sandbox policy
//	tools/editor    str_replace_editor (view/create/str_replace/insert)
//	tools/image     read_image with multimodal ToolResult.Parts
//	tools/terminal  stateful shell sessions (terminal_*)
//	tools/jobs      background processes (job_*)
//	tools/web       web_fetch and pluggable web_search
//	tools/mcp       MCP stdio client and MCP tools/resources/prompts
//	tools/lsp       LSP stdio client and lsp_* tools
//	tools/misc      working_directory and todo_write
package tools
