package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Tier-1：github→linear 切换后 DB 里残留旧 GitHub issue 号；Linear 对这些
// issue(id:) 回 "Entity not found" + 该别名 data=null。批量 GetTaskStates 必须把
// gone ref 当「缺席」而非「整批失败」，存活 ref 仍正常 decode。
func TestTier1LinearGetTaskStatesToleratesEntityNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// r0 = stale GitHub 号 "10"（gone）；r1 = 存活 Linear issue。
		_, _ = w.Write([]byte(`{"data":{"r0":null,"r1":{"state":{"id":"s-done","name":"Done","type":"completed"},"archivedAt":null}},"errors":[{"message":"Entity not found","path":["r0"]}]}`))
	}))
	defer srv.Close()
	lc := NewLinear("k", srv.URL, "p", "", nil)
	got, err := lc.GetTaskStates(context.Background(), []string{"10", "ENG-2"})
	if err != nil {
		t.Fatalf("GetTaskStates 必须容忍 per-ref Entity not found，got: %v", err)
	}
	if _, present := got["10"]; present {
		t.Fatalf("gone ref 10 必须缺席（不得被误标 open）: %+v", got["10"])
	}
	st, ok := got["ENG-2"]
	if !ok {
		t.Fatalf("存活 ref ENG-2 必须在结果里: %+v", got)
	}
	if st.IsOpen {
		t.Fatalf("ENG-2 completed-type 应 IsOpen=false: %+v", st)
	}
}

// Tier-1：容忍 Entity not found 不得顺带吞掉真实 graphql error。
func TestTier1LinearGetTaskStatesStillFoldsRealErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errors":[{"message":"Argument Validation Error"}]}`))
	}))
	defer srv.Close()
	lc := NewLinear("k", srv.URL, "p", "", nil)
	_, err := lc.GetTaskStates(context.Background(), []string{"ENG-1"})
	if err == nil || !strings.Contains(err.Error(), "Argument Validation Error") {
		t.Fatalf("真实 graphql error 必须仍折成 Go error，got: %v", err)
	}
}
