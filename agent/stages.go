package agent

import (
	"fmt"
	"slices"

	"github.com/zeokeel/zeokeel-rig"
)

// stageOrder returns the declared order of a stage (0 when it does not
// implement rig.Ordered).
func stageOrder(stage any) int {
	if ordered, ok := stage.(rig.Ordered); ok {
		return ordered.Order()
	}
	return 0
}

// orderedStages returns a stable copy sorted by Order (registration order on
// ties).
func orderedStages[T any](stages []T) []T {
	if len(stages) < 2 {
		return stages
	}
	out := slices.Clone(stages)
	slices.SortStableFunc(out, func(left, right T) int {
		return stageOrder(left) - stageOrder(right)
	})
	return out
}

// runPromptStages applies the run's prompt stages (no-op when none).
func (r *run) runPromptStages() error {
	stages := r.a.promptStages
	if len(stages) == 0 {
		return nil
	}
	draft := rig.PromptDraft{SessionID: r.sessionID, Model: r.model, SystemPrompt: r.systemPrompt}
	for _, stage := range stages {
		if err := stage.Prompt(r.ctx, &draft); err != nil {
			return fmt.Errorf("rig: prompt stage: %w", err)
		}
	}
	r.systemPrompt = draft.SystemPrompt
	return nil
}

// runRequestStages lets stages rewrite one model request.
func (r *run) runRequestStages(draft *rig.RequestDraft) error {
	for _, stage := range r.a.requestStages {
		if err := stage.Request(r.ctx, draft); err != nil {
			return fmt.Errorf("rig: request stage: %w", err)
		}
	}
	return nil
}

// runResponseStages lets stages rewrite one assembled assistant message.
func (r *run) runResponseStages(draft *rig.ResponseDraft) error {
	for _, stage := range r.a.responseStages {
		if err := stage.Response(r.ctx, draft); err != nil {
			return fmt.Errorf("rig: response stage: %w", err)
		}
	}
	return nil
}

// runTurnStartStages runs the turn-start stages.
func (r *run) runTurnStartStages(iteration int) error {
	if len(r.a.turnStages) == 0 {
		return nil
	}
	info := &rig.TurnInfo{SessionID: r.sessionID, Model: r.model, Iteration: iteration, Result: r.result}
	for _, stage := range r.a.turnStages {
		if err := stage.TurnStart(r.ctx, info); err != nil {
			return fmt.Errorf("rig: turn start stage: %w", err)
		}
	}
	return nil
}

// runTurnEndStages runs the turn-end stages.
func (r *run) runTurnEndStages(iteration int) error {
	if len(r.a.turnStages) == 0 {
		return nil
	}
	info := &rig.TurnInfo{SessionID: r.sessionID, Model: r.model, Iteration: iteration, Result: r.result}
	for _, stage := range r.a.turnStages {
		if err := stage.TurnEnd(r.ctx, info); err != nil {
			return fmt.Errorf("rig: turn end stage: %w", err)
		}
	}
	return nil
}

// runCompactionStages lets stages rewrite the conversation before compaction.
func (r *run) runCompactionStages(draft *rig.CompactionDraft) error {
	for _, stage := range r.a.compactionStages {
		if err := stage.Compact(r.ctx, draft); err != nil {
			return fmt.Errorf("rig: compaction stage: %w", err)
		}
	}
	return nil
}
