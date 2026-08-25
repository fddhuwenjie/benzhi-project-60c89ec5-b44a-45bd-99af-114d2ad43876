package persistence

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"restoration-quality/internal/domain"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	dir      string
	mu       sync.RWMutex
	projects map[string]*domain.RestorationProject
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, projects: map[string]*domain.RestorationProject{}}
	return s, s.load()
}
func (s *Store) load() error {
	b, err := os.ReadFile(filepath.Join(s.dir, "projects.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.projects)
}
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.projects, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "projects.tmp")
	if err = os.WriteFile(tmp, b, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "projects.json"))
}
func (s *Store) Create(p *domain.RestorationProject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.projects[p.ID]; ok {
		return domain.ErrConflict
	}
	s.projects[p.ID] = p
	if err := s.saveLocked(); err != nil {
		delete(s.projects, p.ID)
		return err
	}
	return nil
}

// CreateBatch atomically persists a validated batch of projects.
func (s *Store) CreateBatch(ps []*domain.RestorationProject) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range ps {
		if p == nil {
			return domain.ErrInvalid
		}
		if _, ok := s.projects[p.ID]; ok {
			return domain.ErrConflict
		}
	}
	for _, p := range ps {
		s.projects[p.ID] = p
	}
	if err := s.saveLocked(); err != nil {
		for _, p := range ps {
			delete(s.projects, p.ID)
		}
		return err
	}
	return nil
}
func (s *Store) Get(id string) (*domain.RestorationProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.projects[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	b, _ := json.Marshal(p)
	var cp domain.RestorationProject
	_ = json.Unmarshal(b, &cp)
	return &cp, nil
}

func (s *Store) FindByAsset(asset string) (*domain.RestorationProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(asset)), " "))
	for _, p := range s.projects {
		stored := p.AssetKey
		if stored == "" {
			stored = strings.ToUpper(strings.Join(strings.Fields(strings.TrimSpace(p.AssetCode)), " "))
		}
		if stored == key {
			b, _ := json.Marshal(p)
			var cp domain.RestorationProject
			_ = json.Unmarshal(b, &cp)
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *Store) FindByRequest(request string) (*domain.RestorationProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.projects {
		if p.CreateRequestID == request {
			b, _ := json.Marshal(p)
			var cp domain.RestorationProject
			_ = json.Unmarshal(b, &cp)
			return &cp, nil
		}
	}
	return nil, domain.ErrNotFound
}
func (s *Store) Update(p *domain.RestorationProject, expected int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.projects[p.ID]
	if !ok {
		return domain.ErrNotFound
	}
	if expected > 0 && cur.PlanRevision != expected {
		return domain.ErrConflict
	}
	previous := s.projects[p.ID]
	s.projects[p.ID] = p
	if err := s.saveLocked(); err != nil {
		s.projects[p.ID] = previous
		return err
	}
	return nil
}
func (s *Store) List() []*domain.RestorationProject {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*domain.RestorationProject, 0, len(s.projects))
	for _, p := range s.projects {
		b, _ := json.Marshal(p)
		var cp domain.RestorationProject
		_ = json.Unmarshal(b, &cp)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

type ProjectFilter struct {
	Status, RiskLevel, Custodian string
	AssetPrefix                  string
	CreatedSince, CreatedUntil   *time.Time
}

func (s *Store) ListFiltered(f ProjectFilter) []*domain.RestorationProject {
	all := s.List()
	out := make([]*domain.RestorationProject, 0, len(all))
	for _, p := range all {
		if f.Status != "" && string(p.Status) != f.Status {
			continue
		}
		if f.RiskLevel != "" && p.RiskLevel != f.RiskLevel {
			continue
		}
		if f.Custodian != "" && p.Custodian != f.Custodian {
			continue
		}
		if f.AssetPrefix != "" && !strings.HasPrefix(p.AssetCode, f.AssetPrefix) {
			continue
		}
		if f.CreatedSince != nil && p.CreatedAt.Before(*f.CreatedSince) {
			continue
		}
		if f.CreatedUntil != nil && p.CreatedAt.After(*f.CreatedUntil) {
			continue
		}
		out = append(out, p)
	}
	return out
}

var _ = errors.New
