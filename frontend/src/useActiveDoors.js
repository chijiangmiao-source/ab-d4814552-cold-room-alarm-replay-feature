// useActiveDoors 拉取“未关闭门”只读快照：GET /api/doors/active。
//
// 与告警流刻意解耦：
//   - 快照加载/刷新失败只在本区域提示并允许重试，绝不影响 SSE 告警流与
//     序号完整性判断；
//   - 触发时机由外部决定：实时告警到达后刷新，断线补发完成（replay-done）后
//     再校准一次；
//   - 同一时刻的连续触发合并为一次请求，避免告警风暴时打爆只读接口。
import { ref } from 'vue'

export function useActiveDoors(baseUrl = '') {
  const doors = ref([])          // 最近一次成功加载的快照（按 start_seq 升序）
  const loading = ref(false)     // 首次加载尚未结束
  const error = ref('')          // 最近一次刷新失败的提示；成功后清空

  let requestSeq = 0             // 只采纳最后一次请求的结果
  let flushScheduled = false

  async function refresh() {
    const mySeq = ++requestSeq
    if (doors.value.length === 0) loading.value = true
    try {
      const resp = await fetch(`${baseUrl}/api/doors/active`, {
        headers: { Accept: 'application/json' },
        cache: 'no-store',
      })
      if (!resp.ok) {
        // 沿用后端统一错误结构 {"error": "..."}。
        let msg = `HTTP ${resp.status}`
        try {
          const body = await resp.json()
          if (body && body.error) msg = body.error
        } catch {
          // 非 JSON 错误响应时退回状态码提示。
        }
        throw new Error(msg)
      }
      const data = await resp.json()
      if (mySeq !== requestSeq) return // 已有更新的请求在路上，丢弃过期结果
      doors.value = Array.isArray(data) ? data : []
      error.value = ''
    } catch (err) {
      if (mySeq !== requestSeq) return
      error.value = `未关闭门视图加载失败：${err.message}`
    } finally {
      if (mySeq === requestSeq) loading.value = false
    }
  }

  // scheduleRefresh 把同一轮事件循环内的多次触发合并为一次刷新。
  function scheduleRefresh() {
    if (flushScheduled) return
    flushScheduled = true
    queueMicrotask(() => {
      flushScheduled = false
      refresh()
    })
  }

  function retry() {
    error.value = ''
    refresh()
  }

  return { doors, loading, error, refresh, scheduleRefresh, retry }
}
