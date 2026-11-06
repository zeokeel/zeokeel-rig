// Command quickstart is the smallest complete agent built from the Rig SDK: the
// kit facade wires an OpenAI-compatible provider, the full sandboxed tool set, a
// safe read-only approval gate and in-memory history in one import.
//
// It needs credentials to talk to a model:
//
//	OPENAI_API_KEY=... go run ./examples/quickstart "list the repo and summarize it"
//
// The provider defaults to https://api.openai.com/v1. Set Config.BaseURL for an
// OpenAI-compatible gateway and Config.Model for the model. Without an API key
// the program still builds; the first run reports the provider error.
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

	// One import, one constructor: kit.New defaults to the full tool set
	// sandboxed to the working directory, shell execution disabled (no command
	// allowlist), no network, and a gate that only allows
	// read_file/list_dir/glob/grep. Supply Config.Gate/CommandAllowlist to widen
	// what the agent may do.
	a, err := kit.New(kit.Config{
		Model: "gpt-4o-mini",
		// For an OpenAI-compatible gateway:
		// BaseURL: "https://api.deepseek.com/v1", APIKeyEnv: "DEEPSEEK_API_KEY",
	})
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// RunText returns the aggregated assistant text; emit streams it live.
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
