package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureDir 确保给定文件路径上的所有父目录都存在。
// 如果 path 为空字符串会返回错误。
func EnsureDir(path string) error {
	if path == "" {
		return errors.New("store: empty path")
	}
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	return nil
}

// WriteAtomic 以原子方式（先写临时文件，再 rename）写入 data 到 path。
//
// 写入顺序：
//   1. 创建 path.tmp
//   2. Write + Sync
//   3. Close
//   4. Rename tmp -> target
//
// 这样在进程崩溃时不会出现半写文件，要么保持旧文件，要么得到完整的新文件。
func WriteAtomic(path string, data []byte) error {
	if err := EnsureDir(path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: create tmp file %s: %w", tmp, err)
	}
	defer func() {
		_ = f.Close()
		// 如果 rename 失败，尝试清理临时文件（非致命）。
		if _, e := os.Stat(tmp); e == nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("store: write tmp file %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: fsync tmp file %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close tmp file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// OpenAppend 打开一个用于追加写入的文件，不存在则创建。
// 使用 O_WRONLY|O_APPEND|O_CREATE：只写、每次 Write 原子地追加到文件末尾，
// 避免并发写者互相覆盖彼此的内容。
func OpenAppend(path string) (*os.File, error) {
	if err := EnsureDir(path); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}

// FileExists 返回给定路径是否存在常规文件。
func FileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// FileSize 返回常规文件的字节大小。
func FileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("store: stat %s: %w", path, err)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("store: %s is a directory", path)
	}
	return info.Size(), nil
}
