// useActiveDoors 拉取并维护“未关闭门”快照（只读 GET /api/doors/active）。
//
// 与事件流刻意解耦：快照加载/刷新失败只在面板区域提示并允许重试，
// 不影响 SSE 告警接收，也不影响序号完整性判断。
//
// 刷新时机由调用方在实时告警到达、断线补发完成（replay-done）时触发。
// 短时间内多次触发只产生“在途一次 + 结束后补一次”，避免突发告警打爆接口，
// 且最后一次请求一定在所有在途请求之后结束，视图不会被旧响应覆盖。
//
// 失败后会自动按固定间隔重试（例如网络恢复瞬间补发已成功、快照请求却落空），
// 值班员也可以点“重试”立即再来一次。

import { ref, onBeforeUnmount } from 'vue'

const retryDelayMs = 2000

export function useActiveDoors(baseUrl = '') {
  const doors = ref([])          // ActiveDoor[]，后端已按异常开始序号排好
  const loading = ref(false)    // 仅首次加载（无旧数据可展示）时为 true
  const error = ref('')

  let inflight = false
  let dirty = false
  let retryTimer = null

  function clearRetry() {
    if (retryTimer) {
      clearTimeout(retryTimer)
      retryTimer = null
    }
  }

  function scheduleRetry() {
    if (retryTimer) return
    retryTimer = setTimeout(() => {
      retryTimer = null
      refresh()
    }, retryDelayMs)
  }

  async function refresh() {
    if (inflight) {
      // 已有请求在途：标记脏位，由其在收尾时再拉一次最新状态。
      dirty = true
      return
    }
    clearRetry()
    inflight = true
    loading.value = doors.value.length === 0
    try {
      const res = await fetch(`${baseUrl}/api/doors/active`, {
        headers: { Accept: 'application/json' },
      })
      if (!res.ok) {
        let msg = `未关闭门快照加载失败（HTTP ${res.status}）`
        try {
          const body = await res.json()
          if (body && body.error) msg = `未关闭门快照加载失败：${body.error}`
        } catch {
          // 错误体不是 JSON 也无妨，沿用默认提示。
        }
        throw new Error(msg)
      }
      const data = await res.json()
      // 只有成功才整体替换；失败时保留上一份快照作为参考。
      doors.value = Array.isArray(data) ? data : []
      error.value = ''
    } catch (err) {
      // 网络断开/请求被中断时浏览器抛 TypeError（文案是英文 "Failed to fetch"），
      // 统一成中文提示；HTTP 错误已在上面拼好后端 error。
      error.value = err instanceof TypeError
        ? '未关闭门快照加载失败：网络连接失败'
        : (err && err.message) || '未关闭门快照加载失败'
      scheduleRetry()
    } finally {
      inflight = false
      loading.value = false
      if (dirty && !error.value) {
        dirty = false
        refresh()
      } else if (dirty) {
        // 收尾请求失败、已安排自动重试时，脏位交给下一次重试补上。
        dirty = false
      }
    }
  }

  function retry() {
    refresh()
  }

  onBeforeUnmount(clearRetry)

  return { doors, loading, error, refresh, retry }
}
