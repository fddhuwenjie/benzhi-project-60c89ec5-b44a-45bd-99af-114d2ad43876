package domain

import (
	"testing"
	"time"
)

func TestProjectWorkflowRules(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p, e := NewProject("p1", "a1", "器物", "馆", now)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.SetBaseline("方案", []string{"石灰"}, "low", now); e != nil {
		t.Fatal(e)
	}
	pr := &ProcedureRecord{ID: "pr1", Sequence: 1, Name: "清理", Technician: "技师"}
	if e = p.AddProcedure(pr, now); e != nil {
		t.Fatal(e)
	}
	st := now.Add(time.Minute)
	en := st.Add(time.Minute)
	ev := []*EvidenceItem{{ID: "e1", Kind: "photo", URI: "x", SHA256: "h", CapturedAt: st}}
	if e = p.CompleteProcedure("pr1", st, en, "20C", "说明", "完成", ev, en); e != nil {
		t.Fatal(e)
	}
	if !pr.Completed {
		t.Fatal("procedure should complete")
	}
	i := &InspectionBatch{ID: "i1", Inspector: "检验员", Decision: "pass"}
	if e = p.AddInspection(i, en); e != nil {
		t.Fatal(e)
	}
	if _, e = p.Release("a1", []string{"专家甲", "专家乙"}, map[string]string{"专家甲": "同意", "专家乙": "同意"}, en); e != nil {
		t.Fatal(e)
	}
	if p.Status != StatusArchived {
		t.Fatalf("status=%s", p.Status)
	}
}
func TestEvidenceTimeOrder(t *testing.T) {
	now := time.Now()
	p, _ := NewProject("p", "a", "t", "c", now)
	_ = p.SetBaseline("x", []string{"m"}, "medium", now)
	_ = p.AddProcedure(&ProcedureRecord{ID: "pr", Sequence: 1, Name: "n", Technician: "t"}, now)
	if e := p.CompleteProcedure("pr", now, now.Add(time.Minute), "env", "i", "r", []*EvidenceItem{{ID: "e", Kind: "photo", URI: "u", SHA256: "s", CapturedAt: now.Add(-time.Hour)}}, now); e == nil {
		t.Fatal("expected invalid evidence time")
	}
}
