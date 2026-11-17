package store

// 对抗审计（包内）：文件名映射、Windows 保留设备名、半行裁剪的边界。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// F-SS13（major，Windows）：Windows 把 CON/NUL/AUX/PRN/COM1..9/LPT1..9 当作设备名，
// 且"设备名.任意扩展名"也算设备名。validStem 只要字符集合法就返回 true，于是
// fileStem("con") == "con"，sessionPath 得到 "<dir>/con.ndjson"。
//
// 在 Windows 上 os.OpenFile("<dir>/con.ndjson", O_CREATE|O_EXCL) 会打开 **CON 设备**
// 而不是创建文件：Create 要么报一个与"会话 ID 冲突"无关的 Win32 错误，要么把
// index 条目建成、文件却落在设备里（幽灵会话）。修法：保留名不当作可读文件名，
// 走编码分支（v0.20 起是**可逆**的 enc-<hex>，重启后能还原原始 ID）。
func TestAuditReservedDeviceNamesAreEncoded(t *testing.T) {
	reserved := []string{
		"con", "CON", "nul", "NUL", "aux", "prn",
		"com1", "COM1", "com9", "lpt1", "LPT9",
		"con.txt", "nul.backup", "aux.v2", "PRN.log",
	}
	for _, id := range reserved {
		t.Run(id, func(t *testing.T) {
			if err := checkID(id); err != nil {
				t.Fatalf("checkID(%q) = %v：ID 本身是合法的，问题只在文件名映射", id, err)
			}
			stem := fileStem(id)
			if strings.EqualFold(stem, id) {
				t.Fatalf("fileStem(%q) = %q：Windows 上这是保留设备名，必须走哈希分支", id, stem)
			}
			if !strings.HasPrefix(stem, "enc-") && !strings.HasPrefix(stem, "id-") {
				t.Fatalf("fileStem(%q) = %q, want enc-<hex> 或 id-<hash>", id, stem)
			}
			// 编码必须可逆：扫目录靠文件名还原原始 ID（否则重启后续传断链）。
			decoded, ok := idFromStem(stem)
			if !ok || decoded != id {
				t.Fatalf("idFromStem(%q) = (%q, %v), want (%q, true)", stem, decoded, ok, id)
			}
		})
	}

	// 正常名字必须保持可读（不要为了修设备名把所有文件名都哈希掉）。
	for _, id := range []string{"session-1", "a.b", "user@example", "ABC_123"} {
		if got := fileStem(id); got != id {
			t.Fatalf("fileStem(%q) = %q, want 原样", id, got)
		}
	}
}

// F-SS14：两个不同 ID 不能映射到同一个文件（否则后建者会覆盖别人的数据）。
func TestAuditFileStemIsInjective(t *testing.T) {
	ids := []string{
		"a", "A", "a b", "a\tb", "全中文", "id-" + strings.Repeat("0", 32),
		"x/y", "x\\y", ".hidden", "a.b", "a_b",
	}
	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		stem := fileStem(id)
		if other, dup := seen[stem]; dup {
			t.Fatalf("ID %q 与 %q 映射到同一个文件名 %q", id, other, stem)
		}
		seen[stem] = id
	}
}

// F-SS15：trimPartialTail 的边界（空文件、正好以换行结尾、没有换行、多块扫描）。
func TestAuditTrimPartialTail(t *testing.T) {
	dir := t.TempDir()
	write := func(data []byte) string {
		t.Helper()
		path := filepath.Join(dir, "f.ndjson")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("写入: %v", err)
		}
		return path
	}
	cases := []struct {
		name  string
		data  string
		want  string
		noErr bool
	}{
		{"空文件", "", "", true},
		{"以换行结尾", "a\nbb\n", "a\nbb\n", true},
		{"无换行", "abc", "", true},
		{"半行", "a\nbb\nccc", "a\nbb\n", true},
		{"多块扫描", strings.Repeat("x", 9000) + "\n" + "tail", strings.Repeat("x", 9000) + "\n", true},
		{"全是换行", "\n\n", "\n\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write([]byte(tc.data))
			file, err := os.OpenFile(path, os.O_RDWR, 0o600)
			if err != nil {
				t.Fatalf("打开: %v", err)
			}
			defer func() { _ = file.Close() }()
			if err := trimPartialTail(file); err != nil {
				t.Fatalf("trimPartialTail: %v", err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读回: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("裁剪结果 = %q, want %q", got, tc.want)
			}
		})
	}
}
