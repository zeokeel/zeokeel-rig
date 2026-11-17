//go:build !unix

package instructions_test

import "errors"

// mkfifoHelper 在非 Unix 平台不可用：相关用例应当 Skip 而不是失败。
func mkfifoHelper(path string) error { return errors.New("mkfifo is unavailable on this platform") }
