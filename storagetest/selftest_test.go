package storagetest

// 本文件是套件自身的测试（self-test）：证明"基线套件真的会红"。
//
// 一个全是 skip 也能绿的套件比没有套件更危险——它让人以为有测试。所以这里放一个
// **故意坏掉**的 storage.Storage，用子进程把它跑一遍，断言套件确实抓到了它。

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
	"github.com/zeokeel/zeokeel-rig/memory"
	"github.com/zeokeel/zeokeel-rig/storage"
)

// brokenHelperEnv 让同一个测试二进制以"只跑假实现"的模式再启动一次。
const brokenHelperEnv = "STORAGETEST_BROKEN_HELPER"

// brokenStorage 是**故意坏掉**的实现，一次犯三个真实发生过的错误：
//
//  1. Append 静默丢写（每隔几次就返回成功却什么都没存）——并发/分片锁写错的典型症状；
//  2. Close 之后照样接受读写——"关掉了还在跑"，宿主会把写失败的回合当成功；
//  3. 能力位声明支持检查点，Checkpoints() 却返回 nil——用注释冒充能力。
//
// 它存在**只**为了让下面的元测试有东西可抓；不要在任何生产路径里边改边用。
type brokenStorage struct {
	mu       sync.Mutex
	closed   bool
	appends  int
	sessions map[string][]rig.Message
}

var _ storage.Storage = (*brokenStorage)(nil)

func newBrokenStorage(t *testing.T) storage.Storage {
	t.Helper()
	return &brokenStorage{sessions: make(map[string][]rig.Message)}
}

func (b *brokenStorage) History() rig.History             { return brokenHistory{store: b} }
func (b *brokenStorage) Checkpoints() rig.CheckpointStore { return nil }
func (b *brokenStorage) Memory() memory.Wiki              { return nil }
func (b *brokenStorage) Spill() rig.SpillStore            { return nil }

// Close 只置一个标志——bug 3：标志没人读。
func (b *brokenStorage) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

type brokenHistory struct{ store *brokenStorage }

var _ rig.History = brokenHistory{}

func (h brokenHistory) Load(ctx context.Context, sessionID string) ([]rig.Message, error) {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	// bug 2：不看 closed，关闭后照样读得到。
	return append([]rig.Message(nil), h.store.sessions[sessionID]...), nil
}

func (h brokenHistory) Append(ctx context.Context, sessionID string, messages ...rig.Message) error {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	h.store.appends++
	// bug 1a：一次多条的批次里丢掉**最后一条**（经典的 off-by-one），仍返回成功。
	if len(messages) > 1 {
		messages = messages[:len(messages)-1]
	}
	// bug 1b：每第 3 次单条调用直接丢写（并发下"读-改-写"没用同一把锁的症状）。
	if len(messages) == 1 && h.store.appends%3 == 0 {
		return nil
	}
	h.store.sessions[sessionID] = append(h.store.sessions[sessionID], messages...)
	// bug 2：关闭后照样接受写入。
	return nil
}

// TestBaselineCatchesBrokenBackend 是套件的元测试：坏实现必须被抓住。
//
// 手法：用 os.Args[0]（当前测试二进制）加一个环境变量再启动一次，在子进程里调用
// Run 跑假实现；父进程解析子进程输出里的 `--- FAIL: <用例路径>`，断言**具体哪几条
// 用例**红了。只断言"子进程失败"不够——任何一次 panic 也能让它失败；只断言"用例名
// 出现过"也不够——`-test.v` 会把通过的用例同样打出来。
//
// 于是"有人把并发用例删掉/调松"这件事本身会变成一条红灯，这正是套件想守的东西。
func TestBaselineCatchesBrokenBackend(t *testing.T) {
	if os.Getenv(brokenHelperEnv) == "1" {
		// 子进程模式：跑基线套件，故意让它红。
		Run(t, "broken", newBrokenStorage, Capability{History: true, Checkpoints: true})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestBaselineCatchesBrokenBackend$", "-test.v")
	cmd.Env = append(os.Environ(), brokenHelperEnv+"=1")
	output, err := cmd.CombinedOutput()
	text := string(output)
	if err == nil {
		t.Fatalf("故意丢写、关闭后静默成功的假实现居然通过了基线套件：套件抓不到问题\n%s", text)
	}

	failed := failedCaseNames(text)
	for _, want := range []string{
		"HistoryRoundTrip",                  // 批次里丢最后一条
		"HistoryAppendKeepsOrder",           // 同上（分批写入）
		"ConcurrentAppendKeepsEveryMessage", // 并发静默丢写
		"ClosedStorageRejectsAccess",        // 关闭后必须报错
		"CheckpointRoundTrip",               // 能力位不得撒谎
	} {
		if !failed[want] {
			t.Errorf("用例 %q 没有红——它本该抓到假实现的问题。子进程输出：\n%s", want, text)
		}
	}
	t.Logf("基线套件按预期抓到假实现（%d 条用例红，子进程退出：%v）", len(failed), err)
}

// failedCaseNames 从 `go test -v` 输出里摘出失败的用例名（只保留路径最后一段）。
//
// 输出形如 `    --- FAIL: TestX/broken/HistoryRoundTrip (0.00s)`。
func failedCaseNames(output string) map[string]bool {
	failed := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "FAIL:" {
			continue
		}
		path := fields[2]
		failed[path[strings.LastIndex(path, "/")+1:]] = true
	}
	return failed
}

// TestCapabilitiesRunRequiresRegistration 守登记表语义：未登记 ≠ 什么都不支持。
//
// 靠零值兜底会让"忘了登记"变成"静默跳过全部用例"，那正是我们要消灭的假绿。
func TestCapabilitiesRunRequiresRegistration(t *testing.T) {
	registry := Capabilities{"dir": {History: true}}
	if _, ok := registry.Lookup("dir"); !ok {
		t.Fatalf("Lookup(dir) 未命中")
	}
	if caps, ok := registry.Lookup("nobody-registered-this"); ok || caps != (Capability{}) {
		t.Fatalf("Lookup(未登记) = (%+v, %v)，want 零值与 false", caps, ok)
	}
}
