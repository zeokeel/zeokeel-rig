//go:build !unix

package fsguard_test

import "errors"

// mkfifoForTest 在非 Unix 平台不可用（调用方会 Skip）。
func mkfifoForTest(path string, mode uint32) error { return errors.New("mkfifo is unavailable") }
