package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/zeokeel/zeokeel-rig"
)

// recordRead 是一次读取的结果。它是纯数据，不含任何锁语义，便于单测与复用。
type recordRead struct {
	// messages 是成功解析的消息（保持文件顺序）。
	messages []rig.Message
	// records 是成功解析的条数（≠ 文件行数：损坏行与半行都不计）。
	records int
	// size 是文件的物理字节数（含损坏字节）。
	size int64
	// notices 是读取过程中产生的诊断（在锁外由调用方上报）。
	notices []string
}

// encodeMessages 把消息编码成 NDJSON：一行一条 rig.Message，每条以 '\n' 结尾。
//
// 单条序列化后超过 MaxMessageBytes 立即失败，**不写任何字节**（调用方因此可以
// 把"整批超限"当成一次原子失败）。
//
// 同样拒绝读侧会判定为损坏的记录（role 为空）：写侧放行等于"Append 成功、Load
// 读不回来"，那是最难排查的一类数据丢失。
func encodeMessages(messages []rig.Message) ([]byte, error) {
	var buffer bytes.Buffer
	for index, message := range messages {
		if message.Role == "" {
			return nil, fmt.Errorf("%w: 第 %d 条消息缺少 role", ErrInvalidMessage, index)
		}
		line, err := json.Marshal(message)
		if err != nil {
			return nil, fmt.Errorf("session store: encode message %d: %w", index, err)
		}
		if len(line) > MaxMessageBytes {
			return nil, fmt.Errorf("%w: 第 %d 条消息序列化后 %d 字节，上限 %d 字节",
				ErrMessageTooLarge, index, len(line), MaxMessageBytes)
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	return buffer.Bytes(), nil
}

// readRecords 读取一个会话文件。
//
// 容错规则（顺序很重要）：
//  1. 没有换行结尾的尾部半行**丢弃**（视为未完成的写入）；
//  2. 完整行里不是合法 rig.Message 的**跳过**并记一条 Notice；
//  3. 只要至少有一行解析成功，就返回成功（部分损坏不影响已有数据可读）；
//  4. 文件里有完整行但**全部**无法解析时返回 ErrCorrupt；
//  5. 空文件、或只有一条半行时返回空消息 + nil（等同于空会话）。
//
// 文件不存在时返回 os.ErrNotExist（由调用方决定是自愈还是报错）。
func readRecords(path string) (recordRead, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return recordRead{}, err
	}
	read := recordRead{size: int64(len(data))}
	offset := 0
	completeLines := 0
	// recordLines 只统计"非空的完整行"：空行不是记录，因此只有空行的文件等价于
	// 空文件（0 条、无错误），而不是"全部记录损坏"。
	recordLines := 0
	for offset < len(data) {
		newline := bytes.IndexByte(data[offset:], '\n')
		if newline < 0 {
			// 尾部半行：丢弃（并告诉调用方丢了几个字节）。
			read.notices = append(read.notices, fmt.Sprintf(
				"session store: 丢弃 %s 末尾未写完的 %d 字节半行", path, len(data)-offset))
			break
		}
		line := data[offset : offset+newline]
		offset += newline + 1
		completeLines++
		if len(bytes.TrimSpace(line)) == 0 {
			read.notices = append(read.notices, fmt.Sprintf(
				"session store: 跳过 %s 第 %d 条空行", path, completeLines))
			continue
		}
		recordLines++
		var message rig.Message
		// 一条合法记录必须是一个带 role 的 JSON 对象：{} 或 [1,2,3] 都算损坏，
		// 否则回放时会得到一条没有角色的消息（provider 会直接报错）。
		if err := json.Unmarshal(line, &message); err != nil || message.Role == "" {
			reason := "missing role"
			if err != nil {
				reason = err.Error()
			}
			read.notices = append(read.notices, fmt.Sprintf(
				"session store: 跳过 %s 第 %d 条 corrupt 记录: %s", path, completeLines, reason))
			continue
		}
		read.messages = append(read.messages, message)
		read.records++
	}
	if read.records == 0 && recordLines > 0 {
		return read, fmt.Errorf("%w: %s 的 %d 条完整记录全部无法解析", ErrCorrupt, path, recordLines)
	}
	return read, nil
}

// appendRecords 追加一批已编码的记录，并在返回前 Sync。
//
// 追加之前先裁掉文件末尾的半行：崩溃留下的半行如果不裁掉，新记录会被粘在它后面，
// 变成一条真正的坏行（连带丢掉新写入的那条消息）。
//
// 刻意不带 O_CREATE：会话文件由 Create 建立，"文件不见了"是数据丢失而不是"顺手
// 建一个"——重建会让索引里的条数（旧 + 新）与文件内容（只有新）永久不一致。
// 文件不存在时返回包装 os.ErrNotExist 的错误，由调用方选择自愈策略。
func appendRecords(path string, payload []byte) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("session store: open session file %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := trimPartialTail(file); err != nil {
		return fmt.Errorf("session store: trim partial tail of %q: %w", path, err)
	}
	if _, err := file.Write(payload); err != nil {
		return fmt.Errorf("session store: write session file %q: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("session store: sync session file %q: %w", path, err)
	}
	return nil
}

// trimPartialTail 把文件截断到最后一个换行符之后（没有换行则清空）。
func trimPartialTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	const chunkSize = 4096
	buffer := make([]byte, chunkSize)
	for end := size; end > 0; {
		length := int64(len(buffer))
		if end < length {
			length = end
		}
		chunk := buffer[:length]
		if _, readErr := file.ReadAt(chunk, end-length); readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if index := bytes.LastIndexByte(chunk, '\n'); index >= 0 {
			keep := end - length + int64(index) + 1
			if keep == size {
				return nil
			}
			return file.Truncate(keep)
		}
		end -= length
	}
	// 整份文件都没有换行：它就是一条半行，清空。
	return file.Truncate(0)
}

// replaceRecords 原子替换会话文件内容（tmp + Sync + rename + fsync 目录）。
//
// 读者要么看到旧内容、要么看到新内容；rename 之前崩溃只会留下一个临时文件
// （前缀 "."，重建索引时按非会话文件跳过）。
func replaceRecords(path string, payload []byte) error {
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("session store: create temp session file in %q: %w", dir, err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if _, err := temporary.Write(payload); err != nil {
		cleanup()
		return fmt.Errorf("session store: write temp session file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("session store: sync temp session file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("session store: close temp session file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("session store: replace session file %q: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("session store: sync sessions dir: %w", err)
	}
	return nil
}

// fileSize 返回文件字节数。
func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// syncDir 把"目录项变更"刷到盘上（文件创建/删除/rename 之后调用）。
//
// Windows 上对目录句柄调用 Sync 会失败（NTFS 不支持 FlushFileBuffers on
// directory），因此那里是尽力而为；Linux/macOS 上的失败会如实上报。
func syncDir(dir string) error {
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	if err := handle.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	return nil
}

// hashStem 返回 ID 的稳定哈希（用于把不安全的 ID 映射成文件名）。
func hashStem(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:16])
}
