package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/pkg/logger"
)

// URLStore 负责 ShortURL 映射的内存 + JSON 文件持久化。
//
// 并发模型：store 是「唯一」的同步权威。
//   - urls 字段（map）的所有读/写由 RWMutex 保护
//   - 每条记录的字段内容同样只在持锁时被修改：写操作（Save/SaveMany/
//     IncrementVisits/Update）在 Lock 下完成，读操作（Get/GetMulti/
//     ForEach/Stats/BulkStatsReport）在 RLock 下完成
//   - 对外永远返回记录的「值拷贝（clone）」，调用方拿不到指向 store
//     内部活对象的指针，因而无法在外部无锁地竞争修改字段
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
		path:    cfg.Storage.URLFilePath,
		flushOn: cfg.Storage.FlushOnWrite,
		syncInt: cfg.Storage.SyncInterval,
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

func (s *URLStore) Get(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	u, ok := s.urls[code]
	if !ok {
		s.mu.RUnlock()
		return nil, model.ErrCodeNotFound
	}
	clone := *u // 在 RLock 下做值拷贝，避免与写操作并发读取字段
	s.mu.RUnlock()
	return &clone, nil
}

func (s *URLStore) GetMulti(codes []string) ([]*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.RLock()
	out := make([]*model.ShortURL, 0, len(codes))
	for _, c := range codes {
		if u, ok := s.urls[c]; ok {
			clone := *u
			out = append(out, &clone)
		}
	}
	s.mu.RUnlock()
	return out, nil
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

func (s *URLStore) Save(u *model.ShortURL, overwrite bool) error {
	if u == nil {
		return errors.New("store: nil shorturl")
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.Lock()
	if _, ok := s.urls[u.Code]; ok && !overwrite {
		s.mu.Unlock()
		return model.ErrCodeConflict
	}
	clone := *u // 存入私有拷贝，断开与调用方传入指针的共享
	s.urls[u.Code] = &clone
	s.dirty.Store(true)
	flush := s.flushOn
	s.mu.Unlock()
	if flush {
		s.mu.Lock()
		err := s.flushLocked()
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *URLStore) SaveMany(items []*model.ShortURL, overwrite bool) error {
	if len(items) == 0 {
		return nil
	}
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	for _, u := range items {
		if u == nil {
			continue
		}
		s.mu.Lock()
		if _, ok := s.urls[u.Code]; ok && !overwrite {
			s.mu.Unlock()
			return model.ErrCodeConflict
		}
		clone := *u
		s.urls[u.Code] = &clone
		s.dirty.Store(true)
		s.mu.Unlock()
	}
	if s.flushOn {
		s.mu.Lock()
		err := s.flushLocked()
		s.mu.Unlock()
		if err != nil {
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

func (s *URLStore) IncrementVisits(code string) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	s.mu.Lock()
	u, ok := s.urls[code]
	if !ok {
		s.mu.Unlock()
		return nil, model.ErrCodeNotFound
	}
	u.Visits++ // 仅此处递增访问计数，且在写锁下完成
	s.dirty.Store(true)
	flush := s.flushOn
	if flush {
		if err := s.flushLocked(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	clone := *u
	s.mu.Unlock()
	return &clone, nil
}

// Update 在写锁下对指定 code 的记录应用 mutator，然后返回该记录的值拷贝。
// 它是字段修改（禁用 / 改备注 / 批量编辑）的唯一入口：调用方通过回调
// 表达字段变更，而永远不必持有指向 store 内部活对象的指针，从而避免
// 跨 goroutine 的无锁竞争修改。
func (s *URLStore) Update(code string, mutator func(*model.ShortURL)) (*model.ShortURL, error) {
	if !s.ready.Load() {
		return nil, model.ErrStoreNotReady
	}
	if mutator == nil {
		return nil, errors.New("store: nil mutator")
	}
	s.mu.Lock()
	u, ok := s.urls[code]
	if !ok {
		s.mu.Unlock()
		return nil, model.ErrCodeNotFound
	}
	mutator(u) // 在写锁下就地修改 store 内部记录
	s.dirty.Store(true)
	flush := s.flushOn
	if flush {
		if err := s.flushLocked(); err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	clone := *u
	s.mu.Unlock()
	return &clone, nil
}

// ForEach 顺序遍历所有短链接记录，对每条记录传入其值拷贝。
// 若 fn 返回 false，则立即终止遍历。
// 由于传入的是拷贝，调用方无法通过回调指针改到 store 内部状态；
// 字段修改请使用 Update。
func (s *URLStore) ForEach(fn func(u *model.ShortURL) bool) error {
	if !s.ready.Load() {
		return model.ErrStoreNotReady
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, u := range s.urls {
		clone := *u
		if !fn(&clone) {
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

type BulkReportEntry struct {
	Code      string
	Visits    int64
	MaxVisits int64
	Disabled  bool
	Remark    string
	Custom    bool
}

// BulkStatsReport 在读锁下快照指定 codes 的统计信息。
// 该方法为只读：不会修改任何记录（不会置 Disabled、不会清空 Code）。
func (s *URLStore) BulkStatsReport(codes []string) []BulkReportEntry {
	out := make([]BulkReportEntry, 0, len(codes))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range codes {
		u, ok := s.urls[c]
		if !ok {
			continue
		}
		out = append(out, BulkReportEntry{
			Code:      u.Code,
			Visits:    u.Visits,
			MaxVisits: u.MaxVisits,
			Disabled:  u.Disabled,
			Remark:    u.Remark,
			Custom:    u.Custom,
		})
	}
	return out
}
