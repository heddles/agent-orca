// Package agentrefs holds what the operator and the UI API must agree on about
// the things an Agent's spec refers to.
package agentrefs

// BuiltinTools are injected automatically by the router and don't exist as Tool CRDs.
var BuiltinTools = map[string]bool{
	"_spawn": true, "_handoff": true, "_clarify": true, "_done": true,
	"_fail": true, "_emit_event": true, "_rag_ingest": true, "_rag_search": true,
	"_write_state": true, "_read_state": true, "_list_state": true, "_delete_state": true,
	"_memory_store": true, "_mcp_read_resource": true, "_propose_step": true,
	"_propose_fix": true, "_confirm_fix": true, "_create_workflow": true, "_list_resources": true,
	"_search_history": true,
	// fs_* relay tools (issue #34) are injected from Agent.spec.filesystem;
	// listing them in spec.tools must not require Tool CRs.
	"fs_read_text_file": true, "fs_write_text_file": true, "fs_list_dir": true,
}
