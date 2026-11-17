package gitsnap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitCommand 是执行 git 的统一入口。
//
// 全部用 `git -C <dir>`：路径语义完全交给 git（Windows 上同样成立），也避免我们
// 自己做路径拼接。
type gitCommand struct {
	dir  string
	env  []string
	args []string
}

// git 返回一个绑定到 dir 的命令构造器。
func git(dir string) *gitCommand { return &gitCommand{dir: dir} }

// env 追加环境变量（例如 GIT_INDEX_FILE）。
func (c *gitCommand) withEnv(pairs ...string) *gitCommand {
	c.env = append(c.env, pairs...)
	return c
}

// with 追加参数。
func (c *gitCommand) with(args ...string) *gitCommand {
	c.args = append(c.args, args...)
	return c
}

// run 执行命令，返回 stdout；失败时把 stderr 带进错误。
//
// stdin 为 nil 表示不写标准输入。
func (c *gitCommand) run(ctx context.Context, stdin []byte) ([]byte, error) {
	full := append([]string{"-C", c.dir}, c.args...)
	command := exec.CommandContext(ctx, "git", full...)
	base := os.Environ()
	// 永不交互：个人客户端里出现凭据提示会直接把 agent 冻住。
	base = append(base, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0")
	command.Env = append(base, c.env...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("gitsnap: git %s: %w", c.describe(), ctxErr)
	}
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		detail = err.Error()
	}
	return nil, fmt.Errorf("gitsnap: git %s: %s", c.describe(), detail)
}

// describe 返回可读的命令描述（错误文案用）。
func (c *gitCommand) describe() string {
	return strings.Join(c.args, " ")
}

// gitError 报告 err 是否来自 git 的某个特定 stderr 文案。
func gitError(err error, needle string) bool {
	return err != nil && strings.Contains(err.Error(), needle)
}

// errNotDir 表示 repoDir 不是一个存在的目录。
var errNotDir = errors.New("gitsnap: repository path is not a directory")

// ensureDir 校验 dir 是存在的目录。
func ensureDir(dir string) error {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		return errors.New("gitsnap: repository path is empty")
	}
	info, err := os.Stat(trimmed)
	if err != nil {
		return fmt.Errorf("gitsnap: %q: %w", trimmed, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %q", errNotDir, trimmed)
	}
	return nil
}

// tempIndex 在系统临时目录创建一个一次性 index 文件路径（调用方负责清理）。
//
// 用临时 index 是为了让"读树/写树/加缓存项"这类操作完全不碰用户的 .git/index；
// 放在系统临时目录（而不是仓库里）是为了不污染工作区、也不进快照。
func tempIndex() (string, func(), error) {
	file, err := os.CreateTemp("", "gitsnap-*.index")
	if err != nil {
		return "", nil, fmt.Errorf("gitsnap: create temp index: %w", err)
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", nil, fmt.Errorf("gitsnap: create temp index: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return "", nil, fmt.Errorf("gitsnap: prepare temp index: %w", err)
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", nil, fmt.Errorf("gitsnap: resolve temp index: %w", err)
	}
	return absolute, func() { _ = os.Remove(absolute) }, nil
}

// identityEnv 是构造隐藏提交时使用的固定身份。
//
// 隐藏提交不属于用户，因此不依赖（也可能缺失的）user.name/user.email 配置；直接给
// 环境变量可以保证在任何仓库里都能成功。
var identityEnv = []string{
	"GIT_AUTHOR_NAME=rig",
	"GIT_AUTHOR_EMAIL=rig@localhost",
	"GIT_COMMITTER_NAME=rig",
	"GIT_COMMITTER_EMAIL=rig@localhost",
}
