package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func insertDoorEvent(t *testing.T, s *Store, eventID, door, kind, occurredAt string) Event {
	t.Helper()
	ev, created, err := s.Insert(context.Background(), EventInput{
		EventID:    eventID,
		DoorID:     door,
		Kind:       kind,
		OccurredAt: occurredAt,
	})
	if err != nil {
		t.Fatalf("insert %s: %v", eventID, err)
	}
	if !created {
		t.Fatalf("insert %s: expected created=true", eventID)
	}
	return ev
}

func activeMap(t *testing.T, s *Store) map[string]ActiveDoor {
	t.Helper()
	got, err := s.ActiveDoors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]ActiveDoor, len(got))
	for _, d := range got {
		m[d.DoorID] = d
	}
	return m
}

// 完整生命周期：首次异常开段、同门多次告警刷新最近类型/序号、CLOSED 闭段、
// 无进行中时段的 CLOSED 什么都不做。
func TestActiveDoorLifecycle(t *testing.T) {
	s := newTestStore(t)

	e1 := insertDoorEvent(t, s, "a1", "door-A", KindOpenTooLong, "2026-09-14T22:01:00Z")
	e2 := insertDoorEvent(t, s, "a2", "door-A", KindForcedOpen, "2026-09-14T22:05:00Z")
	insertDoorEvent(t, s, "b1", "door-B", KindForcedOpen, "2026-09-14T22:06:00Z") // seq 3
	// 没有异常时段的门收到 CLOSED：不应产生行。
	insertDoorEvent(t, s, "c1", "door-C", KindClosed, "2026-09-14T22:07:00Z") // seq 4

	got, err := s.ActiveDoors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 active doors, got %+v", got)
	}
	// 固定顺序：按异常开始序号升序（A 在 B 之前），与门号字典序无关。
	if got[0].DoorID != "door-A" || got[1].DoorID != "door-B" {
		t.Fatalf("order must be by start_seq: %+v", got)
	}
	a := got[0]
	// 同门后续告警只更新最近类型/序号/设备时间，开始序号保持为首次。
	if a.StartSeq != e1.Seq || a.LastSeq != e2.Seq || a.LastKind != KindForcedOpen ||
		a.LastOccurredAt != "2026-09-14T22:05:00Z" {
		t.Fatalf("door-A state wrong: %+v", a)
	}
	if got[1].StartSeq != 3 || got[1].LastSeq != 3 || got[1].LastKind != KindForcedOpen {
		t.Fatalf("door-B state wrong: %+v", got[1])
	}

	// 重复回调（哪怕字段改成 CLOSED）不改变门状态：door-A 仍在未关闭列表中。
	dup, created, err := s.Insert(context.Background(), EventInput{
		EventID: "a1", DoorID: "door-A", Kind: KindClosed, OccurredAt: "2026-09-14T23:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || dup.Seq != e1.Seq || dup.Kind != KindOpenTooLong {
		t.Fatalf("duplicate must return first record: %+v created=%v", dup, created)
	}
	if m := activeMap(t, s); len(m) != 2 {
		t.Fatalf("duplicate callback must not change active doors: %+v", m)
	}

	// CLOSED 结束 door-A 的异常时段。
	insertDoorEvent(t, s, "a3", "door-A", KindClosed, "2026-09-14T22:10:00Z")
	got, _ = s.ActiveDoors(context.Background())
	if len(got) != 1 || got[0].DoorID != "door-B" {
		t.Fatalf("after close only door-B should remain: %+v", got)
	}

	// 关闭后再次异常：新时段的开始序号取新事件，而不是旧时段。
	e4 := insertDoorEvent(t, s, "a4", "door-A", KindOpenTooLong, "2026-09-14T22:20:00Z")
	got, _ = s.ActiveDoors(context.Background())
	if len(got) != 2 {
		t.Fatalf("want 2 active doors: %+v", got)
	}
	// door-A 新时段开始更晚，必须排在 door-B 之后。
	if got[0].DoorID != "door-B" || got[1].DoorID != "door-A" || got[1].StartSeq != e4.Seq {
		t.Fatalf("reopened door-A starts a new period ordered by new start_seq: %+v", got)
	}
}

// 从既有事件回算初始视图：重放历史后状态必须与实时维护一致。
func TestActiveDoorsRebuiltFromHistory(t *testing.T) {
	dir := t.TempDir() + "/rebuild.db"
	ctx := context.Background()

	s1, err := OpenStore(ctx, "file:"+dir)
	if err != nil {
		t.Fatal(err)
	}
	insertDoorEvent(t, s1, "x1", "door-X", KindOpenTooLong, "2026-09-14T22:01:00Z")       // seq1 开段
	insertDoorEvent(t, s1, "y1", "door-Y", KindForcedOpen, "2026-09-14T22:02:00Z")        // seq2 开段
	insertDoorEvent(t, s1, "x2", "door-X", KindForcedOpen, "2026-09-14T22:03:00Z")        // seq3 刷新 X
	insertDoorEvent(t, s1, "x3", "door-X", KindClosed, "2026-09-14T22:04:00Z")            // seq4 关闭 X
	y2 := insertDoorEvent(t, s1, "y2", "door-Y", KindOpenTooLong, "2026-09-14T22:05:00Z") // seq5 刷新 Y
	// 断线窗口内“开了又关”的门，回算后不应残留。
	insertDoorEvent(t, s1, "z1", "door-Z", KindForcedOpen, "2026-09-14T22:06:00Z") // seq6
	insertDoorEvent(t, s1, "z2", "door-Z", KindClosed, "2026-09-14T22:07:00Z")     // seq7
	// 重复 event_id 不应影响回算。
	if _, created, _ := s1.Insert(ctx, EventInput{
		EventID: "x1", DoorID: "door-X", Kind: KindForcedOpen, OccurredAt: "2026-09-14T22:01:00Z",
	}); created {
		t.Fatal("duplicate callback must not create")
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：OpenStore 自动回算。
	s2, err := OpenStore(ctx, "file:"+dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, err := s2.ActiveDoors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rebuild should leave only door-Y, got %+v", got)
	}
	if got[0].DoorID != "door-Y" || got[0].StartSeq != 2 || got[0].LastSeq != y2.Seq ||
		got[0].LastKind != KindOpenTooLong || got[0].LastOccurredAt != "2026-09-14T22:05:00Z" {
		t.Fatalf("rebuilt door-Y state wrong: %+v", got[0])
	}

	// 手动再回算一次：幂等，结果不变。
	if err := s2.RebuildActiveDoors(ctx); err != nil {
		t.Fatal(err)
	}
	got2, _ := s2.ActiveDoors(ctx)
	if len(got2) != 1 || got2[0] != got[0] {
		t.Fatalf("rebuild not idempotent: %+v vs %+v", got2, got)
	}
}

// 全部事件都已关闭时，回算结果为空，且接口返回 [] 而不是 null。
func TestRebuildEmptyWhenAllClosed(t *testing.T) {
	s := newTestStore(t)
	insertDoorEvent(t, s, "d1", "door-D", KindForcedOpen, "2026-09-14T22:01:00Z")
	insertDoorEvent(t, s, "d2", "door-D", KindClosed, "2026-09-14T22:02:00Z")
	if err := s.RebuildActiveDoors(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActiveDoors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no active doors, got %+v", got)
	}
}

// 接口：空库返回 []；按开始序号固定顺序；错误沿用 {"error":...} 结构。
func TestActiveDoorsAPI(t *testing.T) {
	ts := newTestServer(t)

	// 空库：200 且为 []。
	resp, err := http.Get(ts.URL + "/api/doors/active")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(raw) != "[]\n" {
		t.Fatalf("empty active list: status=%d body=%q", resp.StatusCode, raw)
	}

	// 先上报 door-b 再上报 door-a：返回顺序必须按开始序号（b 在前），
	// 而不是按门号字典序。
	postEvent(t, ts, `{"event_id":"ab-b","door_id":"door-b","kind":"FORCED_OPEN","occurred_at":"2026-09-14T22:02:00Z"}`)
	postEvent(t, ts, `{"event_id":"ab-a","door_id":"door-a","kind":"OPEN_TOO_LONG","occurred_at":"2026-09-14T22:03:00Z"}`)

	resp, err = http.Get(ts.URL + "/api/doors/active")
	if err != nil {
		t.Fatal(err)
	}
	var doors []ActiveDoor
	if err := json.NewDecoder(resp.Body).Decode(&doors); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(doors) != 2 {
		t.Fatalf("status=%d doors=%+v", resp.StatusCode, doors)
	}
	if doors[0].DoorID != "door-b" || doors[0].StartSeq != 1 || doors[0].LastSeq != 1 ||
		doors[0].LastKind != KindForcedOpen || doors[0].LastOccurredAt != "2026-09-14T22:02:00Z" {
		t.Fatalf("first row wrong: %+v", doors[0])
	}
	if doors[1].DoorID != "door-a" || doors[1].StartSeq != 2 {
		t.Fatalf("second row wrong: %+v", doors[1])
	}

	// 只读接口：POST 必须 405，且错误体沿用现有 JSON 结构。
	presp, err := http.Post(ts.URL+"/api/doors/active", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer presp.Body.Close()
	if presp.StatusCode != http.StatusMethodNotAllowed || presp.Header.Get("Allow") != "GET" {
		t.Fatalf("want 405 Allow=GET, got %d allow=%q", presp.StatusCode, presp.Header.Get("Allow"))
	}
	var eb errorBody
	if err := json.NewDecoder(presp.Body).Decode(&eb); err != nil || eb.Error == "" {
		t.Fatalf("want JSON error body, got %+v err=%v", eb, err)
	}
}
