package full

import (
	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// noticeTool decorates an existing tool with rig.NoticeSource so the agent can
// drain completion notices from a manager that is not itself a tool. The
// decorated tool keeps its model-facing name and behavior.
type noticeTool struct {
	rig.Tool
	drain func() []string
}

// PendingNotices implements rig.NoticeSource.
func (t noticeTool) PendingNotices() []string {
	if t.drain == nil {
		return nil
	}
	return t.drain()
}

// jobNoticeText converts settled jobs into DSH-style model-facing notices.
func jobNoticeText(manager *jobs.Manager) func() []string {
	return func() []string {
		if manager == nil {
			return nil
		}
		pending := manager.PendingNotices()
		notices := make([]string, 0, len(pending))
		for _, notice := range pending {
			text := notice.Text
			if text == "" {
				text = notice.JobID + ": " + string(notice.Status)
			}
			notices = append(notices, text)
		}
		return notices
	}
}
