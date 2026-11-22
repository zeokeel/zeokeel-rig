package kit

// Capability names a module may contribute. They map onto the typed Config
// slots; a wrong type or unknown name fails the build (no silent ignore).

import (
	"fmt"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/costmeter"
	"github.com/zeokeel/zeokeel-rig/sandbox"
	"github.com/zeokeel/zeokeel-rig/storage"
	"github.com/zeokeel/zeokeel-rig/tools/hosttools"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
	"github.com/zeokeel/zeokeel-rig/tools/web"
)

// ModuleCapabilities is the optional interface a Module uses to fill typed
// Config slots (memory, runner, gate, ...).
type ModuleCapabilities interface {
	Capabilities() map[string]any
}

// ApplyCapabilities assigns module-provided capabilities to cfg. Unknown names
// and wrong types are errors so a typo in a module cannot silently drop a
// capability.
func ApplyCapabilities(cfg *Config, capabilities map[string]any) error {
	for name, value := range capabilities {
		if err := applyCapability(cfg, name, value); err != nil {
			return err
		}
	}
	return nil
}

func applyCapability(cfg *Config, name string, value any) error {
	assign := func(ok bool) error {
		if !ok {
			return fmt.Errorf("kit: capability %q has type %T, which does not fit the slot", name, value)
		}
		return nil
	}
	switch name {
	case "provider":
		typed, ok := value.(rig.Provider)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Provider = typed
	case "gate":
		typed, ok := value.(rig.Gate)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Gate = typed
	case "history":
		typed, ok := value.(rig.History)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.History = typed
	case "asker":
		typed, ok := value.(rig.Asker)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Asker = typed
	case "approvals":
		typed, ok := value.(rig.ApprovalStore)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Approvals = typed
	case "searcher":
		typed, ok := value.(web.Searcher)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Searcher = typed
	case "sessionLog":
		typed, ok := value.(rig.SessionLog)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.SessionLog = typed
	case "skills":
		typed, ok := value.(rig.SkillStore)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Skills = typed
	case "teamTasks":
		typed, ok := value.(rig.TeamTaskManager)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.TeamTasks = typed
	case "scheduler":
		typed, ok := value.(rig.Scheduler)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Scheduler = typed
	case "reviewer":
		typed, ok := value.(rig.Reviewer)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Reviewer = typed
	case "runner":
		typed, ok := value.(rig.SubagentRunner)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Runner = typed
	case "presenter":
		typed, ok := value.(rig.FilePresenter)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Presenter = typed
	case "artifactPresenter":
		typed, ok := value.(rig.Presenter)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.ArtifactPresenter = typed
	case "goals":
		typed, ok := value.(rig.GoalStore)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Goals = typed
	case "goalRevisions":
		typed, ok := value.(hosttools.GoalRevisionStore)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.GoalRevisions = typed
	case "planController":
		typed, ok := value.(rig.PlanController)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.PlanController = typed
	case "checkpointStore":
		typed, ok := value.(rig.CheckpointStore)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.CheckpointStore = typed
	case "spill":
		typed, ok := value.(*rig.SpillPolicy)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Spill = typed
	case "sandboxDenialSink":
		typed, ok := value.(func(sandbox.Denial))
		if err := assign(ok); err != nil {
			return err
		}
		cfg.SandboxDenialSink = typed
	case "compactor":
		typed, ok := value.(rig.Compactor)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Compactor = typed
	case "activations":
		typed, ok := value.(rig.ToolActivationStore)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Activations = typed
	case "jobs":
		typed, ok := value.(*jobs.Manager)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Jobs = typed
	case "sandboxRunner":
		typed, ok := value.(sandbox.Runner)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.SandboxRunner = typed
	case "policy":
		typed, ok := value.(*sandbox.Policy)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Policy = typed
	case "storage":
		typed, ok := value.(Storage)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Storage = typed
	case "aggregate":
		typed, ok := value.(storage.Storage)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Aggregate = typed
	case "meter":
		typed, ok := value.(*costmeter.Meter)
		if err := assign(ok); err != nil {
			return err
		}
		cfg.Meter = typed
	default:
		return fmt.Errorf("kit: unknown capability %q", name)
	}
	return nil
}
