package full

import (
	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/tools/jobs"
)

// noticeTool 用 rig.NoticeSource 装饰一个已有工具，使 agent 能从“本身不是
// 工具”的管理器排空完成通知。被装饰的工具保持其面向模型的名称与行为。
type noticeTool struct {
	rig.Tool
	drain func() []string
}

// PendingNotices 实现 rig.NoticeSource。
func (t noticeTool) PendingNotices() []string {
	if t.drain == nil {
		return nil
	}
	return t.drain()
}

// jobNoticeText 把已结束的任务转换成 DSH 风格的面向模型通知。
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
