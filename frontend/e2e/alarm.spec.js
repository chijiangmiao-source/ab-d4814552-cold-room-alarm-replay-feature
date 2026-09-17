// @ts-check
import { test, expect, request as pwRequest } from '@playwright/test'

// 每次验收运行用独立 door_id 标记，页面只统计本次运行产生的事件，
// 这样即使数据库里有历史数据，序号连续性断言依然成立。
const RUN = `e2e-${Date.now()}`
const TS = '2026-09-14T22:00:00Z'

function body(eventId, kind = 'CLOSED', extra = {}) {
  return { event_id: eventId, door_id: RUN, kind, occurred_at: TS, ...extra }
}

async function postEvent(api, eventId, kind, extra) {
  const res = await api.post('/api/events', { data: body(eventId, kind, extra) })
  const json = await res.json()
  return { status: res.status(), dedup: res.headers()['x-deduplicated'], ev: json }
}

async function waitForEventId(page, eventId) {
  const loc = page.locator(`article.event[data-event-id="${eventId}"]`)
  await expect(loc).toHaveCount(1, { timeout: 15_000 })
  return loc
}

async function openConsole(page) {
  await page.goto('/')
  await expect(page.getByTestId('conn-badge')).toBeVisible()
}

test.describe.configure({ mode: 'serial' })

test.beforeAll(async ({ baseURL }) => {
  // 健康检查，确保链路可达。
  const api = await pwRequest.newContext({ baseURL })
  const res = await api.get('/healthz')
  expect(res.status()).toBe(200)
  await api.dispose()
})

test('设备回调的新告警实时出现在页面上', async ({ page, request }) => {
  await openConsole(page)
  const { ev } = await postEvent(request, `${RUN}-live-1`, 'FORCED_OPEN')
  expect(ev.seq).toBeGreaterThan(0)

  const row = await waitForEventId(page, `${RUN}-live-1`)
  await expect(row).toContainText(`#${ev.seq}`)
  await expect(row).toContainText('强制开门')
  await expect(page.getByTestId('last-seq')).toContainText(`#${ev.seq}`)
  await expect(page.getByTestId('conn-badge')).toHaveText('实时')
})

test('非法 JSON、缺字段、非法 kind 返回 400 且不分配序号', async ({ request }) => {
  const api = request
  const bad = [
    '{not json',
    JSON.stringify({ door_id: RUN, kind: 'CLOSED', occurred_at: TS }),                 // 缺 event_id
    JSON.stringify({ event_id: `${RUN}-x`, kind: 'CLOSED', occurred_at: TS }),        // 缺 door_id
    JSON.stringify({ event_id: `${RUN}-x`, door_id: RUN, kind: 'CLOSED' }),           // 缺 occurred_at
    JSON.stringify({ event_id: `${RUN}-x`, door_id: RUN, kind: 'OPEN', occurred_at: TS }), // 非法 kind
  ]
  for (const payload of bad) {
    const res = await api.post('/api/events', {
      data: payload,
      headers: { 'content-type': 'application/json' },
    })
    expect(res.status(), `payload=${payload}`).toBe(400)
    const json = await res.json()
    expect(json.error).toBeTruthy()
  }

  // 前一条合法事件与本条之间夹了 5 个非法请求，序号必须仍紧接 +1。
  const before = await postEvent(request, `${RUN}-seq-before`, 'CLOSED')
  const after = await postEvent(request, `${RUN}-seq-after`, 'CLOSED')
  expect(after.ev.seq).toBe(before.ev.seq + 1)
})

test('相同 event_id 的重复回调返回既有记录，页面只显示一次', async ({ page, request }) => {
  await openConsole(page)
  const first = await postEvent(request, `${RUN}-dup`, 'OPEN_TOO_LONG')
  await waitForEventId(page, `${RUN}-dup`)

  const second = await postEvent(request, `${RUN}-dup`, 'FORCED_OPEN', {
    occurred_at: '2026-09-14T23:59:00Z', // 字段不同也必须返回既有记录
  })
  expect(second.status).toBe(200)
  expect(second.dedup).toBe('true')
  expect(second.ev.seq).toBe(first.ev.seq)
  expect(second.ev.kind).toBe('OPEN_TOO_LONG')

  await expect(page.locator(`article.event[data-event-id="${RUN}-dup"]`)).toHaveCount(1)
})

test('断线期间连续提交与重复提交，重连后序号连续且每条恰好一次', async ({ browser, request }) => {
  // 独立上下文，先看到两条基线事件。
  const ctx = await browser.newContext()
  const page = await ctx.newPage()
  await openConsole(page)

  const base1 = await postEvent(request, `${RUN}-gap-base-1`, 'CLOSED')
  const base2 = await postEvent(request, `${RUN}-gap-base-2`, 'CLOSED')
  await waitForEventId(page, `${RUN}-gap-base-2`)
  await expect(page.getByTestId('last-seq')).toContainText(`#${base2.ev.seq}`)

  // 主动断开页面（只断浏览器，Node 侧继续向 API 提交，模拟中控断网但网关照常上报）。
  await ctx.setOffline(true)
  const offline = [
    await postEvent(request, `${RUN}-gap-a`, 'FORCED_OPEN'),
    await postEvent(request, `${RUN}-gap-b`, 'OPEN_TOO_LONG'),
    await postEvent(request, `${RUN}-gap-c`, 'CLOSED'),
  ]
  // 断线期间的重复回调不得占号。
  const dupAgain = await postEvent(request, `${RUN}-gap-a`, 'CLOSED')
  expect(dupAgain.dedup).toBe('true')
  expect(dupAgain.ev.seq).toBe(offline[0].ev.seq)

  // 给断线态一点时间，再恢复网络，页面应自动带最后序号重连并补发。
  await page.waitForTimeout(1500)
  await ctx.setOffline(false)

  for (const { ev } of offline) {
    const row = await waitForEventId(page, ev.event_id)
    await expect(row).toContainText(`#${ev.seq}`)
  }
  await expect(page.getByTestId('conn-badge')).toHaveText('实时', { timeout: 20_000 })

  // 序号严格连续：基线 +1/+2/+3，无空洞。
  expect(offline[0].ev.seq).toBe(base2.ev.seq + 1)
  expect(offline[1].ev.seq).toBe(base2.ev.seq + 2)
  expect(offline[2].ev.seq).toBe(base2.ev.seq + 3)

  // 每条恰好一次。
  for (const id of [`${RUN}-gap-a`, `${RUN}-gap-b`, `${RUN}-gap-c`]) {
    await expect(page.locator(`article.event[data-event-id="${id}"]`)).toHaveCount(1)
  }

  // 屏幕完整性自检为“连续”，且最后序号追上最新事件。
  await expect(page.getByTestId('completeness')).toContainText('序号连续')
  await expect(page.getByTestId('last-seq')).toContainText(`#${offline[2].ev.seq}`)

  await ctx.close()
})

test('新页面打开时从序号 0 完整补发历史', async ({ browser, request }) => {
  const ids = [`${RUN}-hist-1`, `${RUN}-hist-2`, `${RUN}-hist-3`]
  const seqs = []
  for (const id of ids) {
    const { ev } = await postEvent(request, id, 'CLOSED')
    seqs.push(ev.seq)
  }

  const ctx = await browser.newContext()
  const page = await ctx.newPage()
  await openConsole(page)

  for (const id of ids) {
    await waitForEventId(page, id)
  }
  await expect(page.getByTestId('last-seq')).toContainText(`#${seqs[2]}`)
  await expect(page.getByTestId('completeness')).toContainText('序号连续')
  await ctx.close()
})

// “未关闭门”视图：两门告警 → 关闭其一 → 断线补发，面板始终只剩另一门，
// 时间线每条告警仍只出现一次。
test('未关闭门面板随实时告警与断线补发更新，关闭一门后只剩另一门', async ({ browser, request }) => {
  const ctx = await browser.newContext()
  const page = await ctx.newPage()
  await openConsole(page)

  const door1 = `${RUN}-panel-A`
  const door2 = `${RUN}-panel-B`
  const door3 = `${RUN}-panel-gap`
  const panelDoor = (id) => page.locator(`[data-testid="active-door"][data-door-id="${id}"]`)

  // 两门先后告警，页面在线：实时收到告警后自动刷新面板。
  const a = await postEvent(request, `${RUN}-panel-open-a`, 'OPEN_TOO_LONG', { door_id: door1 })
  const b = await postEvent(request, `${RUN}-panel-open-b`, 'FORCED_OPEN', { door_id: door2 })
  await waitForEventId(page, `${RUN}-panel-open-b`)

  await expect(page.getByTestId('active-count')).toHaveText('2')
  const rows = page.getByTestId('active-door')
  await expect(rows).toHaveCount(2)
  // 按异常开始序号排列：door1 在前。
  await expect(rows.first()).toHaveAttribute('data-door-id', door1)
  await expect(panelDoor(door1)).toContainText('开门超时')
  await expect(panelDoor(door1)).toContainText(`开始 #${a.ev.seq}`)
  await expect(panelDoor(door2)).toContainText('强制开门')
  await expect(panelDoor(door2)).toContainText(`开始 #${b.ev.seq}`)

  // 关闭其中一门：实时刷新后面板只剩另一门。
  const closeA = await postEvent(request, `${RUN}-panel-close-a`, 'CLOSED', { door_id: door1 })
  await waitForEventId(page, `${RUN}-panel-close-a`)
  await expect(panelDoor(door1)).toHaveCount(0)
  await expect(panelDoor(door2)).toHaveCount(1)
  await expect(page.getByTestId('active-count')).toHaveText('1')

  // 模拟中控断网：断线期间让 door2 再报一次（类型变化、序号推进），
  // 另有一门在断线窗口内“开了又关”，补发校准后不得残留在面板。
  await ctx.setOffline(true)
  await page.waitForTimeout(1500)
  const gap = [
    await postEvent(request, `${RUN}-panel-gap-update`, 'OPEN_TOO_LONG', { door_id: door2 }),
    await postEvent(request, `${RUN}-panel-gap-open`, 'FORCED_OPEN', { door_id: door3 }),
    await postEvent(request, `${RUN}-panel-gap-close`, 'CLOSED', { door_id: door3 }),
  ]
  await ctx.setOffline(false)

  // 断线补发完成：连接状态回到实时，最后序号追上。
  await expect(page.getByTestId('conn-badge')).toHaveText('实时', { timeout: 20_000 })
  for (const { ev } of gap) {
    await waitForEventId(page, ev.event_id)
  }
  await expect(page.getByTestId('last-seq')).toContainText(`#${gap[2].ev.seq}`)

  // 面板校准：只剩 door2，且开始序号仍是最早那次、最近类型/序号已更新。
  await expect(page.getByTestId('active-count')).toHaveText('1')
  await expect(page.getByTestId('active-door')).toHaveCount(1)
  await expect(panelDoor(door1)).toHaveCount(0)
  await expect(panelDoor(door3)).toHaveCount(0)
  await expect(panelDoor(door2)).toHaveCount(1)
  await expect(panelDoor(door2)).toContainText('开门超时')
  await expect(panelDoor(door2)).toContainText(`开始 #${b.ev.seq}`)
  await expect(panelDoor(door2)).toContainText(`最近 #${gap[0].ev.seq}`)

  // 时间线完整性：本场景每条告警恰好一张卡片，序号连续、无缺号。
  const scenarioIds = [
    `${RUN}-panel-open-a`, `${RUN}-panel-open-b`, `${RUN}-panel-close-a`,
    `${RUN}-panel-gap-update`, `${RUN}-panel-gap-open`, `${RUN}-panel-gap-close`,
  ]
  for (const id of scenarioIds) {
    await expect(page.locator(`article.event[data-event-id="${id}"]`)).toHaveCount(1)
  }
  await expect(page.getByTestId('completeness')).toContainText('序号连续')

  // 收尾：关掉最后一扇门，保证本运行不留残余，便于在同一数据库上重复执行。
  const cleanup = await postEvent(request, `${RUN}-panel-cleanup`, 'CLOSED', { door_id: door2 })
  await waitForEventId(page, `${RUN}-panel-cleanup`)
  await expect(page.getByTestId('last-seq')).toContainText(`#${cleanup.ev.seq}`)
  await expect(panelDoor(door2)).toHaveCount(0)
  await expect(page.getByTestId('active-count')).toHaveText('0')

  await ctx.close()
})

// 快照加载失败只影响面板本身：时间线告警流与完整性判断照常工作，
// 点击“重试”且接口恢复后面板回到正常。
test('未关闭门快照加载失败时只在面板提示并可重试，不中断告警流', async ({ browser, request }) => {
  const ctx = await browser.newContext()
  const page = await ctx.newPage()

  // 打开页面之前先拦截只读快照接口：主动失败，SSE 通道不受影响。
  await page.route('**/api/doors/active', (route) => route.abort('failed'))
  await page.goto('/')
  await expect(page.getByTestId('conn-badge')).toBeVisible()

  await expect(page.getByTestId('active-error')).toBeVisible({ timeout: 15_000 })
  await expect(page.getByTestId('active-error')).toContainText('未关闭门视图加载失败')
  // 页面级错误条不应出现——告警流没被打断。
  await expect(page.getByTestId('error')).toHaveCount(0)

  // 快照接口挂掉期间，实时告警与完整性自检照常工作。
  const doorX = `${RUN}-panel-fail`
  const { ev } = await postEvent(request, `${RUN}-panel-fail-open`, 'FORCED_OPEN', { door_id: doorX })
  const row = await waitForEventId(page, `${RUN}-panel-fail-open`)
  await expect(row).toContainText(`#${ev.seq}`)
  await expect(page.getByTestId('last-seq')).toContainText(`#${ev.seq}`)
  await expect(page.getByTestId('completeness')).toContainText('序号连续')
  // 面板仍在报错，没有伪装成数据。
  await expect(page.getByTestId('active-error')).toBeVisible()

  // 接口恢复后点重试：面板正常显示该门，错误消失。
  await page.unroute('**/api/doors/active')
  await page.getByTestId('active-retry').click()
  await expect(page.getByTestId('active-error')).toHaveCount(0)
  await expect(page.locator(`[data-testid="active-door"][data-door-id="${doorX}"]`)).toBeVisible()

  // 收尾关门，保持数据库干净。
  const cleanup = await postEvent(request, `${RUN}-panel-fail-close`, 'CLOSED', { door_id: doorX })
  await waitForEventId(page, `${RUN}-panel-fail-close`)
  await expect(page.getByTestId('last-seq')).toContainText(`#${cleanup.ev.seq}`)

  await ctx.close()
})
