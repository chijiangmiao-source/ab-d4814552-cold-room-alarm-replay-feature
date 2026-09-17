<script setup>
import { onMounted, onBeforeUnmount, computed, watch } from 'vue'
import { useAlarmStream } from './useAlarmStream.js'
import { useActiveDoors } from './useActiveDoors.js'

const {
  events, connection, lastSeq, lastError, replayDoneAt, completeness, start, stop,
} = useAlarmStream('')

// “未关闭门”只读视图。它与告警流解耦：任何加载失败都只留在本区域，
// 不影响时间线渲染、SSE 重连与序号完整性判断。
const {
  doors: activeDoors, loading: doorsLoading, error: doorsError,
  refresh: refreshDoors, scheduleRefresh: scheduleDoorsRefresh, retry: retryDoors,
} = useActiveDoors('')

onMounted(() => {
  // 接班第一眼就能看到仍异常开启的门，不必等 SSE 补发完成。
  refreshDoors()
  start()
})
onBeforeUnmount(stop)

const connectionMeta = {
  connecting: { text: '连接中…', cls: 'badge-connecting' },
  replaying: { text: '补发断线期间事件…', cls: 'badge-replaying' },
  live: { text: '实时', cls: 'badge-live' },
  disconnected: { text: '已断开，自动重连中', cls: 'badge-disconnected' },
}
const badge = computed(() => connectionMeta[connection.value] ?? connectionMeta.connecting)

const kindMeta = {
  OPEN_TOO_LONG: { text: '开门超时', cls: 'kind-long' },
  FORCED_OPEN: { text: '强制开门', cls: 'kind-forced' },
  CLOSED: { text: '已关闭', cls: 'kind-closed' },
}

function fmtDeviceTime(s) {
  // 设备时间仅用于展示。
  const d = new Date(s)
  return Number.isNaN(d.getTime()) ? s : d.toLocaleString('zh-CN', { hour12: false })
}

const latestFirst = computed(() => [...events.value].sort((a, b) => b.seq - a.seq))

// 只在实时阶段收到新告警时刷新视图；补发期间的密集入库不刷，
// 统一等 replay-done 后校准一次（断线补发完成后再校准）。
watch(
  () => events.value.length,
  () => {
    if (connection.value === 'live') scheduleDoorsRefresh()
  },
)
watch(replayDoneAt, () => {
  refreshDoors()
})
</script>

<template>
  <main class="screen">
    <header class="topbar">
      <h1>冷库门告警中控</h1>
      <div class="status">
        <span class="badge" :class="badge.cls" data-testid="conn-badge">{{ badge.text }}</span>
        <span class="seq-info" data-testid="last-seq">最后序号 #{{ lastSeq }}</span>
        <span
          class="badge"
          :class="completeness.complete ? 'badge-live' : 'badge-disconnected'"
          data-testid="completeness"
        >
          {{ completeness.complete ? '序号连续 · 屏幕完整' : `缺序号: ${completeness.missing.join(', ')}` }}
        </span>
      </div>
    </header>

    <p v-if="lastError" class="error" data-testid="error">{{ lastError }}</p>

    <div class="content">
      <aside class="panel" data-testid="active-panel">
        <div class="panel-head">
          <h2>未关闭门</h2>
          <span
            class="badge"
            :class="activeDoors.length > 0 ? 'badge-disconnected' : 'badge-live'"
            data-testid="active-count"
          >{{ activeDoors.length }}</span>
        </div>

        <div v-if="doorsLoading && activeDoors.length === 0" class="panel-hint" data-testid="active-loading">
          正在加载未关闭门…
        </div>

        <div v-else-if="doorsError" class="panel-error" data-testid="active-error">
          <p>{{ doorsError }}</p>
          <button type="button" class="retry-btn" data-testid="active-retry" @click="retryDoors">
            重试
          </button>
        </div>

        <div v-else-if="activeDoors.length === 0" class="panel-hint" data-testid="active-empty">
          所有冷库门均已关闭
        </div>

        <ul v-else class="active-list">
          <li
            v-for="d in activeDoors"
            :key="d.door_id"
            class="active-door"
            :class="kindMeta[d.last_kind]?.cls"
            data-testid="active-door"
            :data-door-id="d.door_id"
          >
            <div class="active-door-head">
              <span class="active-door-id">{{ d.door_id }}</span>
              <span class="active-kind">{{ kindMeta[d.last_kind]?.text ?? d.last_kind }}</span>
            </div>
            <div class="active-door-meta">
              开始 #{{ d.start_seq }} · 最近 #{{ d.last_seq }}
            </div>
            <div class="active-door-time">设备时间：{{ fmtDeviceTime(d.last_occurred_at) }}</div>
          </li>
        </ul>
      </aside>

      <section class="list">
        <div v-if="latestFirst.length === 0" class="empty" data-testid="empty">
          暂无告警，等待设备网关上报…
        </div>
        <article
          v-for="ev in latestFirst"
          :key="ev.seq"
          class="event"
          :class="kindMeta[ev.kind]?.cls"
          :data-seq="ev.seq"
          :data-event-id="ev.event_id"
        >
          <div class="event-head">
            <span class="seq">#{{ ev.seq }}</span>
            <span class="kind" data-testid="kind">{{ kindMeta[ev.kind]?.text ?? ev.kind }}</span>
            <span class="door" data-testid="door">{{ ev.door_id }}</span>
          </div>
          <div class="event-body">
            <span>设备时间：{{ fmtDeviceTime(ev.occurred_at) }}</span>
            <span class="event-id">event_id: {{ ev.event_id }}</span>
          </div>
        </article>
      </section>
    </div>
  </main>
</template>

<style>
:root {
  color-scheme: dark;
}
* { box-sizing: border-box; }
body {
  margin: 0;
  font-family: -apple-system, "PingFang SC", "Microsoft YaHei", sans-serif;
  background: #0e1420;
  color: #e6edf3;
}
.screen { max-width: 1180px; margin: 0 auto; padding: 24px; }
.topbar {
  display: flex; justify-content: space-between; align-items: center;
  flex-wrap: wrap; gap: 12px; border-bottom: 1px solid #243044; padding-bottom: 16px;
}
h1 { font-size: 22px; margin: 0; }
.status { display: flex; gap: 10px; align-items: center; }
.badge {
  padding: 4px 12px; border-radius: 999px; font-size: 13px; font-weight: 600;
}
.badge-connecting { background: #33415c; color: #c6d2e1; }
.badge-replaying { background: #7c5e10; color: #ffe9a8; }
.badge-live { background: #14532d; color: #b7f7c8; }
.badge-disconnected { background: #7f1d1d; color: #fecaca; animation: blink 1s infinite; }
@keyframes blink { 50% { opacity: 0.55; } }
.seq-info { font-variant-numeric: tabular-nums; color: #93a4bb; font-size: 13px; }
.error { color: #fca5a5; margin: 12px 0; }

.content { display: flex; gap: 18px; align-items: flex-start; margin-top: 18px; }
.panel {
  flex: 0 0 300px; position: sticky; top: 16px;
  border: 1px solid #243044; border-radius: 10px; background: #141c2b;
  padding: 14px 16px;
}
.panel-head { display: flex; align-items: center; justify-content: space-between; }
.panel-head h2 { font-size: 16px; margin: 0; }
.panel-hint { color: #7184a0; font-size: 13px; padding: 18px 0; text-align: center; }
.panel-error { padding: 10px 0; }
.panel-error p { color: #fca5a5; font-size: 13px; margin: 0 0 10px; }
.retry-btn {
  background: #33415c; color: #e6edf3; border: 1px solid #4b5d7a;
  border-radius: 6px; padding: 5px 14px; font-size: 13px; cursor: pointer;
}
.retry-btn:hover { background: #40506d; }
.active-list { list-style: none; margin: 12px 0 0; padding: 0; display: flex; flex-direction: column; gap: 10px; }
.active-door {
  border: 1px solid #243044; border-left-width: 4px; border-radius: 8px;
  padding: 9px 12px; background: #0e1420;
}
.active-door-head { display: flex; justify-content: space-between; align-items: baseline; gap: 8px; }
.active-door-id { font-weight: 700; color: #e6edf3; }
.active-kind { font-size: 13px; font-weight: 600; }
.active-door-meta { margin-top: 4px; font-size: 12px; color: #93a4bb; font-variant-numeric: tabular-nums; }
.active-door-time { margin-top: 2px; font-size: 12px; color: #7184a0; }
.active-door.kind-long { border-left-color: #f59e0b; }
.active-door.kind-forced { border-left-color: #ef4444; }
.active-door.kind-long .active-kind { color: #fbbf24; }
.active-door.kind-forced .active-kind { color: #f87171; }

.empty { color: #7184a0; text-align: center; padding: 60px 0; }
.list { flex: 1; min-width: 0; display: flex; flex-direction: column; gap: 10px; }
.event {
  border: 1px solid #243044; border-left-width: 4px; border-radius: 10px;
  padding: 12px 16px; background: #141c2b;
}
.event-head { display: flex; gap: 14px; align-items: baseline; }
.seq { font-weight: 700; color: #7dd3fc; font-variant-numeric: tabular-nums; }
.kind { font-weight: 700; }
.door { color: #b8c6db; font-size: 14px; }
.event-body {
  margin-top: 6px; display: flex; justify-content: space-between;
  color: #8fa3bd; font-size: 13px; flex-wrap: wrap; gap: 6px;
}
.event-id { font-family: ui-monospace, monospace; }
.kind-long { border-left-color: #f59e0b; }
.kind-forced { border-left-color: #ef4444; }
.kind-closed { border-left-color: #22c55e; }
.kind-long .kind { color: #fbbf24; }
.kind-forced .kind { color: #f87171; }
.kind-closed .kind { color: #4ade80; }

@media (max-width: 760px) {
  .content { flex-direction: column; }
  .panel { flex: none; width: 100%; position: static; }
}
</style>
