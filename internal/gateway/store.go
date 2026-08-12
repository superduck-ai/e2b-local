package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type SandboxStore struct {
	mu        sync.RWMutex
	sandboxes map[string]*sandboxEntry
}

// sandboxEntry has a stable identity for the lifetime of a stored sandbox.
// lifecycleMu serializes runtime and state transitions for that sandbox.
type sandboxEntry struct {
	lifecycleMu sync.Mutex
	record      SandboxRecord
}

func NewSandboxStore() *SandboxStore {
	return &SandboxStore{
		sandboxes: make(map[string]*sandboxEntry),
	}
}

// lockSandbox 锁住指定 id 对应的沙箱条目，并确保返回的条目是 Store 中当前有效的条目。
//
// 返回值 (*sandboxEntry, bool) 中，bool 表示是否成功找到并锁住了有效条目。如果该 id
// 在 Store 中不存在，或者加锁期间原条目被删除/替换，则返回 nil, false。
//
// 实现要点：
//   1. 先用 Store 的读锁查出 id 对应的 entry。
//   2. 释放 Store 读锁后，对该 entry 的 lifecycleMu 加锁（避免长时间阻塞其他沙箱）。
//   3. 再次用 Store 读锁确认该 entry 仍是当前 id 指向的同一个对象，防止 ABA 问题：
//      等待 lifecycleMu 期间，旧 entry 可能被删除，新 entry 可能复用同一 id 创建。
//   4. 如果确认失败，解锁 lifecycleMu 并从头循环；成功则返回 entry, true。
//
// 调用方获得 entry 后必须尽快完成操作并调用 entry.lifecycleMu.Unlock()，否则同沙箱的
// 其他生命周期操作会被阻塞。
func (s *SandboxStore) lockSandbox(id string) (*sandboxEntry, bool) {
	for {
		// 第一步：在 Store 读锁保护下定位条目。
		s.mu.RLock()
		entry, ok := s.sandboxes[id]
		s.mu.RUnlock()
		if !ok {
			// 条目根本不存在，无需加锁。
			return nil, false
		}

		// 第二步：对单个沙箱的生命周期锁加锁。这里不持有 Store 锁，避免阻塞全局 map。
		entry.lifecycleMu.Lock()

		// 第三步：加锁完成后重新校验该条目是否仍是当前有效的条目。
		// 等待 lifecycleMu 期间，另一个 goroutine 可能已经把该 entry 删除，
		// 甚至创建了一个同 id 的新 entry，因此必须按指针比较。
		s.mu.RLock()
		current, exists := s.sandboxes[id]
		isCurrent := exists && current == entry
		s.mu.RUnlock()
		if isCurrent {
			return entry, true
		}

		// 校验失败说明拿到了一个已经失效的条目，解锁后重试。
		entry.lifecycleMu.Unlock()
	}
}

func (s *SandboxStore) Create(record SandboxRecord) (SandboxRecord, error) {
	if record.ID == "" {
		id, err := newSandboxID()
		if err != nil {
			return SandboxRecord{}, err
		}
		record.ID = id
	}

	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	if record.EndAt.IsZero() {
		record.EndAt = defaultSandboxEndAt(record.CreatedAt)
	}

	if record.State == "" {
		record.State = "running"
	}

	record = record.clone()
	if record.EnvdURL == "" {
		record.EnvdURL = record.RuntimeInfo.EnvdURL
	}

	s.mu.Lock()

	if _, ok := s.sandboxes[record.ID]; ok {
		s.mu.Unlock()
		return SandboxRecord{}, fmt.Errorf("sandbox %s already exists", record.ID)
	}

	s.sandboxes[record.ID] = &sandboxEntry{record: record}
	s.mu.Unlock()
	return record.clone(), nil
}

func (s *SandboxStore) Upsert(record SandboxRecord) (SandboxRecord, error) {
	if strings.TrimSpace(record.ID) == "" {
		return SandboxRecord{}, fmt.Errorf("sandbox id is required")
	}

	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	if record.EndAt.IsZero() {
		record.EndAt = defaultSandboxEndAt(record.CreatedAt)
	}

	if record.State == "" {
		record.State = "running"
	}

	record = record.clone()
	if record.EnvdURL == "" {
		record.EnvdURL = record.RuntimeInfo.EnvdURL
	}

	s.mu.Lock()

	entry, exists := s.sandboxes[record.ID]
	if exists {
		record = mergeSandboxRecordForCache(entry.record, record)
		entry.record = record
	} else {
		entry = &sandboxEntry{record: record}
		s.sandboxes[record.ID] = entry
	}
	s.mu.Unlock()
	return record.clone(), nil
}

func (s *SandboxStore) List() []SandboxRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := make([]SandboxRecord, 0, len(s.sandboxes))
	for _, entry := range s.sandboxes {
		records = append(records, entry.record.clone())
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})

	return records
}

func (s *SandboxStore) Get(id string) (SandboxRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	entry, ok := s.sandboxes[id]
	if !ok {
		return SandboxRecord{}, false
	}

	return entry.record.clone(), true
}

func (s *SandboxStore) Delete(id string) (bool, error) {
	s.mu.Lock()

	_, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		return false, nil
	}

	delete(s.sandboxes, id)
	s.mu.Unlock()
	return true, nil
}

func (s *SandboxStore) SetState(id, state string) (SandboxRecord, bool, error) {
	s.mu.Lock()

	entry, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		return SandboxRecord{}, false, nil
	}

	record := entry.record
	record.State = state
	entry.record = record
	s.mu.Unlock()
	return record.clone(), true, nil
}

func (s *SandboxStore) SetStateAndRuntimeInfo(id, state string, info SandboxRuntimeInfo) (SandboxRecord, bool, error) {
	return s.SetStateRuntimeInfoAndEndAt(id, state, info, time.Time{})
}

func (s *SandboxStore) SetEndAt(id string, endAt time.Time) (SandboxRecord, bool, error) {
	s.mu.Lock()

	entry, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		return SandboxRecord{}, false, nil
	}

	record := entry.record
	record.EndAt = endAt
	entry.record = record
	s.mu.Unlock()
	return record.clone(), true, nil
}

func (s *SandboxStore) ExtendEndAt(id string, endAt time.Time) (SandboxRecord, bool, error) {
	s.mu.Lock()

	entry, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		return SandboxRecord{}, false, nil
	}

	record := entry.record
	if record.EndAt.After(endAt) {
		endAt = record.EndAt
	}
	record.EndAt = endAt
	entry.record = record
	s.mu.Unlock()
	return record.clone(), true, nil
}

func (s *SandboxStore) SetStateRuntimeInfoAndEndAt(id, state string, info SandboxRuntimeInfo, endAt time.Time) (SandboxRecord, bool, error) {
	s.mu.Lock()

	entry, ok := s.sandboxes[id]
	if !ok {
		s.mu.Unlock()
		return SandboxRecord{}, false, nil
	}

	record := entry.record
	record.State = state
	record.RuntimeInfo = info
	record.EnvdURL = info.EnvdURL
	if !endAt.IsZero() {
		record.EndAt = endAt
	}
	entry.record = record
	s.mu.Unlock()
	return record.clone(), true, nil
}

func newSandboxID() (string, error) {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("generate sandbox id: %w", err)
	}

	return "sbx" + hex.EncodeToString(bytes[:]), nil
}

func copyStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	return maps.Clone(values)
}

func CopyStringMap(values map[string]string) map[string]string {
	return copyStringMap(values)
}

func (record SandboxRecord) clone() SandboxRecord {
	record.Metadata = maps.Clone(record.Metadata)
	record.RuntimeInfo.VolumeMounts = slices.Clone(record.RuntimeInfo.VolumeMounts)
	record.RuntimeInfo.PublishedPorts = slices.Clone(record.RuntimeInfo.PublishedPorts)
	return record
}

func mergeSandboxRecordForCache(cached SandboxRecord, incoming SandboxRecord) SandboxRecord {
	record := incoming.clone()
	cached = cached.clone()

	if record.ID == "" {
		record.ID = cached.ID
	}
	if record.TemplateID == "" {
		record.TemplateID = cached.TemplateID
	}
	if record.Alias == "" {
		record.Alias = cached.Alias
	}
	if record.ClientID == "" {
		record.ClientID = cached.ClientID
	}
	if record.EnvdVersion == "" {
		record.EnvdVersion = cached.EnvdVersion
	}
	if len(record.Metadata) == 0 {
		record.Metadata = cached.Metadata
	}
	if record.EnvdURL == "" {
		record.EnvdURL = cached.EnvdURL
	}
	record.RuntimeInfo = mergeSandboxRuntimeInfo(cached.RuntimeInfo, record.RuntimeInfo)
	if record.EnvdURL == "" {
		record.EnvdURL = record.RuntimeInfo.EnvdURL
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = cached.CreatedAt
	}
	if record.EndAt.IsZero() {
		record.EndAt = cached.EndAt
	}
	if record.State == "" {
		record.State = cached.State
	}
	if record.CPUCount == 0 {
		record.CPUCount = cached.CPUCount
	}
	if record.DiskSizeMB == 0 {
		record.DiskSizeMB = cached.DiskSizeMB
	}
	if record.MemoryMB == 0 {
		record.MemoryMB = cached.MemoryMB
	}
	if record.InternetAccessPolicy == InternetAccessUnspecified {
		record.InternetAccessPolicy = cached.InternetAccessPolicy
	}
	return record.clone()
}

func cloneStringSlicePtr(values *[]string) *[]string {
	if values == nil {
		return nil
	}

	copied := append([]string(nil), (*values)...)
	return &copied
}
