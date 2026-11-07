// Command rigdoc 从真实工具构造器生成 AI 可读的 Rig 工具目录。
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/zeokeel/zeokeel-rig/internal/rigdoc"
)

func main() {
	check := flag.Bool("check", false, "校验已生成文件是否最新，而不是写入")
	flag.Parse()
	jsonPath := rigdoc.JSONPath
	mdPath := rigdoc.MarkdownPath
	if args := flag.Args(); len(args) > 0 {
		jsonPath = args[0]
	}
	if len(flag.Args()) > 1 {
		mdPath = flag.Args()[1]
	}
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "rigdoc:", err)
		os.Exit(1)
	}
	if *check {
		if err := rigdoc.Check(jsonPath, mdPath); err != nil {
			fail(err)
		}
		return
	}
	if err := rigdoc.Write(jsonPath, mdPath); err != nil {
		fail(err)
	}
	docs := rigdoc.BuildCatalog()
	fmt.Printf("wrote %s (%d tools) and %s\n", jsonPath, len(docs), mdPath)
}
