package inspectionidempotencyfingerprint

import (
	"path/filepath"
	"testing"
	"time"

	"restoration-quality/internal/application"
	"restoration-quality/internal/audit"
	"restoration-quality/internal/domain"
	"restoration-quality/internal/persistence"
)

func TestInspectionIdempotencyDetectsSampleChanges(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p, _ := domain.NewProject("p", "A-4", "文物", "馆", now)
	p.Status = domain.StatusInProgress
	p.RiskLevel = "medium"
	p.Procedures = []*domain.ProcedureRecord{
		{ID: "pr1", ProjectID: "p", Sequence: 1, Name: "清理", Technician: "技师", Completed: true},
		{ID: "pr2", ProjectID: "p", Sequence: 2, Name: "加固", Technician: "技师", Completed: true},
	}
	p.Evidence = []*domain.EvidenceItem{
		{ID: "e1", ProjectID: "p", ProcedureID: "pr1", Kind: "photo", URI: "e1://", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CapturedAt: now, Metadata: map[string]string{"description": "一"}},
		{ID: "e2", ProjectID: "p", ProcedureID: "pr2", Kind: "photo", URI: "e2://", SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CapturedAt: now, Metadata: map[string]string{"description": "二"}},
	}
	store, err := persistence.New(filepath.Join(t.TempDir(), "data")); if err != nil { t.Fatal(err) }
	if err := store.Create(p); err != nil { t.Fatal(err) }
	svc := application.New(store, audit.New(filepath.Join(t.TempDir(), "audit")))
	due := time.Now().Add(24 * time.Hour)
	if _, err := svc.Inspect("p", "检验", "remediate", []string{"发现"}, &due, "操作员", "req", []string{"pr1"}, []string{"e1"}); err != nil { t.Fatal(err) }
	if _, err := svc.Inspect("p", "检验", "remediate", []string{"发现"}, &due, "操作员", "req", []string{"pr2"}, []string{"e2"}); err == nil { t.Fatal("expected request reuse conflict when sampled procedures change") }
}
