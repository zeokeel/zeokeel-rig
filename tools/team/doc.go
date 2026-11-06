// Package team provides in-process multi-subagent orchestration tools for
// ZeoKeel Rig: team_spawn, team_list, team_send, team_wait, team_last,
// team_close and team_interrupt.
//
// # Composition
//
// The tools are thin wrappers over a Manager. The Manager receives any
// contracts/v1.SubagentRunner; the agent package provides the default in-process
// implementation (agent.NewRunner), and the manager never starts a process
// itself:
//
//	parent := v1.AgentSpec{
//		Provider: prov,
//		Model:    "gpt-4o-mini",
//		Tools:    parentTools, // includes the team tools so the parent can orchestrate
//		History:  session.NewMemory(),
//	}
//	// Children must not orchestrate: exclude the team tools from their tool set.
//	excluded := append(append([]string{}, agent.DefaultExcludedTools...), team.ToolNames()...)
//	runner, err := agent.NewRunner(parent, agent.RunnerOptions{
//		MaxDepth:      2, // the parent plus one level of children by default
//		MaxConcurrent: 4,
//		ExcludedTools: excluded,
//	})
//	if err != nil {
//		return err
//	}
//	manager := team.NewManager(runner)
//	defer manager.CloseAll()
//	registry := tools.NewRegistry(append(parentTools, manager.Tools()...)...)
//
// The model then calls team_spawn to start a child with a first prompt, reads
// the team with team_list, exchanges messages with team_send, fetches results
// with team_wait/team_last, and stops children with team_interrupt (cancel the
// current run, keep the member listed) or team_close (stop and remove).
//
// # Lifecycle
//
// Spawning is asynchronous: team_spawn returns a member id/name immediately
// while the child's first run continues in the background. Member names and ids
// are resolved by id first, then by unique name; two members may share a name,
// in which case name references report the ambiguity and the id must be used.
//
// A member is active, interrupted (stop requested, still listed) or closed
// (stopped and removed). Every live member counts against the manager limit
// (8 by default, see WithMaxMembers). The host calls Manager.CloseAll when the
// agent instance goes away; it stops every live child and makes the manager
// reject further work.
//
// # Limits
//
// The Manager is one process's view of its children: it does not persist team
// state across restarts, share members across processes, or supervise a
// cross-process agent runtime. A real runner executes each child in-process on
// the parent's provider/model/tool configuration.
package team
