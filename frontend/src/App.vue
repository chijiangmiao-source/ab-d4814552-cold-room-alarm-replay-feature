<script setup>
import { onMounted, onBeforeUnmount, computed } from 'vue'
import { useAlarmStream } from './useAlarmStream.js'
import { useActiveDoors } from './useActiveDoors.js'

const {
  doors, loading: doorsLoading, error: doorsError, refresh: refreshDoors, retry: retryDoors,
} = useActiveDoors('')

const {
  events, connection, lastSeq, lastError, completeness, start, stop,
} = useAlarmStream('', {
  // 收到实时新告警后刷新未关闭门视图。
  onAlarm: () => { refreshDoors() },
  // 断线补发完成后再校准一次（初始全量补发也走这里，顺带拿到初始视图）。
  onReplayDone: () => { refreshDoors() },
})

onMounted(() => {
  start()
  // 接班时立即加载一次未关闭门视图：即使 SSE 尚未完成补发，
  // 值班员也能先看到当前仍未关闭的门。
  refreshDoors()
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

    <div class="layout">
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

      <!-- 未关闭门视图：值班员接班时与时间线并列，一眼看到仍异常开启的门。 -->
      <aside class="doors-panel" data-testid="doors-panel">
        <div class="doors-head">
          <h2>未关闭门</h2>
          <span
            class="badge"
            :class="doors.length ? 'badge-disconnected' : 'badge-live'"
            data-testid="doors-count"
          >{{ doors.length }} 扇</span>
        </div>

        <div v-if="doorsLoading" class="doors-msg" data-testid="doors-loading">
          正在加载未关闭门…
        </div>

        <!-- 首次加载就失败：面板内提示并允许重试。 -->
        <div v-else-if="doorsError && doors.length === 0" class="doors-error" data-testid="doors-error">
          <p>{{ doorsError }}</p>
          <button type="button" class="retry-btn" data-testid="doors-retry" @click="retryDoors">
            重试
          </button>
        </div>

        <template v-else>
          <!-- 已有快照后刷新失败：保留旧列表，只在面板顶部提示。 -->
          <div v-if="doorsError" class="doors-banner" data-testid="doors-error">
            <span>{{ doorsError }}</span>
            <button type="button" class="retry-link" data-testid="doors-retry" @click="retryDoors">
              重试
            </button>
          </div>

          <div v-if="doors.length === 0" class="doors-msg" data-testid="doors-empty">
            当前没有异常开启的门
          </div>

          <ul v-else class="doors-list">
            <li
              v-for="d in doors"
              :key="d.door_id"
              class="door-row"
              :class="kindMeta[d.last_kind]?.cls"
              :data-door-id="d.door_id"
            >
              <div class="door-row-head">
                <span class="door-name" data-testid="active-door-id">{{ d.door_id }}</span>
                <span class="kind" data-testid="active-door-kind">
                  {{ kindMeta[d.last_kind]?.text ?? d.last_kind }}
                </span>
              </div>
              <div class="door-seqs">
                异常开始 #{{ d.start_seq }} · 最近 #{{ d.last_seq }}
              </div>
              <div class="door-time">最近设备时间：{{ fmtDeviceTime(d.last_occurred_at) }}</div>
            </li>
          </ul>
        </template>
      </aside>
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
.screen { max-width: 1240px; margin: 0 auto; padding: 24px; }
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
.empty { color: #7184a0; text-align: center; padding: 60px 0; }

.layout { display: grid; grid-template-columns: 1fr 340px; gap: 18px; margin-top: 18px; align-items: start; }
@media (max-width: 900px) {
  .layout { grid-template-columns: 1fr; }
}

.list { display: flex; flex-direction: column; gap: 10px; min-width: 0; }
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

.doors-panel {
  border: 1px solid #243044; border-radius: 12px; background: #141c2b;
  padding: 14px 16px; position: sticky; top: 16px;
}
.doors-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 10px; }
.doors-head h2 { font-size: 16px; margin: 0; }
.doors-msg { color: #7184a0; font-size: 13px; padding: 24px 0; text-align: center; }
.doors-error { color: #fca5a5; font-size: 13px; }
.doors-error p { margin: 8px 0; }
.doors-banner {
  display: flex; justify-content: space-between; align-items: center; gap: 8px;
  background: #2a1a1a; border: 1px solid #7f1d1d; border-radius: 8px;
  color: #fca5a5; font-size: 12px; padding: 6px 10px; margin-bottom: 10px;
}
.retry-link {
  background: none; border: none; color: #fca5a5; text-decoration: underline;
  cursor: pointer; font-size: 12px; padding: 0; flex: none;
}
.retry-btn {
  background: #7f1d1d; color: #fecaca; border: 1px solid #b91c1c; border-radius: 8px;
  padding: 6px 18px; font-size: 13px; cursor: pointer; font-weight: 600;
}
.retry-btn:hover { background: #991b1b; }
.doors-list { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 10px; }
.door-row {
  border: 1px solid #243044; border-left-width: 4px; border-radius: 10px;
  padding: 10px 12px; background: #0f1726;
}
.door-row-head { display: flex; justify-content: space-between; align-items: baseline; gap: 8px; }
.door-name { font-weight: 700; font-size: 15px; color: #e6edf3; }
.door-seqs { margin-top: 6px; color: #93a4bb; font-size: 12px; font-variant-numeric: tabular-nums; }
.door-time { margin-top: 2px; color: #7184a0; font-size: 12px; }
</style>
