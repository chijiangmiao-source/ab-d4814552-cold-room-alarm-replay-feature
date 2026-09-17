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
	// 从既有事件回算“未关闭门”视图，保证重启后初始状态与事件流一致。
	if err := s.RebuildActiveDoors(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS events (
	seq         INTEGER PRIMARY KEY AUTOINCREMENT,
	event_id    TEXT NOT NULL UNIQUE,
	door_id     TEXT NOT NULL,
	kind        TEXT NOT NULL,
	occurred_at TEXT NOT NULL,
	received_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_seq ON events(seq);

-- door_active 每个门至多一行：存在即表示该门有一段“尚未关闭”的异常时段。
-- start_seq 是首次接受的 OPEN_TOO_LONG/FORCED_OPEN 序号；同门后续告警只刷新
-- last_*；CLOSED 删除该行。重启后由 events 全量回算，不依赖本表持久状态。
CREATE TABLE IF NOT EXISTS door_active (
	door_id          TEXT PRIMARY KEY,
	start_seq        INTEGER NOT NULL,
	last_seq         INTEGER NOT NULL,
	last_kind        TEXT NOT NULL,
	last_occurred_at TEXT NOT NULL
);
`)
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

// findEventByEventIDTx 是事务内版本，供 Insert 在同一事务里做幂等检查。
func (s *Store) findEventByEventIDTx(ctx context.Context, tx *sql.Tx, eventID string) (Event, error) {
	row := tx.QueryRowContext(ctx, `
SELECT seq, event_id, door_id, kind, occurred_at, received_at
FROM events WHERE event_id = ?`, eventID)
	return scanEvent(row)
}

// ActiveDoor 是“未关闭门”视图中的一行：一段尚未被 CLOSED 结束的异常时段。
// StartSeq 为异常开始时（首个被接受的 OPEN_TOO_LONG/FORCED_OPEN）的服务端序号，
// LastSeq/LastKind/LastOccurredAt 来自该门最近一次被接受的异常告警。
type ActiveDoor struct {
	DoorID         string `json:"door_id"`
	StartSeq       int64  `json:"start_seq"`
	LastSeq        int64  `json:"last_seq"`
	LastKind       string `json:"last_kind"`
	LastOccurredAt string `json:"last_occurred_at"`
}

// ActiveDoors 返回所有仍处于异常开启状态的门，按异常开始序号升序排列，
// 序号并列时按门号兜底，保证接口返回顺序稳定可断言。
func (s *Store) ActiveDoors(ctx context.Context) ([]ActiveDoor, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT door_id, start_seq, last_seq, last_kind, last_occurred_at
FROM door_active
ORDER BY start_seq ASC, door_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveDoor
	for rows.Next() {
		var d ActiveDoor
		if err := rows.Scan(&d.DoorID, &d.StartSeq, &d.LastSeq, &d.LastKind, &d.LastOccurredAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Insert 写入一条新告警并返回带服务端序号的完整记录。
// 返回 created=false 表示该 event_id 已存在（重复回调或唯一约束冲突），
// 此时返回的是既有记录，不新增行、不分配新序号，也不改变“未关闭门”状态。
//
// 首次接受的事件与其门状态变更在同一个 SQLite 事务内完成：
//   - OPEN_TOO_LONG / FORCED_OPEN：该门无异常时段时开段（start_seq 取本事件
//     序号），已有异常时段时只刷新最近类型/序号/设备时间；
//   - CLOSED：结束该门当前异常时段（没有进行中的时段则什么都不做）。
func (s *Store) Insert(ctx context.Context, in EventInput) (ev Event, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, err
	}
	defer func() {
		if err != nil {
			tx.Rollback()
		}
	}()

	if existing, ferr := s.findEventByEventIDTx(ctx, tx, in.EventID); ferr == nil {
		// 重复回调：原结果不变，门状态也不变。
		if err = tx.Commit(); err != nil {
			return Event{}, false, err
		}
		return existing, false, nil
	} else if !errors.Is(ferr, ErrNotFound) {
		return Event{}, false, ferr
	}

	received := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `
INSERT INTO events (event_id, door_id, kind, occurred_at, received_at)
VALUES (?, ?, ?, ?, ?)`,
		in.EventID, in.DoorID, in.Kind, in.OccurredAt, received)
	if err != nil {
		// 唯一约束冲突：并发下的重复回调（单连接串行写入，正常走不到，仅作兜底）。
		if isUniqueConflict(err) {
			if rerr := tx.Rollback(); rerr != nil {
				return Event{}, false, rerr
			}
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
	ev = Event{
		Seq:        seq,
		EventID:    in.EventID,
		DoorID:     in.DoorID,
		Kind:       in.Kind,
		OccurredAt: in.OccurredAt,
		ReceivedAt: received,
	}

	if err = s.applyDoorStateTx(ctx, tx, ev); err != nil {
		return Event{}, false, err
	}

	if err = tx.Commit(); err != nil {
		return Event{}, false, err
	}
	return ev, true, nil
}

// applyDoorStateTx 在插入事件的同一事务内推进门异常时段。
func (s *Store) applyDoorStateTx(ctx context.Context, tx *sql.Tx, ev Event) error {
	switch ev.Kind {
	case KindOpenTooLong, KindForcedOpen:
		// 已有异常时段：只更新最近类型/序号/设备时间，开始序号保持不变。
		res, err := tx.ExecContext(ctx, `
UPDATE door_active SET last_seq = ?, last_kind = ?, last_occurred_at = ?
WHERE door_id = ?`, ev.Seq, ev.Kind, ev.OccurredAt, ev.DoorID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		// 首个被接受的异常告警：建立异常时段。
		_, err = tx.ExecContext(ctx, `
INSERT INTO door_active (door_id, start_seq, last_seq, last_kind, last_occurred_at)
VALUES (?, ?, ?, ?, ?)`,
			ev.DoorID, ev.Seq, ev.Seq, ev.Kind, ev.OccurredAt)
		return err
	case KindClosed:
		// CLOSED 结束当前异常时段；没有进行中的时段则忽略。
		_, err := tx.ExecContext(ctx, `DELETE FROM door_active WHERE door_id = ?`, ev.DoorID)
		return err
	default:
		// kind 已在入口校验，走到这里说明漏了新类型——宁可报错也不静默。
		return fmt.Errorf("applyDoorStateTx: unknown kind %q", ev.Kind)
	}
}

// RebuildActiveDoors 清空并按 seq 升序回放全部事件，回算“未关闭门”视图。
// 回算规则与实时写入完全一致：首个未配对的 OPEN_TOO_LONG/FORCED_OPEN 开段，
// 同门后续告警刷新最近类型/序号，CLOSED 闭段；重复 event_id 本就只存一份，
// 因此回算天然幂等。
func (s *Store) RebuildActiveDoors(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM door_active`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `
SELECT seq, door_id, kind, occurred_at FROM events ORDER BY seq ASC`)
	if err != nil {
		return err
	}
	type applied struct {
		Seq        int64
		DoorID     string
		Kind       string
		OccurredAt string
	}
	var pending []applied
	for rows.Next() {
		var a applied
		if err := rows.Scan(&a.Seq, &a.DoorID, &a.Kind, &a.OccurredAt); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, a := range pending {
		ev := Event{Seq: a.Seq, DoorID: a.DoorID, Kind: a.Kind, OccurredAt: a.OccurredAt}
		if err := s.applyDoorStateTx(ctx, tx, ev); err != nil {
			return err
		}
	}
	return tx.Commit()
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
