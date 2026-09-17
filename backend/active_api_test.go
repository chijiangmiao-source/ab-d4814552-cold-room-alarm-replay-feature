package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func doorBody(id, door, kind string) string {
	return fmt.Sprintf(
		`{"event_id":%q,"door_id":%q,"kind":%q,"occurred_at":"2026-09-14T22:00:00Z"}`,
		id, door, kind)
}

func getActiveDoors(t *testing.T, ts *httptest.Server) (int, []ActiveDoor) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/doors/active")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET active doors: status %d body %s", resp.StatusCode, raw)
	}
	var got []ActiveDoor
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode active doors: %v (body %s)", err, raw)
	}
	return resp.StatusCode, got
}

// 接口必须按异常开始序号固定返回门号、开始/最近序号、最近类型与设备时间。
func TestActiveDoorsAPIFixedOrderAndShape(t *testing.T) {
	ts := newTestServer(t)

	// 空库：200 且是空数组 []，不是 null。
	resp, err := http.Get(ts.URL + "/api/doors/active")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(raw) != "[]\n" {
		t.Fatalf("empty view should be 200 [], got %d %q", resp.StatusCode, raw)
	}

	// 按 seq：1=A 开段, 2=B 开段, 3=A 刷新。返回顺序必须固定为 A、B（按开始序号）。
	postEvent(t, ts, doorBody("api-a1", "door-A", "OPEN_TOO_LONG"))
	postEvent(t, ts, doorBody("api-b1", "door-B", "FORCED_OPEN"))
	_, a2, _ := postEvent(t, ts, doorBody("api-a2", "door-A", "FORCED_OPEN"))

	_, got := getActiveDoors(t, ts)
	want := []ActiveDoor{
		{
			DoorID:         "door-A",
			StartSeq:       1,
			LastSeq:        a2.Seq, // 3
			LastKind:       "FORCED_OPEN",
			LastOccurredAt: "2026-09-14T22:00:00Z",
		},
		{
			DoorID:         "door-B",
			StartSeq:       2,
			LastSeq:        2,
			LastKind:       "FORCED_OPEN",
			LastOccurredAt: "2026-09-14T22:00:00Z",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("want %d active doors, got %+v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d mismatch:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}

	// 关闭 door-A 后只剩 B，顺序再校准一次。
	postEvent(t, ts, doorBody("api-a3", "door-A", "CLOSED"))
	_, got = getActiveDoors(t, ts)
	if len(got) != 1 || got[0].DoorID != "door-B" || got[0].StartSeq != 2 {
		t.Fatalf("after closing A only B should remain in order, got %+v", got)
	}
}

// 只读接口：非 GET 方法返回 405 与现有错误结构，且带 Allow 头。
func TestActiveDoorsAPIMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/api/doors/active", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET" {
		t.Fatalf("want 405 Allow: GET, got %d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Error == "" {
		t.Fatalf("want existing JSON error body, got %+v err=%v", body, err)
	}
}
