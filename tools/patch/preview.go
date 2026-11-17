package patch

// 本文件提供 apply_patch 的**只读**预览能力：解析补丁、在内存里算出每个文件的新
// 内容，然后把"将会发生什么"交给调用方，**一个字节都不落盘**。
//
// 它存在的产品理由：个人 agent 客户端最重要的一条交互是"先给我看 diff，再决定要
// 不要落盘"。审批门禁（gate.DiffPrompt）在工具执行**之前**运行，拿不到工具结果，
// 因此它需要一条与 Invoke 完全相同、但停在 commit 之前的入口——就是这里。
//
// 复用关系是刻意的：解析、校验、路径安全、上下文匹配、+N/-M 统计全部走 Invoke
// 用的同一份代码（parsePatch + buildPlans），因此预览与真正执行**不可能**给出
// 不同结论。commit 是唯一没有调用的阶段。

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// Change 是一处"补丁将会造成"的文件变更（预览结果，不会写盘）。
type Change struct {
	// Path 是补丁里写的、相对工作区根的斜杠路径。
	Path string
	// Op 取 "add" / "modify" / "delete"。
	//
	// 带 hunk 的移动（Move to）也归为 modify，目标路径见 MoveTo。
	Op string
	// MoveTo 是移动目标（相对根）；空表示不移动。
	MoveTo string
	// Old 是变更前内容（add 为 nil）。
	Old []byte
	// New 是变更后内容（delete 为 nil）。
	New []byte
	// Added/Removed 是补丁自己算出的 +N/-M（与 Invoke 的报告一致）。
	Added, Removed int
	// Existed 报告源文件在应用前是否存在（add 恒为 false）。
	Existed bool
}

// PreviewChanges 解析一份补丁文本并返回它将会造成的变更，不修改任何文件。
//
// 语义与 Invoke 完全一致：补丁语法、路径安全（含 policy 的只读/保护区判定）、
// 上下文匹配、重复路径、规模上限都在这里判定；任何一步失败都返回错误，且磁盘
// 上的文件不会有任何改动。
//
// root 是工作区根；policy 可为 nil（只做工具自带的根校验）。返回的 Old/New 是
// 副本，调用方可以自由持有。
func PreviewChanges(root string, patchText string, policy *sandbox.Policy) ([]Change, error) {
	if strings.TrimSpace(patchText) == "" {
		return nil, errors.New("patch: patch text is required and must not be empty")
	}
	if len(patchText) > DefaultMaxPatchBytes {
		return nil, fmt.Errorf("patch: patch is %d bytes, which exceeds the %d byte limit (max_patch_bytes); split it into smaller patches",
			len(patchText), DefaultMaxPatchBytes)
	}
	rootFS, err := newRootFS(root)
	if err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	defer rootFS.Close()

	ops, err := parsePatch(patchText)
	if err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	plans, err := buildPlans(rootFS, limits{maxFileBytes: DefaultMaxFileBytes, maxFiles: DefaultMaxFiles}, policy, ops)
	if err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	return planChanges(plans), nil
}

// planChanges 把执行计划映射成对外可见的变更描述。
//
// PreviewChanges 与 dry_run 的 diff 预览共用它，因此"预览看到的"与"预览接口
// 返回的"不可能不一致。
func planChanges(plans []plan) []Change {
	changes := make([]Change, 0, len(plans))
	for _, item := range plans {
		change := Change{
			Path:    item.rel,
			Added:   item.added,
			Removed: item.removed,
			Existed: item.existed,
			MoveTo:  item.moveRel,
		}
		switch item.op {
		case opAdd:
			change.Op = "add"
			change.New = item.content
		case opDelete:
			change.Op = "delete"
			change.Old = item.old
		default:
			change.Op = "modify"
			change.Old = item.old
			change.New = item.content
		}
		changes = append(changes, change)
	}
	return changes
}
