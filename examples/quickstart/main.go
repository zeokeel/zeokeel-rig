// Command quickstart 是用 Rig SDK 构建的最小完整 agent：kit 门面在一次 import 中
// 装配好兼容 OpenAI 的 provider、完整的沙箱工具集、安全的只读审批门禁以及内存
// 历史。
//
// 与模型对话需要凭据：
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// provider 默认使用 https://api.openai.com/v1。接入 OpenAI 兼容网关时设置
// Config.BaseURL，并用 Config.Model 选择模型。没有 API key 时程序仍可构建；
// 第一次运行会报告 provider 错误。
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/zeokeel/zeokeel-rig/kit"
)

func main() {
	prompt := strings.Join(os.Args[1:], " ")
	if prompt == "" {
		prompt = "List the files in this directory and summarize them."
	}

	// 一次 import、一个构造函数：kit.New 默认使用完整工具集，沙箱限定在工作目录，
	// 禁用 shell 执行（没有命令允许列表），不联网，并且门禁只放行
	// read_file/list_dir/glob/grep。需要扩大 agent 能力时传入
	// Config.Gate/CommandAllowlist。
	a, err := kit.New(kit.Config{
		Model: "gpt-4o-mini",
		// 接入 OpenAI 兼容网关时：
		// BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// RunText 返回聚合后的助手文本；emit 会实时流出事件。
	_, result, err := a.RunText(ctx, "quickstart", prompt, func(event kit.Event) {
		if event.Type == kit.EventTextDelta {
			fmt.Print(event.Text)
		}
	})
	if err != nil {
		fail(err)
	}
	fmt.Println()
	fmt.Printf("finish=%s iterations=%d tokens=%d\n", result.FinishReason, result.Iterations, result.Usage.TotalTokens)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "quickstart:", err)
	os.Exit(1)
}
