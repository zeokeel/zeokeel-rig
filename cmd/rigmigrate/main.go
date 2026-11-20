// Command rigmigrate 把宿主代码里用到"已删除 / 已改签名"的 Rig API 一次列全。
//
// 它解决的问题：升级 Rig 时宿主编译器只会一条条报错——改一个、再编、再报下一个。
// rigmigrate 用 go/parser 只读扫描一个宿主目录，按符号表（internal/rigmigrate）
// 报告"文件:行、旧符号、替代做法"，让迁移从"猜"变成"照着清单改"。
//
// 用法：
//
//	go run github.com/zeokeel/zeokeel-rig/cmd/rigmigrate -C ./host     # 人读
//	go run github.com/zeokeel/zeokeel-rig/cmd/rigmigrate -C ./host -json
//
// 退出码：命中 → 1，干净 → 0，用法/读取/解析错误 → 2。
// 它不编译也不修改代码：宿主升级时的真实状态就是"编译不过"，扫描必须能在那里工作。
// 完整的迁移说明见 docs/MIGRATION-v0.25.md。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/zeokeel/zeokeel-rig/internal/rigmigrate"
)

// 退出码。
const (
	exitClean = 0 // 没有命中
	exitFound = 1 // 有命中
	exitUsage = 2 // 用法、读取或解析错误
)

// jsonReport 是 -json 的输出形态。Findings 永远是非 nil 切片（干净时是 `[]`）。
type jsonReport struct {
	Root     string               `json:"root"`
	Count    int                  `json:"count"`
	Findings []rigmigrate.Finding `json:"findings"`
}

// main 把 run 的返回值交给 os.Exit，方便测试直接调 run。
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 执行一次扫描并返回退出码。
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("rigmigrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("C", ".", "host directory to scan")
	asJSON := flags.Bool("json", false, "print machine-readable JSON")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: rigmigrate [-C dir] [-json]")
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Lists uses of Rig APIs removed or re-signature in v0.25.")
		fmt.Fprintln(stderr, "Exit codes: 0 clean, 1 findings, 2 usage error.")
		fmt.Fprintln(stderr)
		flags.PrintDefaults()
	}

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitClean
		}
		return exitUsage
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "rigmigrate: unexpected arguments: %s\n", strings.Join(flags.Args(), " "))
		flags.Usage()
		return exitUsage
	}

	findings, err := rigmigrate.Scan(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "rigmigrate: %v\n", err)
		return exitUsage
	}
	if findings == nil {
		findings = []rigmigrate.Finding{}
	}

	if *asJSON {
		report := jsonReport{Root: *dir, Count: len(findings), Findings: findings}
		encoded, marshalErr := json.MarshalIndent(report, "", "  ")
		if marshalErr != nil {
			fmt.Fprintf(stderr, "rigmigrate: encode report: %v\n", marshalErr)
			return exitUsage
		}
		fmt.Fprintf(stdout, "%s\n", encoded)
	} else {
		for _, finding := range findings {
			fmt.Fprintln(stdout, finding.String())
		}
		if len(findings) > 0 {
			fmt.Fprintf(stderr, "rigmigrate: %d removed API use(s) found; see docs/MIGRATION-v0.25.md\n", len(findings))
		}
	}

	if len(findings) > 0 {
		return exitFound
	}
	return exitClean
}
