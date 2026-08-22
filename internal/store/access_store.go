package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/logger"
)

// AccessLogStore 负责访问日志的按行追加（NDJSON）与读取。
//
// 写路径：Append / AppendMany 受互斥锁保护。
// 读路径：Scan 每次重新打开只读句柄避免与写入的 offset 冲突。
type AccessLogStore struct {
	cfg     *config.StorageCfg
	mu      sync.Mutex
	file    *os.File
	path    string
	ready   atomic.Bool
	wg      sync.WaitGroup
	cancel  context.CancelFunc
	syncInt time.Duration
}

// NewAccessLogStore 构造 AccessLogStore。
func NewAccessLogStore(cfg *config.Config) (*AccessLogStore, error) {
	if cfg == nil {
		return nil, model.ErrStoreNotReady
	}
	return &AccessLogStore{
		cfg:     &cfg.Storage,
		path:    cfg.Storage.LogFilePath,
		syncInt: cfg.Storage.SyncInterval,
	}, nil
}

// Open 打开或创建日志文件，并启动后台 fsync 任务。
func (a *AccessLogStore) Open(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path == "" {
		return model.NewStoreError("OpenLog", "", errors.New("empty path"))
	}
	if err := EnsureDir(a.path); err != nil {
		return model.NewStoreError("EnsureDir", a.path, err)
	}
	f, err := OpenAppend(a.path)
	if err != nil {
		return model.NewStoreError("OpenLogFile", a.path, err)
	}
	a.file = f
	a.ready.Store(true)

	if a.syncInt > 0 {
		// 后台定时 fsync 与调用方（HTTP 请求 / 信号）的 ctx 生命周期无关：
		// 用 Background 派生，仅由 Close() 通过 cancel 终止，避免请求
		// 结束 ctx 被取消后定时落盘 goroutine 立刻退出，导致后续 Append
		// 不能真正落盘。
		var inner context.Context
		inner, a.cancel = context.WithCancel(context.Background())
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			t := time.NewTicker(a.syncInt)
			defer t.Stop()
			for {
				select {
				case <-inner.Done():
					a.mu.Lock()
					if a.file != nil {
						_ = a.file.Sync()
					}
					a.mu.Unlock()
					return
				case <-t.C:
					a.mu.Lock()
					if a.file != nil {
						_ = a.file.Sync()
					}
					a.mu.Unlock()
				}
			}
		}()
	}
	return nil
}

// Close 停止后台任务并关闭日志文件。
func (a *AccessLogStore) Close() error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	var err error
	if a.file != nil {
		if err = a.file.Sync(); err != nil {
			logger.Warn("access log sync on close error", logger.Fields{"err": err.Error()})
		}
		err = a.file.Close()
		a.file = nil
	}
	a.ready.Store(false)
	return err
}

// Ready 返回存储是否就绪。
func (a *AccessLogStore) Ready() bool { return a.ready.Load() }

// Path 返回日志文件路径。
func (a *AccessLogStore) Path() string { return a.path }

// Sync 立即把日志文件的用户态缓冲刷到磁盘内核（fdatasync 语义）。
// 当 file 未打开或 store 未 ready 时视为空操作（返回 nil，保持幂等）。
func (a *AccessLogStore) Sync() error {
	if a == nil {
		return nil
	}
	if !a.ready.Load() {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	return a.file.Sync()
}

// Append 追加单条访问日志。
func (a *AccessLogStore) Append(log *model.AccessLog) error {
	if log == nil {
		return errors.New("store: nil access log")
	}
	if !a.ready.Load() {
		return model.ErrStoreNotReady
	}
	if err := log.Validate(); err != nil {
		// 拒绝写入不合法的日志，保证落盘文件干净合法。
		return model.NewStoreError("ValidateLog", log.Code, err)
	}
	data, err := json.Marshal(log)
	if err != nil {
		return model.NewStoreError("MarshalLog", log.Code, err)
	}
	line := append(data, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return model.ErrStoreNotReady
	}
	if _, err := a.file.Write(line); err != nil {
		return model.NewStoreError("WriteLog", log.Code, err)
	}
	if a.cfg != nil && a.cfg.FlushOnWrite {
		_ = a.file.Sync()
	}
	return nil
}

// AppendMany 批量追加访问日志（只加一次锁，减少加锁开销）。
// 单条日志为 nil 或校验失败时跳过该条并记录告警，不丢弃整批，
// 避免坏数据导致大批正常日志一起丢失。
func (a *AccessLogStore) AppendMany(logs []*model.AccessLog) error {
	if len(logs) == 0 {
		return nil
	}
	if !a.ready.Load() {
		return model.ErrStoreNotReady
	}
	buf := make([]byte, 0, 512*len(logs))
	for _, l := range logs {
		if l == nil {
			continue
		}
		if err := l.Validate(); err != nil {
			logger.Warn("skip invalid access log", logger.Fields{"code": l.Code, "err": err.Error()})
			continue
		}
		data, err := json.Marshal(l)
		if err != nil {
			logger.Warn("skip unmarshalable access log", logger.Fields{"code": l.Code, "err": err.Error()})
			continue
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return model.ErrStoreNotReady
	}
	if _, err := a.file.Write(buf); err != nil {
		return model.NewStoreError("WriteLogs", "", err)
	}
	if a.cfg != nil && a.cfg.FlushOnWrite {
		_ = a.file.Sync()
	}
	return nil
}

// BatchAppend 接收外部传入的指向 batch slice 的指针，将其中所有日志写入磁盘，
// 然后直接把该外部 slice 重置为空（复用底层数组）。
// 单条日志为 nil 或校验失败时跳过该条，不丢弃整批。
// 注意：调用方持有该 slice 的 goroutine 可能与其他调用方共享同一个底层数组，
// 因此调用方需自行保证传入期间不会有并发对该 slice 的追加或重置。
func (a *AccessLogStore) BatchAppend(batch *[]*model.AccessLog) error {
	if batch == nil || len(*batch) == 0 {
		return nil
	}
	if !a.ready.Load() {
		return model.ErrStoreNotReady
	}
	logs := *batch
	buf := make([]byte, 0, 512*len(logs))
	for _, l := range logs {
		if l == nil {
			continue
		}
		if err := l.Validate(); err != nil {
			logger.Warn("skip invalid access log", logger.Fields{"code": l.Code, "err": err.Error()})
			continue
		}
		data, err := json.Marshal(l)
		if err != nil {
			logger.Warn("skip unmarshalable access log", logger.Fields{"code": l.Code, "err": err.Error()})
			continue
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return model.ErrStoreNotReady
	}
	if _, err := a.file.Write(buf); err != nil {
		return model.NewStoreError("WriteLogs", "", err)
	}
	if a.cfg != nil && a.cfg.FlushOnWrite {
		_ = a.file.Sync()
	}
	*batch = (*batch)[:0]
	return nil
}

// Scan 从头到尾扫描日志文件，对每条记录调用 fn；fn 返回 false 则提前终止。
// maxRecords 指定最多扫描的条数，<=0 表示不限制。
// 返回已扫描的实际条数。
func (a *AccessLogStore) Scan(fn func(l *model.AccessLog) bool, maxRecords int) (int, error) {
	if !a.ready.Load() {
		return 0, model.ErrStoreNotReady
	}
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	f, err := osReadOpen(path)
	if err != nil {
		return 0, model.NewStoreError("ScanLogOpen", path, err)
	}
	defer func() { _ = f.Close() }()

	dec := json.NewDecoder(f)
	count := 0
	for dec.More() {
		if maxRecords > 0 && count >= maxRecords {
			return count, nil
		}
		var l model.AccessLog
		if err := dec.Decode(&l); err != nil {
			logger.Warn("access log decode error, skip line", logger.Fields{"err": err.Error()})
			continue
		}
		count++
		if !fn(&l) {
			return count, nil
		}
	}
	return count, nil
}

// CountLines 返回日志当前的总条数。
// 注意：当数据量非常大时这个函数成本较高，建议后台调用。
func (a *AccessLogStore) CountLines() (int64, error) {
	if !a.ready.Load() {
		return 0, model.ErrStoreNotReady
	}
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	f, err := osReadOpen(path)
	if err != nil {
		return 0, model.NewStoreError("CountLogLines", path, err)
	}
	defer func() { _ = f.Close() }()
	var n int64
	dec := json.NewDecoder(f)
	for dec.More() {
		var l model.AccessLog
		if err := dec.Decode(&l); err != nil {
			// 跳过坏行，不计数。
			continue
		}
		n++
	}
	return n, nil
}

// SizeBytes 返回日志文件字节大小（供管理/监控接口使用）。
func (a *AccessLogStore) SizeBytes() (int64, error) {
	a.mu.Lock()
	path := a.path
	a.mu.Unlock()
	return FileSize(path)
}

// --- 包内私有辅助函数，从 store.go 挪到此处以保持文件解耦 ---

func osReadOpen(path string) (*os.File, error) { return os.Open(path) }

func readAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
