package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
)

// Entry 是插件集合里的一项。
type Entry struct {
	// Instance 是绑定配置后的插件。
	Instance *Instance
	// Enabled 是启用状态，取自配置。
	Enabled bool
	// Order 是执行顺序，越小越先执行。
	Order int
	// Position 是加载顺序，Order 相同时按它排序。
	Position int
}

// Key 返回插件 key。
func (e Entry) Key() string { return e.Instance.Key() }

// Store 是插件集合。可以并发使用。
type Store struct {
	entries []Entry
	byKey   map[string]*Entry
	logger  *slog.Logger
}

// NewStore 创建插件集合。空集合是合法状态：没有任何插件时所有钩子都直通。
func NewStore(entries []Entry, logger *slog.Logger) (*Store, error) {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Order != sorted[j].Order {
			return sorted[i].Order < sorted[j].Order
		}
		return sorted[i].Position < sorted[j].Position
	})
	store := &Store{
		entries: sorted,
		byKey:   make(map[string]*Entry, len(sorted)),
		logger:  logger,
	}
	for i := range store.entries {
		key := store.entries[i].Key()
		if _, exists := store.byKey[key]; exists {
			return nil, fmt.Errorf("plugin: 插件 key %s 重复", key)
		}
		store.byKey[key] = &store.entries[i]
	}
	return store, nil
}

// EmptyStore 返回一个不含插件的集合。
func EmptyStore(logger *slog.Logger) *Store {
	store, err := NewStore(nil, logger)
	if err != nil {
		// NewStore 对空集合不会失败；此处只是让调用方不必处理不可能的错误。
		panic(err)
	}
	return store
}

// Len 返回插件数量。
func (s *Store) Len() int {
	if s == nil {
		return 0
	}
	return len(s.entries)
}

// Entries 按执行顺序返回全部插件。
func (s *Store) Entries() []Entry {
	if s == nil {
		return nil
	}
	return append([]Entry(nil), s.entries...)
}

// Get 按 key 取插件实例。
func (s *Store) Get(key string) (*Instance, bool) {
	if s == nil {
		return nil, false
	}
	entry, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	return entry.Instance, true
}

// Keys 按执行顺序返回全部 key。
func (s *Store) Keys() []string {
	if s == nil {
		return nil
	}
	keys := make([]string, 0, len(s.entries))
	for _, entry := range s.entries {
		keys = append(keys, entry.Key())
	}
	return keys
}

// Missing 返回给了名字但集合里没有的 key，供配置校验报告。
func (s *Store) Missing(keys []string) []string {
	var missing []string
	for _, key := range keys {
		if _, ok := s.Get(key); !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

// Merger 把插件的改写结果合并回负载。
//
// payload 是合并前的负载，rewrite 是插件返回的新值。返回 nil 表示本次不改写，
// 返回错误按插件失败处理。
type Merger func(payload, rewrite []byte) ([]byte, error)

// Apply 按顺序对启用插件调用一个钩子，每个插件看到的是上一个插件改写之后的完整
// 负载，而不是只看到上一个插件的返回值。
//
// keys 为空表示全部启用插件参与；非空时只有列出的 key 参与。
// merger 为 nil 时，插件返回的新值直接成为下一次调用的负载。
//
// 单个插件失败不影响链路：该插件被跳过，前一个成功的改写结果继续传给后面的插件。
func (s *Store) Apply(ctx context.Context, hook Hook, input []byte, merger Merger, keys []string, session Session) Outcome {
	outcome := Outcome{Data: input}
	if s == nil || len(s.entries) == 0 {
		return outcome
	}
	var filter map[string]bool
	if len(keys) > 0 {
		filter = make(map[string]bool, len(keys))
		for _, key := range keys {
			filter[key] = true
		}
	}
	for _, entry := range s.entries {
		if !entry.Enabled || !entry.Instance.Has(hook) {
			continue
		}
		if filter != nil && !filter[entry.Key()] {
			continue
		}
		output, changed, err := entry.Instance.Call(ctx, hook, outcome.Data, session)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return outcome
			}
			outcome.Failures = append(outcome.Failures, Failure{Key: entry.Key(), Hook: hook, Err: err})
			s.log(entry.Key(), hook, err)
			continue
		}
		if !changed {
			continue
		}
		merged := output
		if merger != nil {
			merged, err = merger(outcome.Data, output)
			if err != nil {
				outcome.Failures = append(outcome.Failures, Failure{Key: entry.Key(), Hook: hook, Err: err})
				s.log(entry.Key(), hook, err)
				continue
			}
			if merged == nil {
				continue
			}
		}
		outcome.Data = merged
		outcome.Applied = append(outcome.Applied, entry.Key())
	}
	return outcome
}

// log 记录一次插件失败。插件失败不改变转发结果，日志是唯一的线索。
func (s *Store) log(key string, hook Hook, err error) {
	if s.logger == nil {
		return
	}
	s.logger.Warn("插件执行失败，已跳过", "plugin", key, "hook", string(hook), "error", err.Error())
}

// HasAny 报告是否有启用插件导出了该钩子。没有插件参与时，调用方可以走不解析
// 负载的快速路径。
func (s *Store) HasAny(hook Hook) bool {
	if s == nil {
		return false
	}
	for _, entry := range s.entries {
		if entry.Enabled && entry.Instance.Has(hook) {
			return true
		}
	}
	return false
}
