package sandbox_test

import (
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig/sandbox"
)

// FuzzParseShellCommand checks that the shared shell parser never panics on
// arbitrary model/terminal input, always returns sandbox.ErrDenied for policy
// rejections, and is deterministic. The interesting security properties are
// covered by the table-driven bypass corpus; fuzzing protects the lexer itself.
func FuzzParseShellCommand(f *testing.F) {
	seeds := []string{
		"echo hi",
		`echo "a;b"`,
		"echo \"$(id)\"",
		"echo `id`",
		"echo \"$\\\n(id)\"",
		"echo hi \\\n| id",
		"echo 'a\\\nb'",
		"echo ${x:-$(id)}",
		"cat <(id)",
		"echo hi # ; comment",
		"echo \"unterminated",
		"",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, command string) {
		policy := sandbox.New(sandbox.WithCommandAllowlist("echo"))
		words, err := policy.ParseShellCommand(command)
		if err != nil && !errors.Is(err, sandbox.ErrDenied) {
			t.Fatalf("ParseShellCommand(%q) returned a non-policy error: %v", command, err)
		}
		wordsAgain, errAgain := policy.ParseShellCommand(command)
		if (err == nil) != (errAgain == nil) || len(words) != len(wordsAgain) {
			t.Fatalf("ParseShellCommand(%q) is not deterministic: %v / %v", command, err, errAgain)
		}
		if err == nil {
			for i := range words {
				if words[i] != wordsAgain[i] {
					t.Fatalf("ParseShellCommand(%q) is not deterministic at word %d", command, i)
				}
			}
		}
		if err == nil {
			_ = policy.CheckShellCommand(command) // must also agree that the command is acceptable
		}
	})
}
