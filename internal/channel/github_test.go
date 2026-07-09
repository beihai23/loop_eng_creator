package channel

import "testing"

func TestParseIssueJSONAndBody(t *testing.T) {
	raw := `[{"number":42,"title":"add X","body":"## 任务\nadd X\ntype: feature\n## 验收标准\n- [ ] it works"}]`
	tasks, err := parseIssuesJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].Ref != "42" || tasks[0].TaskType != "feature" {
		t.Fatalf("parse wrong: %+v", tasks)
	}
	if len(tasks[0].AcceptanceCriteria) != 1 || tasks[0].AcceptanceCriteria[0] != "it works" {
		t.Fatalf("criteria wrong: %+v", tasks[0].AcceptanceCriteria)
	}
	if tasks[0].Description != "add X" {
		t.Fatalf("desc wrong: %q", tasks[0].Description)
	}
}
