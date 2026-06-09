package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"

	"e2b-local/internal/e2bapi"
)

type GatewayManagementStore struct {
	mu sync.RWMutex

	templates        map[string]managedTemplate
	templateFiles    map[string]managedTemplateFile
	templateUploads  map[string]managedTemplateFileUpload
	deletedTemplates map[string]bool
	nodeStatuses     map[string]e2bapi.NodeStatus
}

type managedTemplate struct {
	Template GatewayTemplate
	Tags     map[string]e2bapi.TemplateTag
	Logs     []e2bapi.BuildLogEntry
	Builds   map[string]managedTemplateBuild
}

type managedTemplateBuild struct {
	Template GatewayTemplate
	Logs     []e2bapi.BuildLogEntry
}

type GatewayTemplateBuildRecord struct {
	Template GatewayTemplate
	Logs     []e2bapi.BuildLogEntry
}

type managedTemplateFile struct {
	TemplateID string
	Hash       string
	Data       []byte
	CreatedAt  time.Time
}

type managedTemplateFileUpload struct {
	TemplateID string
	Hash       string
	Token      string
	ExpiresAt  time.Time
}

func NewGatewayManagementStore() *GatewayManagementStore {
	return &GatewayManagementStore{
		templates:        map[string]managedTemplate{},
		templateFiles:    map[string]managedTemplateFile{},
		templateUploads:  map[string]managedTemplateFileUpload{},
		deletedTemplates: map[string]bool{},
		nodeStatuses:     map[string]e2bapi.NodeStatus{},
	}
}

func (s *GatewayManagementStore) saveLocked() error {
	return nil
}

func (s *GatewayManagementStore) ListManagedTemplates() []GatewayTemplate {
	s.mu.RLock()
	defer s.mu.RUnlock()

	items := make([]GatewayTemplate, 0, len(s.templates))
	for _, item := range s.templates {
		items = append(items, item.Template)
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CreatedAt.Before(items[j].CreatedAt)
	})
	return items
}

func (s *GatewayManagementStore) UpsertTemplate(template GatewayTemplate, tags []string, logs []e2bapi.BuildLogEntry) (GatewayTemplate, error) {
	template.TemplateID = strings.TrimSpace(template.TemplateID)
	if template.TemplateID == "" {
		return template, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, existingOK := s.templates[template.TemplateID]
	previousDeleted, previousDeletedOK := s.deletedTemplates[template.TemplateID]
	tagMap := cloneTemplateTags(existing.Tags)
	if tagMap == nil {
		tagMap = map[string]e2bapi.TemplateTag{}
	}
	builds := cloneTemplateBuilds(existing.Builds)
	if builds == nil {
		builds = map[string]managedTemplateBuild{}
	}

	buildID := stableTemplateBuildUUID(template.TemplateID, template.BuildID)
	now := time.Now().UTC()
	for _, tag := range normalizedTags(tags) {
		tagMap[tag] = e2bapi.TemplateTag{
			BuildID:   buildID,
			CreatedAt: now,
			Tag:       tag,
		}
	}

	if len(logs) == 0 {
		logs = existing.Logs
	}

	if strings.TrimSpace(template.BuildID) != "" {
		build := builds[template.BuildID]
		buildLogs := logs
		if len(buildLogs) == 0 {
			buildLogs = build.Logs
		}
		if template.CreatedAt.IsZero() && !build.Template.CreatedAt.IsZero() {
			template.CreatedAt = build.Template.CreatedAt
		}
		if template.UpdatedAt.IsZero() {
			template.UpdatedAt = now
		}
		builds[template.BuildID] = managedTemplateBuild{
			Template: template,
			Logs:     append([]e2bapi.BuildLogEntry(nil), buildLogs...),
		}
	}

	s.templates[template.TemplateID] = managedTemplate{
		Template: template,
		Tags:     tagMap,
		Logs:     append([]e2bapi.BuildLogEntry(nil), logs...),
		Builds:   builds,
	}
	delete(s.deletedTemplates, template.TemplateID)
	if err := s.saveLocked(); err != nil {
		if existingOK {
			s.templates[template.TemplateID] = existing
		} else {
			delete(s.templates, template.TemplateID)
		}
		if previousDeletedOK {
			s.deletedTemplates[template.TemplateID] = previousDeleted
		} else {
			delete(s.deletedTemplates, template.TemplateID)
		}
		return template, err
	}
	return template, nil
}

func (s *GatewayManagementStore) GetManagedTemplate(templateID string) (GatewayTemplate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.templates[templateID]
	if !ok {
		return GatewayTemplate{}, false
	}
	return item.Template, true
}

func (s *GatewayManagementStore) DeleteTemplate(templateID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previousTemplate, previousTemplateOK := s.templates[templateID]
	previousDeleted, previousDeletedOK := s.deletedTemplates[templateID]
	delete(s.templates, templateID)
	s.deletedTemplates[templateID] = true
	if err := s.saveLocked(); err != nil {
		if previousTemplateOK {
			s.templates[templateID] = previousTemplate
		} else {
			delete(s.templates, templateID)
		}
		if previousDeletedOK {
			s.deletedTemplates[templateID] = previousDeleted
		} else {
			delete(s.deletedTemplates, templateID)
		}
		return err
	}
	return nil
}

func (s *GatewayManagementStore) DeletedTemplateIDs() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]bool, len(s.deletedTemplates))
	for templateID, deleted := range s.deletedTemplates {
		result[templateID] = deleted
	}
	return result
}

func (s *GatewayManagementStore) ListTemplateTags(templateID string) []e2bapi.TemplateTag {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.templates[templateID]
	if !ok {
		return nil
	}
	tags := make([]e2bapi.TemplateTag, 0, len(item.Tags))
	for _, tag := range item.Tags {
		tags = append(tags, tag)
	}
	sort.Slice(tags, func(i, j int) bool {
		return tags[i].Tag < tags[j].Tag
	})
	return tags
}

func (s *GatewayManagementStore) AssignTemplateTags(template GatewayTemplate, tags []string) (e2bapi.AssignedTemplateTags, error) {
	template, err := s.UpsertTemplate(template, tags, nil)
	if err != nil {
		return e2bapi.AssignedTemplateTags{}, err
	}
	return e2bapi.AssignedTemplateTags{
		BuildID: stableTemplateBuildUUID(template.TemplateID, template.BuildID),
		Tags:    normalizedTags(tags),
	}, nil
}

func (s *GatewayManagementStore) DeleteTemplateTags(templateID string, tags []string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.templates[templateID]
	if !ok {
		return false, nil
	}
	item := cloneManagedTemplate(existing)
	for _, tag := range normalizedTags(tags) {
		delete(item.Tags, tag)
	}
	s.templates[templateID] = item
	if err := s.saveLocked(); err != nil {
		s.templates[templateID] = existing
		return true, err
	}
	return true, nil
}

func (s *GatewayManagementStore) TemplateBuildLogs(templateID string) []e2bapi.BuildLogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.templates[templateID]
	if !ok {
		return nil
	}
	return append([]e2bapi.BuildLogEntry(nil), item.Logs...)
}

func (s *GatewayManagementStore) TemplateBuild(template GatewayTemplate, buildID string) (GatewayTemplateBuildRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.templates[template.TemplateID]
	if !ok {
		return GatewayTemplateBuildRecord{}, false
	}

	build, ok := managedTemplateBuildByID(template.TemplateID, item.Builds, buildID)
	if !ok {
		return GatewayTemplateBuildRecord{}, false
	}

	return GatewayTemplateBuildRecord{
		Template: build.Template,
		Logs:     append([]e2bapi.BuildLogEntry(nil), build.Logs...),
	}, true
}

func (s *GatewayManagementStore) TemplateBuilds(templateID string) []GatewayTemplateBuildRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	item, ok := s.templates[templateID]
	if !ok {
		return nil
	}

	records := make([]GatewayTemplateBuildRecord, 0, len(item.Builds))
	for _, build := range item.Builds {
		records = append(records, GatewayTemplateBuildRecord{
			Template: build.Template,
			Logs:     append([]e2bapi.BuildLogEntry(nil), build.Logs...),
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Template.UpdatedAt.Equal(records[j].Template.UpdatedAt) {
			return records[i].Template.BuildID < records[j].Template.BuildID
		}
		return records[i].Template.UpdatedAt.After(records[j].Template.UpdatedAt)
	})
	return records
}

func managedTemplateBuildByID(templateID string, builds map[string]managedTemplateBuild, buildID string) (managedTemplateBuild, bool) {
	buildID = strings.TrimSpace(buildID)
	if buildID == "" {
		return managedTemplateBuild{}, false
	}
	if build, ok := builds[buildID]; ok {
		return build, true
	}
	for _, build := range builds {
		if stableTemplateBuildUUID(templateID, build.Template.BuildID).String() == buildID {
			return build, true
		}
	}
	return managedTemplateBuild{}, false
}

func (s *GatewayManagementStore) TemplateFilePresent(templateID string, hash string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.templateFiles[templateFileKey(templateID, hash)]
	return ok
}

func (s *GatewayManagementStore) CreateTemplateFileUpload(templateID string, hash string) (string, error) {
	token, err := newManagementSecret("tfu_")
	if err != nil {
		return "", err
	}

	upload := managedTemplateFileUpload{
		TemplateID: strings.TrimSpace(templateID),
		Hash:       strings.TrimSpace(hash),
		Token:      token,
		ExpiresAt:  time.Now().UTC().Add(15 * time.Minute),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.templateUploads[token] = upload
	return token, nil
}

func (s *GatewayManagementStore) StoreTemplateFileUpload(templateID string, hash string, token string, data []byte) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	upload, ok := s.templateUploads[strings.TrimSpace(token)]
	if !ok {
		return false, nil
	}
	if time.Now().UTC().After(upload.ExpiresAt) {
		delete(s.templateUploads, upload.Token)
		return false, nil
	}
	if upload.TemplateID != strings.TrimSpace(templateID) || upload.Hash != strings.TrimSpace(hash) {
		return false, nil
	}

	key := templateFileKey(templateID, hash)
	previousFile, previousFileOK := s.templateFiles[key]
	s.templateFiles[key] = managedTemplateFile{
		TemplateID: strings.TrimSpace(templateID),
		Hash:       strings.TrimSpace(hash),
		Data:       append([]byte(nil), data...),
		CreatedAt:  time.Now().UTC(),
	}
	delete(s.templateUploads, upload.Token)
	if err := s.saveLocked(); err != nil {
		if previousFileOK {
			s.templateFiles[key] = previousFile
		} else {
			delete(s.templateFiles, key)
		}
		s.templateUploads[upload.Token] = upload
		return true, err
	}
	return true, nil
}

func (s *GatewayManagementStore) TemplateFile(templateID string, hash string) (TemplateBuildFile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	file, ok := s.templateFiles[templateFileKey(templateID, hash)]
	if !ok {
		return TemplateBuildFile{}, false
	}
	return TemplateBuildFile{
		TemplateID: file.TemplateID,
		Hash:       file.Hash,
		Data:       append([]byte(nil), file.Data...),
	}, true
}

func (s *GatewayManagementStore) NodeStatus(nodeID string) e2bapi.NodeStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if status := s.nodeStatuses[nodeID]; status != "" {
		return status
	}
	return e2bapi.NodeStatusReady
}

func (s *GatewayManagementStore) SetNodeStatus(nodeID string, status e2bapi.NodeStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, previousOK := s.nodeStatuses[nodeID]
	s.nodeStatuses[nodeID] = status
	if err := s.saveLocked(); err != nil {
		if previousOK {
			s.nodeStatuses[nodeID] = previous
		} else {
			delete(s.nodeStatuses, nodeID)
		}
		return err
	}
	return nil
}

func cloneManagedTemplate(item managedTemplate) managedTemplate {
	return managedTemplate{
		Template: item.Template,
		Tags:     cloneTemplateTags(item.Tags),
		Logs:     append([]e2bapi.BuildLogEntry(nil), item.Logs...),
		Builds:   cloneTemplateBuilds(item.Builds),
	}
}

func cloneTemplateTags(tags map[string]e2bapi.TemplateTag) map[string]e2bapi.TemplateTag {
	if tags == nil {
		return nil
	}
	cloned := make(map[string]e2bapi.TemplateTag, len(tags))
	for key, value := range tags {
		cloned[key] = value
	}
	return cloned
}

func cloneTemplateBuilds(builds map[string]managedTemplateBuild) map[string]managedTemplateBuild {
	if builds == nil {
		return nil
	}
	cloned := make(map[string]managedTemplateBuild, len(builds))
	for key, value := range builds {
		value.Logs = append([]e2bapi.BuildLogEntry(nil), value.Logs...)
		cloned[key] = value
	}
	return cloned
}

func newManagementSecret(prefix string) (string, error) {
	var bytes [24]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(bytes[:]), nil
}

func normalizedTags(tags []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		result = append(result, tag)
	}
	sort.Strings(result)
	return result
}

func templateFileKey(templateID string, hash string) string {
	return strings.TrimSpace(templateID) + "/" + strings.TrimSpace(hash)
}
