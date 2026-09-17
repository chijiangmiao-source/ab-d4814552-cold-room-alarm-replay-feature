package main

import (
	"context"
	"database/sql"
	"testing"
)

func inputAt(id, door, kind, occurredAt string) EventInput {
	return EventInput{
		EventID:    id,
		DoorID:     door,
		Kind:       kind,
		OccurredAt: occurredAt,
	}
}

func mustInsert(t *testing.T, s *Store, in EventInput) Event {
	t.Helper()
	ev, created, err := s.Insert(context.Background(), in)
	if err != nil || !created {
		t.Fatalf("insert %s: ev=%+v created=%v err=%v", in.EventID, ev, created, err)
	}
	return ev
}

// 同门多次告警只更新最近类型/序号；CLOSED 结束时段；CLOSED 后再开是新时段。
func TestActiveDoorsLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	a1 := mustInsert(t, s, inputAt("a1", "door-A", KindOpenTooLong, "2026-09-14T22:00:00Z"))
	mustInsert(t, s, inputAt("b1", "door-B", KindForcedOpen, "2026-09-14T22:01:00Z"))

	doors, err := s.ActiveDoors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doors) != 2 || doors[0].DoorID != "door-A" || doors[1].DoorID != "door-B" {
		t.Fatalf("want [door-A door-B] ordered by start seq, got %+v", doors)
	}
	if doors[0].StartSeq != a1.Seq || doors[0].LastSeq != a1.Seq || doors[0].LastKind != KindOpenTooLong {
		t.Fatalf("door-A initial state wrong: %+v", doors[0])
	}

	// 同门再来一条异常告警：开始序号/开始时间不动，最近类型/序号/设备时间刷新。
	a2 := mustInsert(t, s, inputAt("a2", "door-A", KindForcedOpen, "2026-09-14T22:05:30Z"))
	doors, _ = s.ActiveDoors(ctx)
	if len(doors) != 2 {
		t.Fatalf("still two active doors, got %+v", doors)
	}
	gotA := doors[0]
	if gotA.StartSeq != a1.Seq || gotA.LastSeq != a2.Seq || gotA.LastKind != KindForcedOpen {
		t.Fatalf("door-A should refresh latest only: %+v", gotA)
	}
	if gotA.LastOccurredAt != "2026-09-14T22:05:30Z" {
		t.Fatalf("last device time not refreshed: %+v", gotA)
	}

	// 重复 event_id 不改变任何结果——哪怕重发时 kind/door 被改成别的值。
	dup, created, err := s.Insert(ctx, inputAt("a1", "door-OTHER", KindClosed, "2026-01-01T00:00:00Z"))
	if err != nil || created {
		t.Fatalf("duplicate callback: created=%v err=%v", created, err)
	}
	if dup.Seq != a1.Seq || dup.Kind != KindOpenTooLong {
		t.Fatalf("duplicate must return original record: %+v", dup)
	}
	doors, _ = s.ActiveDoors(ctx)
	if len(doors) != 2 || doors[0].LastSeq != a2.Seq || doors[0].LastKind != KindForcedOpen {
		t.Fatalf("duplicate callback changed active view: %+v", doors)
	}

	// CLOSED 结束 door-A 的时段，视图只剩 door-B。
	mustInsert(t, s, inputAt("a3", "door-A", KindClosed, "2026-09-14T22:06:00Z"))
	doors, _ = s.ActiveDoors(ctx)
	if len(doors) != 1 || doors[0].DoorID != "door-B" {
		t.Fatalf("after CLOSED only door-B should remain, got %+v", doors)
	}

	// door-B 也关闭后视图为空（必须是空切片而非 nil）。
	mustInsert(t, s, inputAt("b2", "door-B", KindClosed, "2026-09-14T22:07:00Z"))
	doors, _ = s.ActiveDoors(ctx)
	if doors == nil || len(doors) != 0 {
		t.Fatalf("want non-nil empty slice, got %#v", doors)
	}

	// CLOSED 之后再次异常：以新事件序号建立全新时段，而不是复活旧行。
	a4 := mustInsert(t, s, inputAt("a4", "door-A", KindForcedOpen, "2026-09-14T22:20:00Z"))
	doors, _ = s.ActiveDoors(ctx)
	if len(doors) != 1 {
		t.Fatalf("reopened door should create one period, got %+v", doors)
	}
	if doors[0].StartSeq != a4.Seq || doors[0].LastSeq != a4.Seq || doors[0].LastKind != KindForcedOpen {
		t.Fatalf("reopened door must start a fresh period: %+v", doors[0])
	}
}

// 没有异常告警、直接 CLOSED 的门（或重复 CLOSED）不应出现在视图中。
func TestActiveDoorsClosedWithoutOpenNeverAppears(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustInsert(t, s, inputAt("c1", "door-C", KindClosed, "2026-09-14T22:00:00Z"))
	mustInsert(t, s, inputAt("c2", "door-C", KindClosed, "2026-09-14T22:01:00Z"))
	doors, err := s.ActiveDoors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doors) != 0 {
		t.Fatalf("CLOSED before any alarm must not create an active row: %+v", doors)
	}
}

// 从“旧版本库”（只有 events、没有 door_active、user_version=0）启动时，
// 初始未关闭门视图必须从既有事件按 seq 回算得到。
func TestActiveDoorsBackfilledFromHistory(t *testing.T) {
	path := t.TempDir() + "/legacy.db"
	dsn := "file:" + path
	ctx := context.Background()

	// 1) 先用当前版本建库，再把 door_active 与版本标记抹掉，模拟旧版本写入的库。
	setup, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	setup.Close()

	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	if _, err := raw.Exec(`DROP TABLE door_active`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	legacy := []struct {
		id, door, kind, at string
	}{
		{"e1", "door-A", KindOpenTooLong, "2026-09-14T22:00:00Z"}, // A 开段 seq 1
		{"e2", "door-B", KindForcedOpen, "2026-09-14T22:01:00Z"},  // B 开段 seq 2
		{"e3", "door-A", KindForcedOpen, "2026-09-14T22:02:00Z"},  // A 刷新 seq 3
		{"e4", "door-C", KindClosed, "2026-09-14T22:02:30Z"},      // 无开段的 CLOSED
		{"e5", "door-A", KindClosed, "2026-09-14T22:03:00Z"},      // A 关闭
		{"e6", "door-B", KindOpenTooLong, "2026-09-14T22:04:00Z"}, // B 刷新 seq 6
	}
	for _, e := range legacy {
		if _, err := raw.Exec(
			`INSERT INTO events (event_id, door_id, kind, occurred_at, received_at)
			 VALUES (?, ?, ?, ?, ?)`, e.id, e.door, e.kind, e.at, "2026-09-14T14:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	// 2) 用当前版本重新打开：door_active 应被回算。
	s, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	doors, err := s.ActiveDoors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(doors) != 1 {
		t.Fatalf("backfill should leave only door-B active, got %+v", doors)
	}
	b := doors[0]
	if b.DoorID != "door-B" || b.StartSeq != 2 || b.LastSeq != 6 ||
		b.LastKind != KindOpenTooLong || b.LastOccurredAt != "2026-09-14T22:04:00Z" {
		t.Fatalf("backfilled door-B mismatch: %+v", b)
	}

	// 3) 再次打开不重复回算，结果一致；此后新写入继续正常维护。
	s.Close()
	s2, err := OpenStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	doors, _ = s2.ActiveDoors(ctx)
	if len(doors) != 1 || doors[0].LastSeq != 6 {
		t.Fatalf("reopen must preserve backfilled view, got %+v", doors)
	}
	a := mustInsert(t, s2, inputAt("e7", "door-A", KindForcedOpen, "2026-09-14T22:10:00Z"))
	doors, _ = s2.ActiveDoors(ctx)
	if len(doors) != 2 {
		t.Fatalf("new alarm after backfill should add door-A, got %+v", doors)
	}
	// 回算的 B 开始序号更早，排在新打开的 A 前面。
	if doors[0].DoorID != "door-B" || doors[1].DoorID != "door-A" || doors[1].StartSeq != a.Seq {
		t.Fatalf("ordering across backfilled/new periods wrong: %+v", doors)
	}
}
