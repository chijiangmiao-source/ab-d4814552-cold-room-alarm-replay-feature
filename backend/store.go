package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Event 是落库后对外暴露的告警记录。Seq 是服务端序号：
// 由 SQLite AUTOINCREMENT 分配，严格递增且永不复用，是排序与续传的唯一依据。
// OccurredAt 来自设备时钟，仅用于展示，绝不参与排序。
type Event struct {
	Seq        int64  `json:"seq"`
	EventID    string `json:"event_id"`
	DoorID     string `json:"door_id"`
	Kind       string `json:"kind"`
	OccurredAt string `json:"occurred_at"`
	ReceivedAt string `json:"received_at"`
}

// EventInput 是设备网关回调的请求体。
type EventInput struct {
	EventID    string `json:"event_id"`
	DoorID     string `json:"door_id"`
	OccurredAt string `json:"occurred_at"`
	Kind       string `json:"kind"`
}

// 允许的告警类型。
const (
	KindOpenTooLong = "OPEN_TOO_LONG"
	KindForcedOpen  = "FORCED_OPEN"
	KindClosed      = "CLOSED"
)

// ValidationError 表示请求内容非法，调用方应返回 4xx，且不得分配序号。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func validate(in EventInput) error {
	if in.EventID == "" {
		return &ValidationError{Msg: "event_id is required"}
	}
	if in.DoorID == "" {
		return &ValidationError{Msg: "door_id is required"}
	}
	if in.OccurredAt == "" {
		return &ValidationError{Msg: "occurred_at is required"}
	}
	if _, err := time.Parse(time.RFC3339, in.OccurredAt); err != nil {
		return &ValidationError{Msg: "occurred_at must be an RFC3339 timestamp, e.g. 2026-09-14T22:00:00Z"}
	}
	switch in.Kind {
	case KindOpenTooLong, KindForcedOpen, KindClosed:
	default:
		return &ValidationError{Msg: fmt.Sprintf("kind must be one of %s, %s, %s", KindOpenTooLong, KindForcedOpen, KindClosed)}
	}
	return nil
}

// Store 封装 SQLite。所有写入都通过 busy_timeout 容忍短暂锁竞争。
type Store struct {
	db *sql.DB
}

func OpenStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单连接即可串行化写入，避免 SQLITE_BUSY；吞吐对告警场景完全够用。
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// activeDoorsSchemaVersion 标记 door_active 是否已从既有事件回算过。
// 存于 SQLite 的 PRAGMA user_version：新库为 0，旧版本写入的库升级时也是 0。
const activeDoorsSchemaVersion = 1

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id    TEXT NOT NULL UNIQUE,
	door_id     TEXT NOT NULL,
	kind        TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_seq ON events(seq);

-- door_active 是值班员接班时看到的“未关闭门”视图：每行是一个仍处于异常
-- 开启时段的门。它只在与 events 插入相同的事务里被维护：
--   首次接受的 OPEN_TOO_LONG/FORCED_OPEN 建立时段（或在 CLOSED 之后重开）；
--   同门后续的异常告警只更新最近类型/最近序号/最近设备时间；
--   CLOSED 结束并删除该时段；重复 event_id 不进入写入事务，因此不改变结果。
CREATE TABLE IF NOT EXISTS door_active (
	door_id          TEXT PRIMARY KEY,
	start_seq        INTEGER NOT NULL,
	start_occurred_at TEXT NOT NULL,
	last_seq         INTEGER NOT NULL,
	last_kind        TEXT NOT NULL,
	last_occurred_at TEXT NOT NULL
);
`); err != nil {
		return err
	}
	return s.backfillActiveDoors(ctx)
}

// backfillActiveDoors 从既有事件按 seq 升序重放一遍，回算未关闭门初始视图。
// 新库为空回放；旧版本写入的库升级后首次打开时，door_active 会与事件流
// 严格对齐（events.event_id 本就唯一，无需考虑重复回调）。
func (s *Store) backfillActiveDoors(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= activeDoorsSchemaVersion {
		return nil // 已回算并持续由写入路径维护。
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 从头重放：先清空，保证即使上次在提交与打版本标记之间崩溃，重放也确定幂等。
	if _, err := tx.ExecContext(ctx, `DELETE FROM door_active`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT door_id, kind, occurred_at, seq FROM events ORDER BY seq ASC`)
	if err != nil {
		return err
	}
	type pt struct {
		doorID, kind, occurredAt string
		seq                      int64
	}
	var pending []pt
	for rows.Next() {
		var p pt
		if err := rows.Scan(&p.doorID, &p.kind, &p.occurredAt, &p.seq); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// open 记录每扇门当前是否处于异常时段。
	open := map[string]bool{}
	for _, p := range pending {
		switch p.kind {
		case KindClosed:
			if open[p.doorID] {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM door_active WHERE door_id = ?`, p.doorID); err != nil {
					return err
				}
				open[p.doorID] = false
			}
		case KindOpenTooLong, KindForcedOpen:
			if open[p.doorID] {
				if _, err := tx.ExecContext(ctx, `
UPDATE door_active
SET last_seq = ?, last_kind = ?, last_occurred_at = ?
WHERE door_id = ?`, p.seq, p.kind, p.occurredAt, p.doorID); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx, `
INSERT INTO door_active
	(door_id, start_seq, start_occurred_at, last_seq, last_kind, last_occurred_at)
VALUES (?, ?, ?, ?, ?, ?)`,
					p.doorID, p.seq, p.occurredAt, p.seq, p.kind, p.occurredAt); err != nil {
					return err
				}
				open[p.doorID] = true
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// PRAGMA user_version 不能在事务内生效，提交后再打标记。
	_, err = s.db.ExecContext(ctx,
		fmt.Sprintf(`PRAGMA user_version = %d`, activeDoorsSchemaVersion))
	return err
}

// ErrNotFound 用于按 event_id 查询未命中。
var ErrNotFound = errors.New("event not found")

// FindByEventID 返回既有记录。未命中时返回 ErrNotFound。
func (s *Store) FindByEventID(ctx context.Context, eventID string) (Event, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT seq, event_id, door_id, kind, occurred_at, received_at
FROM events WHERE event_id = ?`, eventID)
	return scanEvent(row)
}

// Insert 写入一条新告警并返回带服务端序号的完整记录。
// 返回 created=false 表示该 event_id 已存在（重复回调或唯一约束冲突），
// 此时返回的是既有记录，不新增行、不分配新序号、不改变未关闭门视图。
//
// 首次接受的事件与其对 door_active 的维护在同一个 SQLite 事务中提交：
// 值班员看到的未关闭门视图永远不会与事件流出现半写状态。
func (s *Store) Insert(ctx context.Context, in EventInput) (ev Event, created bool, err error) {
	if existing, ferr := s.FindByEventID(ctx, in.EventID); ferr == nil {
		return existing, false, nil
	} else if !errors.Is(ferr, ErrNotFound) {
		return Event{}, false, ferr
	}

	received := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
INSERT INTO events (event_id, door_id, kind, occurred_at, received_at)
VALUES (?, ?, ?, ?, ?)`,
		in.EventID, in.DoorID, in.Kind, in.OccurredAt, received)
	if err != nil {
		// 唯一约束冲突：并发下的重复回调（单连接串行写入，正常走不到，仅作兜底）。
		if isUniqueConflict(err) {
			tx.Rollback()
			existing, ferr := s.FindByEventID(ctx, in.EventID)
			if ferr != nil {
				return Event{}, false, ferr
			}
			return existing, false, nil
		}
		return Event{}, false, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Event{}, false, err
	}

	if err := applyDoorEvent(ctx, tx, in.DoorID, in.Kind, in.OccurredAt, seq); err != nil {
		return Event{}, false, err
	}
	if err := tx.Commit(); err != nil {
		if isUniqueConflict(err) {
			existing, ferr := s.FindByEventID(ctx, in.EventID)
			if ferr != nil {
				return Event{}, false, ferr
			}
			return existing, false, nil
		}
		return Event{}, false, err
	}

	return Event{
		Seq:        seq,
		EventID:    in.EventID,
		DoorID:     in.DoorID,
		Kind:       in.Kind,
		OccurredAt: in.OccurredAt,
		ReceivedAt: received,
	}, true, nil
}

// applyDoorEvent 在已开启的事务里把一条新事件应用到未关闭门视图：
// CLOSED 结束（删除）异常时段；异常告警在无时段时建立时段、有时段时刷新最近信息。
func applyDoorEvent(ctx context.Context, tx *sql.Tx, doorID, kind, occurredAt string, seq int64) error {
	switch kind {
	case KindClosed:
		_, err := tx.ExecContext(ctx, `DELETE FROM door_active WHERE door_id = ?`, doorID)
		return err
	case KindOpenTooLong, KindForcedOpen:
	default:
		// validate 已拒绝其它 kind，防御性忽略。
		return nil
	}

	// 同门已有异常时段：只更新最近类型、最近序号与最近设备时间，开始序号不动。
	res, err := tx.ExecContext(ctx, `
UPDATE door_active
SET last_seq = ?, last_kind = ?, last_occurred_at = ?
WHERE door_id = ?`, seq, kind, occurredAt, doorID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// 首次异常（或 CLOSED 之后再次异常）：建立新时段，开始序号即本条序号。
	_, err = tx.ExecContext(ctx, `
INSERT INTO door_active
	(door_id, start_seq, start_occurred_at, last_seq, last_kind, last_occurred_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		doorID, seq, occurredAt, seq, kind, occurredAt)
	return err
}

// EventsAfter 返回序号严格大于 after 的全部事件，按序号升序——即补发顺序。
func (s *Store) EventsAfter(ctx context.Context, after int64) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT seq, event_id, door_id, kind, occurred_at, received_at
FROM events WHERE seq > ? ORDER BY seq ASC`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		ev, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MaxSeq 返回当前最大序号，空库返回 0。
func (s *Store) MaxSeq(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM events`).Scan(&max); err != nil {
		return 0, err
	}
	return max.Int64, nil
}

// ActiveDoor 是“未关闭门”视图的一行：某扇仍处于异常开启时段的门。
// StartSeq/LastSeq 都直接复用服务端序号；LastOccurredAt 是最近一条告警的设备时间。
type ActiveDoor struct {
	DoorID         string `json:"door_id"`
	StartSeq       int64  `json:"start_seq"`
	LastSeq        int64  `json:"last_seq"`
	LastKind       string `json:"last_kind"`
	LastOccurredAt string `json:"last_occurred_at"`
}

// ActiveDoors 返回所有仍未关闭的门，按异常开始序号升序排列——
// 值班员接班时最先看到最早进入异常的门。空库返回空切片（非 nil）。
func (s *Store) ActiveDoors(ctx context.Context) ([]ActiveDoor, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT door_id, start_seq, last_seq, last_kind, last_occurred_at
FROM door_active
ORDER BY start_seq ASC, door_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActiveDoor{}
	for rows.Next() {
		var d ActiveDoor
		if err := rows.Scan(&d.DoorID, &d.StartSeq, &d.LastSeq, &d.LastKind, &d.LastOccurredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEvent(r rowScanner) (Event, error) {
	var ev Event
	err := r.Scan(&ev.Seq, &ev.EventID, &ev.DoorID, &ev.Kind, &ev.OccurredAt, &ev.ReceivedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

func isUniqueConflict(err error) bool {
	// modernc.org/sqlite 约束错误文本包含 "constraint failed"。
	return err != nil && strings.Contains(err.Error(), "constraint failed")
}
