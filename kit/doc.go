// Package kit is ZeoKeel Rig's one-import, batteries-included facade: New wires
// an OpenAI-compatible provider, the full sandboxed tool set, a safe read-only
// approval gate and in-memory session history into a ready-to-run agent.
//
// A complete agent in one import:
//
//	a, err := kit.New(kit.Config{Model: "gpt-4o-mini", Root: "."})
//	if err != nil {
//		log.Fatal(err)
//	}
//	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
//	defer cancel()
//	_, _, err = a.RunText(ctx, "session-1", "list the files in this repo",
//		func(ev kit.Event) {
//			if ev.Type == kit.EventTextDelta {
//				fmt.Print(ev.Text)
//			}
//		})
//
// # Defaults
//
// kit.Config only requires Model; the zero Config plus a model is a working,
// deliberately conservative agent. The "defaults" section of Config documents
// every choice: a full tools/full tool set, a sandbox policy rooted at Root with
// no shell allowlist and no network, an approval gate that only allows
// read_file/list_dir/glob/grep, and a fresh in-memory history.
//
// # Escape hatch
//
// kit is a thin facade, not a framework: every type here is an alias of its
// github.com/zeokeel/zeokeel-rig counterpart (see kit/aliases.go), so a consumer
// that outgrows the defaults can keep the same values and import the specialized
// packages — agent, tools/full, gate, sandbox, session, provider/openai —
// directly. *kit.Agent itself implements rig.Agent.
package kit
