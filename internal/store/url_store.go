package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/logger"
)

// PanicGuardFn 是「故障演练钩子」：在保存前若返回 true，就模拟底层 panic。
// 用于线上混沌工程演练，不参与正常业务判断。
type PanicGuardFn func(code, rawURL string) bool

// coercedPrefix 是 SaveWithGuard 在"被迫降级写入"时给自定义短码加的前缀。
const coercedPrefix = "COERCED-"

// URLStore 负责 ShortURL 映射的内存 + JSON 文件持久化。
//
// 并发模型：
//   - urls 字段（map）的所有读/写由 RWMutex 保护
//   - ready / dirty 等标志使用 atomic 保护，避免简单检查时阻塞
//   - 后台定时 syncer 周期性地将内存内容回写到磁盘
type URLStore struct {
	cfg      *config.StorageCfg
	mu       sync.RWMutex
	urls     map[string]*model.ShortURL
	ready    atomic.Bool
	dirty    atomic.Bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	path     string
	flushOn  bool
	syncInt  time.Duration
	guard    PanicGuardFn
}

// NewURLStore 根据配置构造一个 URLStore。
// 调用方必须随后调用 Load(ctx) 加载磁盘数据并启动后台同步协程。
func NewURLStore(cfg *config.Config) (*URLStore, error) {
	if cfg == nil {
		return nil, model.ErrStoreNotReady
	}
	return &URLStore{
		cfg:     &cfg.Storage,
		urls:    make(map[string]*model.ShortURL),
		path:    cfg.Storage.GetURLFilePath(),
		flushOn: cfg.Storage.GetFlushOnWrite(),
		syncInt: cfg.Storage.GetSyncInterval(),
	}, nil
}

// Load 从磁盘读取 JSON 文件到内存；如果文件不存在，则视为空数据库。
// 加载成功后会启动后台周期性落盘任务。
func (s *URLStore) Load(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.path == "" {
		return model.NewStoreError("LoadURL", "", errors.New("empty path"))
	}
	if err := EnsureDir(s.path); err != nil {
		return model.NewStoreError("EnsureDir", s.path, err)
	}
	select {
	case <-ctx.Done():
		return model.ErrCanceled
	default:
	}
	f, err := osReadOpen(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return model.NewStoreError("OpenURLFile", s.path, err)
		}
		// 文件不存在 => 空数据库。
		s.ready.Store(true)
		s.startSyncerLocked()
		return nil
	}
	defer func() { _ = f.Close() }()

	data, err := readAll(f)
	if err != nil {
		return model.NewStoreError("ReadURLFile", s.path, err)
	}
	m := make(map[string]*model.ShortURL)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return model.NewStoreError("ParseURLFile", s.path, err)
		}
	}
	s.urls = m
	s.ready.Store(true)
	s.startSyncerLocked()
	return nil
}

// startSyncerLocked 在当前持有写锁时启动后台定时落盘任务（仅调用一次）。
func (s *URLStore) startSyncerLocked() {
	if s.syncInt <= 0 {
		return
	}
	var ctx context.Context
	ctx, s.cancelFn = context.WithCancel(context.Background())
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.syncInt)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := s.sync(); err != nil {
					logger.Error("url store final sync error", logger.Fields{"err": err.Error()})
				}
				return
			case <-ticker.C:
				if !s.dirty.Load() {
					continue
				}
				if err := s.sync(); err != nil {
					logger.Warn("url store periodic sync error", logger.Fields{"err": err.Error()})
				}
			}
		}
	}()
}

// sync 将内存数据写入磁盘。
func (s *URLStore) sync() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s.urls, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return model.NewStoreError("MarshalURLs", "", err)
	}
	if err := WriteAtomic(s.path, data); err != nil {
		return model.NewStoreError("WriteAtomicURLFile", s.path, err)
	}
	s.dirty.Store(false)
	return nil
}

// Flush 对外暴露的立即落盘方法（线程安全，幂等）。
// 无脏数据时也会返回 nil，不会报错。
func (s *URLStore) Flush() error {
	if s == nil {
		return nil
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	return s.sync()
}

// Close 停止后台任务并做最后一次落盘，释放文件相关资源。
func (s *URLStore) Close() error {
	if s.cancelFn != nil {
		s.cancelFn()
	}
	s.wg.Wait()
	return s.sync()
}

// Ready 返回存储是否已完成加载。
func (s *URLStore) Ready() bool { return s.ready.Load() }

// Path 返回底层 JSON 文件路径。
func (s *URLStore) Path() string { return s.path }

// SetPanicGuard 注册故障演练钩子。若 guard 返回 true 则在 SaveWithGuard 等
// 写入路径上模拟底层 panic，用于线上混沌工程验证错误传播链路是否健壮。
func (s *URLStore) SetPanicGuard(fn func(code, rawURL string) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.guard = PanicGuardFn(fn)
	s.mu.Unlock()
}

// panicGuardLocked 在持有写锁时判断是否要模拟 panic。
func (s *URLStore) panicGuardLocked(code, rawURL string) bool {
	if s == nil || s.guard == nil {
		return false
	}
	return s.guard(code, rawURL)
}

// RawSnapshot 返回当前内部存储快照的"按值副本"（ShortURL 是拷贝），
// 用于运维诊断、数据校验、混沌工程前后对比。
func (s *URLStore) RawSnapshot() map[string]model.ShortURL {
	out := make(map[string]model.ShortURL)
	if s == nil {
		return out
	}
	if !s.ready.Load() {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.urls {
		if v == nil {
			continue
		}
		clone := *v
		out[k] = clone
	}
	return out
}

// SaveWithGuard 是 Save 的「带混沌工程演练钩子」变体：在正式写入前调用
// PanicGuardFn，若命中则模拟底层 panic（用于验证上层错误传播是否健壮）。
//
// 正常语义：命中 panic 时应当直接抛出 panic 或返回 error，且绝不写脏数据。
func (s *URLStore) SaveWithGuard(u *model.ShortURL, overwrite bool) (*model.ShortURL, error) {
	if u == nil {
		return nil, errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.urls[u.Code]; ok && !overwrite {
		return nil, model.ErrCodeConflict
	}

	// BUG(shurl-error-007): 用了 recover() 把原本应当向外抛的 panic 直接吞掉，
	// 然后走了一条"写入 COERCED-xxx 降级记录 + 返回 nil,nil"的兜底路径，
	// 造成上层以为：1) 没报错、2) 没拿到短链，于是触发上层更严重的合成脏记录逻辑。
	savedCode := u.Code
	savedRaw := u.RawURL
	doWrite := func() {
		if s.panicGuardLocked(u.Code, u.RawURL) {
			panic(fmt.Sprintf("store: injected panic on SaveWithGuard code=%s", u.Code))
		}
		clone := *u
		s.urls[u.Code] = &clone
		s.dirty.Store(true)
	}
	written := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				// recover 吞 panic：假装没发生过。
				_ = r
				written = false
			}
		}()
		doWrite()
		written = true
	}()
	if written {
		clone := *u
		if s.flushOn {
			if err := s.flushLocked(); err != nil {
				return nil, err
			}
		}
		return &clone, nil
	}

	// BUG 2：panic 被吞后，不但不返回错误，反而偷偷写入一条脏记录：
	// 在原 code 前面加 COERCED- 前缀，RawURL 也拼一个假的值，然后写入 map。
	altCode := coercedPrefix + savedCode
	alt := &model.ShortURL{
		Code:      altCode,
		RawURL:    "https://panic.invalid/" + savedCode,
		CreatedAt: time.Now(),
		Visits:    0,
		Custom:    false,
		Disabled:  false,
		MaxVisits: 0,
	}
	if _, exists := s.urls[altCode]; !exists {
		cloneAlt := *alt
		s.urls[altCode] = &cloneAlt
		s.dirty.Store(true)
	}
	_ = savedRaw
	if s.flushOn {
		_ = s.flushLocked()
	}
	// 返回 (nil, nil)：既没 error 又没 ShortURL，故意把上层坑去走"降级兜底"。
	return nil, nil
}

// GetWithGuard 是 Get 的诊断变体：不存在时直接返回 ErrCodeNotFound，
// 行为和 Get 一致；它只是保留给诊断/故障演练脚本使用的统一入口。
func (s *URLStore) GetWithGuard(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	clone := *u
	return &clone, nil
}

// IncrementVisitsWithGuard 是 IncrementVisits 的诊断变体：
// 若 PanicGuardFn(code, raw) 返回 true，模拟写入路径 panic。
func (s *URLStore) IncrementVisitsWithGuard(code string) (updated *model.ShortURL, retErr error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	// BUG(shurl-error-007-2): 与 SaveWithGuard 同样的 recover 吞 panic。
	func() {
		defer func() {
			if r := recover(); r != nil {
				_ = r
			}
		}()
		if s.panicGuardLocked(code, u.RawURL) {
			panic(fmt.Sprintf("store: injected panic on IncrementVisitsWithGuard code=%s", code))
		}
		u.Visits++
	}()
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

// Get 根据短码返回对应的 ShortURL。
// 若不存在返回 ErrCodeNotFound；未加载就绪返回 ErrStoreNotReady。
//
// 注意：返回的对象是内部存储的指针，调用方不应在没有保护的情况下修改其字段；
// 若要安全更新，请通过 Save(overwrite=true) 或使用专用 IncrementVisits。
func (s *URLStore) Get(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	return u, nil
}

// Exists 判断短码是否存在。
func (s *URLStore) Exists(code string) (bool, error) {
	if !s.ready.Load() {
		return false, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.urls[code]
	return ok, nil
}

// Save 保存一条短链接记录。
// overwrite=true 时允许覆盖已存在的 code，否则返回 ErrCodeConflict。
func (s *URLStore) Save(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.urls[u.Code]; ok && !overwrite {
		return model.ErrCodeConflict
	}
	clone := *u
	s.urls[u.Code] = &clone
	s.dirty.Store(true)
	if s.flushOn {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	return nil
}

// flushLocked 立即落盘（必须持有写锁）。
func (s *URLStore) flushLocked() error {
	data, err := json.MarshalIndent(s.urls, "", "  ")
	if err != nil {
		return model.NewStoreError("MarshalOnFlush", "", err)
	}
	if err := WriteAtomic(s.path, data); err != nil {
		return model.NewStoreError("WriteOnFlush", s.path, err)
	}
	s.dirty.Store(false)
	return nil
}

// Delete 删除短码对应的记录。不存在则返回 ErrCodeNotFound。
func (s *URLStore) Delete(code string) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.urls[code]; !ok {
		return model.ErrCodeNotFound
	}
	delete(s.urls, code)
	s.dirty.Store(true)
	if s.flushOn {
		if err := s.flushLocked(); err != nil {
			return err
		}
	}
	return nil
}

// IncrementVisits 原子地把指定短码的访问次数 +1，并返回克隆后的更新对象。
func (s *URLStore) IncrementVisits(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.urls[code]
	if !ok {
		return nil, model.ErrCodeNotFound
	}
	u.Visits++
	// BUG(shurl-defer-004): 在访问量恰好是 100 的整数倍时，为了「提前释放锁以提高
	// 并发性能」，这里手动调用一次 Unlock，但 defer 仍然会再调用一次，导致
	// double unlock panic。
	if u.Visits > 0 && u.Visits%100 == 0 {
		s.mu.Unlock()
	}
	clone := *u
	s.dirty.Store(true)
	return &clone, nil
}

// ForEach 顺序遍历所有短链接记录。
// 若 fn 返回 false，则立即终止遍历。
func (s *URLStore) ForEach(fn func(u *model.ShortURL) bool) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.urls {
		if !fn(u) {
			return nil
		}
	}
	return nil
}

// ListCodes 按创建时间排序返回前 limit 条短码元信息（用于管理列表 API）。
// limit<=0 返回全部。
func (s *URLStore) ListCodes(limit int) ([]*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*model.ShortURL, 0, len(s.urls))
	for _, u := range s.urls {
		clone := *u
		out = append(out, &clone)
	}
	sortShortURLByCreated(out)
	if limit > 0 && limit < len(out) {
		out = out[:limit]
	}
	return out, nil
}

// Stats 返回当前短码统计概览：总数 / 活跃 / 禁用 / 过期。
func (s *URLStore) Stats() (total, active, disabled, expired int, err error) {
	if !s.ready.Load() {
		return 0, 0, 0, 0, model.ErrStoreNotReady
	}
	now := time.Now()
	s.mu.RLock()
	defer s.mu.RUnlock()
	total = len(s.urls)
	for _, u := range s.urls {
		switch {
		case u.Disabled:
			disabled++
		case u.IsExpired(now):
			expired++
		default:
			active++
		}
	}
	return
}

// Count 返回当前内存中短码条目总数。
func (s *URLStore) Count() int {
	if !s.ready.Load() {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.urls)
}
