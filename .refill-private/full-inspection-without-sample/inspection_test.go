package fullinspectionwithoutsample

import (
	"testing"
	"time"

	"restoration-quality/internal/domain"
)

func TestCompleteProjectPassesInspectionWithoutSampleList(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p, _ := domain.NewProject("p", "A-3", "文物", "馆", now)
	if err := p.SetBaseline("方案", []string{"材料"}, "medium", now); err != nil { t.Fatal(err) }
	for n := 1; n <= 2; n++ {
		pr := &domain.ProcedureRecord{ID: string(rune('a' + n)), Sequence: n, Name: string(rune('清' + n)), Technician: "技师", StartedAt: ptr(now), EndedAt: ptr(now.Add(time.Minute)), Completed: true}
		p.Procedures = append(p.Procedures, pr)
	}
	p.Status = domain.StatusInProgress
	if err := p.AddInspection(&domain.InspectionBatch{ID: "in", Inspector: "检验", Decision: "pass"}, now.Add(time.Hour)); err != nil { t.Fatal(err) }
}

func ptr(t time.Time) *time.Time { return &t }
