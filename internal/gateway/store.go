package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type SandboxStore struct {
	mu        sync.RWMutex
	sandboxes map[string]SandboxRecord
}

func NewSandboxStore() *SandboxStore {
	return &SandboxStore{
		sandboxes: make(map[string]SandboxRecord),
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
		record.EndAt = record.CreatedAt.Add(5 * time.Minute)
	}

	if record.State == "" {
		record.State = "running"
	}

	record = copySandboxRecord(record)
	if record.EnvdURL == "" {
		record.EnvdURL = record.RuntimeInfo.EnvdURL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.sandboxes[record.ID]; ok {
		return SandboxRecord{}, fmt.Errorf("sandbox %s already exists", record.ID)
	}

	s.sandboxes[record.ID] = record
	if err := s.saveLocked(); err != nil {
		delete(s.sandboxes, record.ID)
		return SandboxRecord{}, err
	}

	return record, nil
}

func (s *SandboxStore) Upsert(record SandboxRecord) (SandboxRecord, error) {
	if strings.TrimSpace(record.ID) == "" {
		return SandboxRecord{}, fmt.Errorf("sandbox id is required")
	}

	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	if record.EndAt.IsZero() {
		record.EndAt = record.CreatedAt.Add(5 * time.Minute)
	}

	if record.State == "" {
		record.State = "running"
	}

	record = copySandboxRecord(record)
	if record.EnvdURL == "" {
		record.EnvdURL = record.RuntimeInfo.EnvdURL
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	previous, exists := s.sandboxes[record.ID]
	if exists {
		record = mergeSandboxRecordForCache(previous, record)
	}
	s.sandboxes[record.ID] = record
	if err := s.saveLocked(); err != nil {
		if exists {
			s.sandboxes[record.ID] = previous
		} else {
			delete(s.sandboxes, record.ID)
		}
		return SandboxRecord{}, err
	}

	return copySandboxRecord(record), nil
}

func (s *SandboxStore) List() []SandboxRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	records := make([]SandboxRecord, 0, len(s.sandboxes))
	for _, record := range s.sandboxes {
		records = append(records, copySandboxRecord(record))
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})

	return records
}

func (s *SandboxStore) Get(id string) (SandboxRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.sandboxes[id]
	if !ok {
		return SandboxRecord{}, false
	}

	return copySandboxRecord(record), true
}

func (s *SandboxStore) Delete(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	previous, ok := s.sandboxes[id]
	if !ok {
		return false, nil
	}

	delete(s.sandboxes, id)
	if err := s.saveLocked(); err != nil {
		s.sandboxes[id] = previous
		return true, err
	}
	return true, nil
}

func (s *SandboxStore) SetState(id, state string) (SandboxRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.sandboxes[id]
	if !ok {
		return SandboxRecord{}, false, nil
	}

	previous := record
	record.State = state
	s.sandboxes[id] = record
	if err := s.saveLocked(); err != nil {
		s.sandboxes[id] = previous
		return SandboxRecord{}, true, err
	}
	return copySandboxRecord(record), true, nil
}

func (s *SandboxStore) SetStateAndRuntimeInfo(id, state string, info SandboxRuntimeInfo) (SandboxRecord, bool, error) {
	return s.SetStateRuntimeInfoAndEndAt(id, state, info, time.Time{})
}

func (s *SandboxStore) SetEndAt(id string, endAt time.Time) (SandboxRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.sandboxes[id]
	if !ok {
		return SandboxRecord{}, false, nil
	}

	previous := record
	record.EndAt = endAt
	s.sandboxes[id] = record
	if err := s.saveLocked(); err != nil {
		s.sandboxes[id] = previous
		return SandboxRecord{}, true, err
	}
	return copySandboxRecord(record), true, nil
}

func (s *SandboxStore) SetStateRuntimeInfoAndEndAt(id, state string, info SandboxRuntimeInfo, endAt time.Time) (SandboxRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.sandboxes[id]
	if !ok {
		return SandboxRecord{}, false, nil
	}

	previous := record
	record.State = state
	record.RuntimeInfo = info
	record.EnvdURL = info.EnvdURL
	if !endAt.IsZero() {
		record.EndAt = endAt
	}
	s.sandboxes[id] = record
	if err := s.saveLocked(); err != nil {
		s.sandboxes[id] = previous
		return SandboxRecord{}, true, err
	}
	return copySandboxRecord(record), true, nil
}

func (s *SandboxStore) saveLocked() error {
	return nil
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

	copied := make(map[string]string, len(values))
	for k, v := range values {
		copied[k] = v
	}
	return copied
}

func CopyStringMap(values map[string]string) map[string]string {
	return copyStringMap(values)
}

func copySandboxRecord(record SandboxRecord) SandboxRecord {
	record.Metadata = copyStringMap(record.Metadata)
	record.RuntimeInfo.VolumeMounts = append([]VolumeMount(nil), record.RuntimeInfo.VolumeMounts...)
	return record
}

func mergeSandboxRecordForCache(cached SandboxRecord, incoming SandboxRecord) SandboxRecord {
	record := copySandboxRecord(incoming)
	cached = copySandboxRecord(cached)

	if record.ID == "" {
		record.ID = cached.ID
	}
	if record.TemplateID == "" {
		record.TemplateID = cached.TemplateID
	}
	if record.Alias == nil {
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
	if record.AllowInternetAccess == nil {
		record.AllowInternetAccess = cached.AllowInternetAccess
	}

	return copySandboxRecord(record)
}

func cloneStringSlicePtr(values *[]string) *[]string {
	if values == nil {
		return nil
	}

	copied := append([]string(nil), (*values)...)
	return &copied
}
