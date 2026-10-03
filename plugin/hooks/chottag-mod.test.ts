// Tests for chottag's mod, run by `claude plugin test plugin` (a maintainer
// check before a release: the repo's gate never runs the real claude). They
// drive the module with a fake `$`, so no chottag, network or session is used.
import { expect, test } from 'claude-code/testing'
import { CT_HELP, GUARD_LOGIN, GUARD_LOGOUT, bandModel, displayWidth, formatReset, layoutCard, parseCt, register } from './chottag-mod.js'

const ROUTED = 'http://chottag.default.0123456789abcdef0123456789abcdef:x@127.0.0.1:47850'
const HOUR = 3600000

// doc is a `chottag statusline --json` document for a routed session.
function doc(extra: any = {}): any {
  return {
    ok: true, session: 'routed', daemon: 'up', serving: 'C', account: 'C',
    fiveHourPct: 28.2, sevenDayPct: 68.4, okAccounts: 1, rotationAccounts: 3,
    remote: 'A', nearWindow: '7d',
    ...extra,
  }
}

type Opts = { env?: any, run?: (argv: string[]) => any, store?: Map<string, any>, now?: number }

// harness registers the module against a fake `$` and returns handles on it.
function harness(opts: Opts = {}) {
  const handlers: any[] = []
  const on = (event: string, a: any, b?: any) => handlers.push({ event, filter: typeof a === 'function' ? undefined : a, fn: typeof a === 'function' ? a : b })
  register(on)
  const env: any = { HOME: '/Users/alice', HTTPS_PROXY: ROUTED, ...(opts.env || {}) }
  const store = opts.store || new Map<string, any>()
  const out: any = { opts: [] as any[], toasts: [] as string[], logs: [] as string[], runs: [] as string[][], registered: [] as any[], timers: [] as any[], now: opts.now ?? 0, invalidated: 0 }
  const $: any = {
    env: { get: async (k: string) => env[k] },
    process: { run: async (argv: string[], o: any) => { out.runs.push(argv); out.opts.push(o); return opts.run ? opts.run(argv) : { exitCode: 0, stdout: '', stderr: '' } } },
    store: { get: async (k: string) => store.get(k), set: async (k: string, v: any) => { store.set(k, v) } },
    clock: { now: async () => out.now, every: (_ms: number, fn: any) => { out.timers.push(fn) } },
    ui: {
      toast: async (t: string) => { out.toasts.push(t) },
      log: async (t: string) => { out.logs.push(t) },
      invalidate: () => { out.invalidated++ },
      resolve: () => ({ Box: (p: any) => ({ el: 'Box', ...p }), Text: (p: any) => ({ el: 'Text', ...p }) }),
    },
    command: { register: async (c: any) => { out.registered.push(c) } },
  }
  const matches = (h: any, event: string, e: any) => {
    if (h.event !== event) return false
    const f = h.filter
    if (!f) return true
    if (f.command !== undefined) return f.command === e.command
    if (f.component !== undefined) return f.component === e.component
    if (f.tool !== undefined) return f.tool instanceof RegExp ? f.tool.test(e.tool) : f.tool === e.tool
    return true
  }
  // fire runs the matching hooks as a chain; `last` is what the end of it gives.
  const fire = (event: string, e: any, last: any = async () => undefined) => {
    const chain = handlers.filter((h) => matches(h, event, e))
    const step = (i: number): any => (x: any) => (i >= chain.length ? last(x) : chain[i].fn($, x, step(i + 1)))
    return step(0)(e)
  }
  // start fires session.start, then waits on the refresh it began: the timer's
  // callback returns the refresh in flight, so nothing waits on a fixed count.
  const tick = async () => { await out.timers[0]() }
  return { $, env, store, out, fire, handlers, start: async () => { await fire('session.start', {}); await tick() }, tick }
}

const statuslineRun = (docs: any[]) => {
  let i = 0
  return (argv: string[]) => {
    if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(docs[Math.min(i++, docs.length - 1)]), stderr: '' }
    return { exitCode: 0, stdout: 'ok', stderr: '' }
  }
}

// rowsOf flattens a rendered band into one string per row (a row is a Box of Text).
const rowsOf = (tree: any): string[] => {
  const mine = tree.children[0]
  const rows = mine.flexDirection === 'column' ? mine.children : [mine]
  return rows.map((r: any) => r.children.map((x: any) => (x.children ? x.children.join('') : '')).join(''))
}
const flat = (lay: any) => lay.rows.map((r: any) => r.map((s: any) => s.t).join(''))

// ---- the guard ----

test('/login and /logout are stopped in a chottag session, with the verbatim text', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  const login = await h.fire('command.run', { command: 'login' }, async () => ({ text: 'NEXT' }))
  const logout = await h.fire('command.run', { command: 'logout' }, async () => ({ text: 'NEXT' }))
  expect(login.text).toBe(GUARD_LOGIN)
  expect(logout.text).toBe(GUARD_LOGOUT)
  expect(login.text).toBe("`/login` here would change Claude Code's own login (your Home account), not a chottag account. To add or repair a chottag account: `chottag login <name>` in a terminal. To change Home anyway: start a session with `CHOTTAG_BYPASS=1 claude`, or run `claude auth login` in a terminal.")
  expect(logout.text).toBe(login.text.replace('`/login` here would change', '`/logout` here would log out of'))
})

test('/login and /logout pass through when the session is not routed through chottag', async () => {
  const h = harness({ env: { HTTPS_PROXY: '' }, run: statuslineRun([doc()]) })
  await h.start()
  for (const command of ['login', 'logout']) {
    expect((await h.fire('command.run', { command }, async () => ({ text: 'NEXT' }))).text).toBe('NEXT')
  }
  const other = harness({ env: { HTTPS_PROXY: 'http://proxy.example.com:3128' } })
  await other.start()
  expect((await other.fire('command.run', { command: 'login' }, async () => ({ text: 'NEXT' }))).text).toBe('NEXT')
})

// ---- the band ----

test('the band: a left accent and no frame, two rows when wide, text only in Text elements', async () => {
  const h = harness({ run: statuslineRun([doc({ fiveHourResetsAt: new Date(3 * HOUR).toISOString(), sevenDayResetsAt: new Date(50 * HOUR).toISOString() })]) })
  await h.start()
  const tree = await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 120 } })
  const mine = tree.children[0]
  expect(mine.borderStyle).toBeUndefined() // no border prop is documented for one side: the accent is a ▌ prefix
  const rows = rowsOf(tree)
  expect(rows.length).toBe(2)
  expect(mine.children[0].children[0].children[0]).toBe('▌ ')
  expect(mine.children[0].children[0].color).toBe('blue')
  const text = rows.join('\n')
  expect(text).toContain('c» chottag')
  expect(text).toContain('Serving')
  expect(text).toContain('28%')
  expect(text).toContain('68%')
  expect(text).toContain('1 of 3 accounts ready')
  expect(text).not.toContain('▰')
  expect(text).not.toMatch(/[╭╮╰╯│─]/)
})


test('the band keeps what other mods draw under the card', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  const tree = await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 100 } }, async () => ({ el: 'Other' }))
  expect(tree.children.length).toBe(2)
  expect(tree.children[1].el).toBe('Other')
})

test('the band in its other states: down (red), off (dim), unknown after 3 failures, missing (nothing)', async () => {
  const render = (h: any) => h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 100 } })
  const note = async (h: any) => (await render(h)).children[0].children[1]

  const down = harness({ run: statuslineRun([{ ...doc(), daemon: 'down', serving: '', account: '' }]) })
  await down.start()
  const dt = await note(down)
  expect(dt.children[0]).toBe('c» chottag · down: the daemon is not answering')
  expect(dt.color).toBe('red')

  const off = harness({ env: { HTTPS_PROXY: '' }, run: statuslineRun([doc()]) })
  await off.start()
  const ot = await note(off)
  expect(ot.children[0]).toBe('c» chottag · off: this session is not routed through chottag')
  expect(ot.dimColor).toBe(true)

  const bad = harness({ run: () => ({ exitCode: 0, stdout: 'not json', stderr: '' }) })
  await bad.start() // failure 1
  await bad.tick() // 2
  expect((await note(bad)).children[0]).toBe('c» …')
  await bad.tick() // 3
  expect((await note(bad)).children[0]).toBe('c» ?')

  const none = harness({ run: () => { throw new Error('spawn chottag ENOENT') } })
  await none.start()
  expect(bandModel({ missing: true, isChottag: true, fails: 0, last: null }).kind).toBe('none')
  expect(await render(none)).toBeUndefined() // nothing drawn, and nothing of the other mods' lost
})


test('the band keeps the last good line while refreshes fail, then recovers', async () => {
  let fail = false
  const h = harness({ run: (argv) => (fail ? { exitCode: 1, stdout: '', stderr: 'x' } : statuslineRun([doc()])(argv)) })
  await h.start()
  fail = true
  await h.tick()
  await h.tick()
  expect(rowsOf(await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 100 } })).join('\n')).toContain('28%')
  fail = false
  await h.tick()
  expect(h.out.invalidated).toBeGreaterThan(0)
})

test('no row is wider than bodyColumns - 4, at every width from 20 to 250', () => {
  const docs = [
    doc(),
    doc({ label: 'dev', pool: 'work', limited: true, updateAvailable: '0.9.1', fiveHourPct: 100, sevenDayPct: 0 }),
    doc({ account: '日本語アカウント', serving: '日本語アカウント', remote: 'Zoë 🚀' }),
    doc({ account: 'B', serving: 'C' }),
    doc({ remote: '', fiveHourPct: undefined, sevenDayPct: undefined }),
    doc({ remote: 'C' }),
    doc({ remote: undefined }),
    OLD_0_8_5,
    { ...OLD_0_8_5, resetsAt: new Date(2 * HOUR + 10 * 60000).toISOString() },
  ]
  const nowMs = 0
  const fmt = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  for (const d of docs) {
    for (let columns = 20; columns <= 250; columns++) {
      const withResets = { ...d, fiveHourResetsAt: d.resetsAt === undefined ? new Date(nowMs + 3 * HOUR + 20 * 60000).toISOString() : undefined, sevenDayResetsAt: d.resetsAt === undefined ? new Date(nowMs + 3 * 24 * HOUR).toISOString() : undefined }
      const lay = layoutCard(withResets, nowMs, columns, fmt)
      if (lay.mode === 'line') {
        expect(lay.rows.length).toBe(1)
        continue
      }
      expect(lay.rows.length).toBeLessThanOrEqual(columns >= 100 ? 2 : 3)
      for (const r of lay.rows) {
        expect(displayWidth(r.map((s: any) => s.t).join(''))).toBeLessThanOrEqual(columns - 4)
        for (const s of r) expect(/[▰▱╭╮╰╯│]/.test(s.t)).toBe(false)
      }
    }
  }
})

test('the segments keep their order and are 3 spaces apart, with no padding', () => {
  const fmt = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  const legacy = { ...OLD_0_8_5, resetsAt: new Date(2 * HOUR + 10 * 60000).toISOString(), updateAvailable: '0.9.1', remote: 'A' }
  const full = doc({ updateAvailable: '0.9.1', fiveHourResetsAt: new Date(2 * HOUR).toISOString(), sevenDayResetsAt: new Date(3 * 24 * HOUR).toISOString() })
  for (const [d, order] of [
    [full, ['c» chottag', 'Serving C', '5-hour', 'weekly', 'Remote A', '1 of 3 accounts ready', 'update 0.9.1 available']],
    [legacy, ['c» chottag', 'Serving C', '5-hour', 'weekly', 'next reset in', 'Remote A', '1 of 3 accounts ready', 'update 0.9.1 available']],
  ] as any[]) {
    for (const columns of [250, 140, 100, 99, 80, 60]) {
      const lay = layoutCard(d, 0, columns, fmt)
      const text = flat(lay).join('\n')
      let at = -1
      for (const part of order) {
        const i = text.indexOf(part)
        if (i < 0) continue // a narrow width may have dropped nothing, but be strict about order only
        expect(i).toBeGreaterThan(at)
        at = i
      }
      for (const r of lay.rows) {
        expect(r[0].t).toBe('▌ ')
        for (let i = 1; i < r.length; i++) {
          if (/^ {2,}$/.test(r[i].t)) expect(r[i].t).toBe('   ')
        }
        expect(/^ +$/.test(r[r.length - 1].t)).toBe(false) // never padded to the width
        for (let i = 1; i < r.length - 1; i++) if (r[i].t === '   ') expect(/^ +$/.test(r[i + 1].t)).toBe(false)
      }
    }
  }
})

test('the variant chosen at the boundaries: bars, then no bars, then short resets', () => {
  const fmt = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  const d = doc({ fiveHourResetsAt: new Date(2 * HOUR + 10 * 60000).toISOString(), sevenDayResetsAt: new Date(4 * 24 * HOUR).toISOString() })
  const variant = (cols: number) => {
    const lay = layoutCard(d, 0, cols, fmt)
    const text = flat(lay).join('\n')
    if (lay.mode === 'line') return 'line'
    return text.includes('█') || text.includes('░') ? 'bars' : text.includes('resets') ? 'full' : 'short'
  }
  const rank: any = { line: 0, short: 1, full: 2, bars: 3 }
  // Within each row class (60-99: 3 rows; 100+: 2 rows) a wider band never gets a poorer variant,
  // and one column narrower than where a variant first appears gives a poorer one.
  for (const [lo, hi] of [[60, 99], [100, 250]]) {
    let prev = 0
    const firstAt: any = {}
    for (let c = lo; c <= hi; c++) {
      const v = variant(c)
      expect(rank[v]).toBeGreaterThanOrEqual(prev)
      prev = Math.max(prev, rank[v])
      if (!(v in firstAt)) firstAt[v] = c
    }
    expect(Object.keys(firstAt).length).toBeGreaterThanOrEqual(2)
    for (const v of Object.keys(firstAt)) if (firstAt[v] > lo) expect(rank[variant(firstAt[v] - 1)]).toBeLessThan(rank[v])
  }
  expect(variant(100)).not.toBe('line')
  expect(variant(59)).toBe('line')
  expect(variant(20)).toBe('line')
  expect(layoutCard(d, 0, 99, fmt).rows.length).toBeLessThanOrEqual(3)
  expect(layoutCard(d, 0, 100, fmt).mode).toBe('wide')
  expect(layoutCard(d, 0, 99, fmt).mode).toBe('medium')
  expect(layoutCard(d, 0, 60, fmt).mode).toBe('medium')
  expect(layoutCard(d, 0, 59, fmt).mode).toBe('line')
})


test('display width counts CJK and emoji as 2 cells and marks as 0', () => {
  expect(displayWidth('abc')).toBe(3)
  expect(displayWidth('日本')).toBe(4)
  expect(displayWidth('🚀')).toBe(2)
  expect(displayWidth('⚡')).toBe(2)
  expect(displayWidth('✅')).toBe(2)
  expect(displayWidth('🇹')).toBe(1)
  expect(displayWidth('🪄')).toBe(2)
  expect(displayWidth('█░')).toBe(2)
  expect(displayWidth('é')).toBe(1)
})

test('width classes: 2 rows from 100 columns, 3 from 60, one line below', () => {
  const d = doc({ nearWindow: undefined, fiveHourResetsAt: new Date(2 * HOUR + 10 * 60000).toISOString(), sevenDayResetsAt: new Date(4 * 24 * HOUR).toISOString() })
  const fmt = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  const at = (cols: number) => layoutCard(d, 0, cols, fmt)
  const wide = at(180)
  expect(wide.mode).toBe('wide')
  expect(flat(wide)[0]).toMatch(/^▌ c» chottag {3}Serving C {3}5-hour ██░░░░░░ 28% · resets in 2h 10m \(\d\d:\d\d\) {3}weekly █████░░░ 68% · resets /)
  expect(flat(wide).join('\n')).toMatch(/Remote A · Remote Control, connectors and artifacts use it/)
  expect(wide.rows.length).toBeLessThanOrEqual(2)
  const medium = at(99)
  expect(medium.mode).toBe('medium')
  expect(medium.rows.length).toBeLessThanOrEqual(3)
  expect(flat(medium)[0]).toMatch(/^▌ c» chottag {3}Serving C/)
  expect(flat(medium).join('')).toContain('5-hour')
  expect(flat(medium).join('')).toContain('weekly')
  expect(flat(medium).join('')).toContain('1 of 3 accounts ready')
  expect(at(60).mode).toBe('medium')
  const small = at(59)
  expect(small.mode).toBe('line')
  expect(flat(small)).toEqual(['c» C · 5h 28% · 7d 68%'])
  expect(at(39).mode).toBe('line')
})

test('a tight width drops the bars, then the long reset text, before it gives up on a row count', () => {
  const d = doc({ fiveHourResetsAt: new Date(2 * HOUR + 10 * 60000).toISOString(), sevenDayResetsAt: new Date(4 * 24 * HOUR).toISOString() })
  const fmt = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  const w = layoutCard(d, 0, 100, fmt)
  expect(w.mode).toBe('wide')
  expect(w.rows.length).toBeLessThanOrEqual(2)
  expect(flat(w).join('')).not.toContain('█') // the bars did not fit in 2 rows of 96 cells
  const m = layoutCard(d, 0, 66, fmt)
  expect(m.mode).toBe('medium')
  expect(flat(m).join('')).toContain('↻')
  expect(flat(m).join('')).not.toContain('resets')
})


test('the bars are 8 blocks, with a rounded percent, and – when unknown', () => {
  const lay = layoutCard(doc({ fiveHourPct: 28.2, sevenDayPct: undefined }), 0, 120)
  const text = flat(lay).join('\n')
  expect(text).toContain('5-hour ██░░░░░░ 28%')
  expect(text).toContain('weekly ░░░░░░░░ –')
  expect(flat(layoutCard(doc({ fiveHourPct: 100, sevenDayPct: 50 }), 0, 140)).join('')).toContain('5-hour ████████ 100%')
})


test('the role segments: serving, remote, both, spread, no remote, and an older chottag', () => {
  const rows = (d: any, cols = 250) => flat(layoutCard(d, 0, cols)).join('\n')
  const apart = rows(doc())
  expect(apart).toMatch(/Serving C/)
  expect(apart).toMatch(/Remote A · Remote Control, connectors and artifacts use it/)

  const both = rows(doc({ remote: 'C' }))
  expect(both).toContain('Serving + remote C')
  expect(both).not.toContain('Remote C')

  const spread = rows(doc({ account: 'B', serving: 'C' }))
  expect(spread).toMatch(/This session B +pool serves C/)
  expect(spread).toContain('Remote A')

  expect(rows(doc({ remote: '' }))).toContain('Remote none (uses your own Claude login)')
  expect(rows(doc({ remote: undefined }))).not.toContain('Remote')
})

// What chottag 0.8.5 prints for `statusline --json` on a routed session (the
// fields of its statuslineResult): no remote, no per-window resets, no nearWindow.
const OLD_0_8_5 = {
  version: 1, ok: true, warnings: [], session: 'routed', daemon: 'up', serving: 'C', account: 'C',
  fiveHourPct: 11, sevenDayPct: 77, resetsAt: '', okAccounts: 1, rotationAccounts: 3,
}

test('an older chottag: no remote segment, and its single reset is its own "next reset in" segment', () => {
  const fmt = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  const reset = new Date(2 * HOUR + 10 * 60000).toISOString()
  for (const five of [11, 85]) {
    const text = flat(layoutCard({ ...OLD_0_8_5, fiveHourPct: five, resetsAt: reset }, 0, 200, fmt)).join('\n')
    expect(text).toMatch(/next reset in 2h 10m \(\d\d:\d\d\)/)
    expect(text).not.toContain('· resets') // never attached to a window it may not belong to
    expect(text).not.toContain('Remote')
  }
  expect(flat(layoutCard({ ...OLD_0_8_5, resetsAt: reset }, 0, 70, fmt)).join('\n')).toMatch(/next (reset in|↻ )/)
  // no reset at all: nothing
  const none = flat(layoutCard(OLD_0_8_5, 0, 200, fmt)).join('\n')
  expect(none).not.toContain('reset')
  expect(none).toContain('1 of 3 accounts ready')
  // 0.9.0 and later say "no remote" with ''
  expect(flat(layoutCard({ ...OLD_0_8_5, remote: '' }, 0, 200, fmt)).join('\n')).toContain('Remote none')
})


test('the window nearer its switch point is bold, and colours follow the use', () => {
  const lay = layoutCard(doc({ nearWindow: '7d', fiveHourPct: 10, sevenDayPct: 95 }), 0, 120)
  const bold = (name: string) => lay.rows.flat().filter((s: any) => s.t.startsWith(name)).every((s: any) => s.bold)
  expect(bold('weekly')).toBe(true)
  expect(bold('5-hour')).toBe(false)
  const colours = (d: any) => layoutCard(d, 0, 120).rows.flat().filter((s: any) => /^\s*\d+%$/.test(s.t)).map((s: any) => s.color)
  expect(colours(doc({ fiveHourPct: 69, sevenDayPct: 70 }))).toEqual(['green', 'yellow'])
  expect(colours(doc({ fiveHourPct: 89, sevenDayPct: 90 }))).toEqual(['yellow', 'red'])
  expect(colours(doc({ fiveHourPct: 5, sevenDayPct: 5, limited: true }))).toEqual(['red', 'red'])
})

test('the dev label turns the chrome amber and joins the title; a pool joins it too', () => {
  const lay = layoutCard(doc({ label: 'dev', pool: 'work' }), 0, 140)
  expect(lay.rows[0][0]).toEqual({ t: '▌ ', color: 'yellow' })
  expect(lay.rows[0][1].t).toBe('c» chottag dev · pool work')
  expect(lay.rows[0][1].color).toBe('yellow')
  expect(layoutCard(doc(), 0, 140).rows[0][1].color).toBe('blue')
})


test('NO_COLOR turns every colour off', async () => {
  const h = harness({ env: { NO_COLOR: '1' }, run: statuslineRun([doc()]) })
  await h.start()
  const tree = await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 100 } })
  const mine = tree.children[0]
  const texts = (mine.flexDirection === 'column' ? mine.children : [mine]).flatMap((r: any) => r.children)
  expect(texts.some((t: any) => t.color !== undefined)).toBe(false)
})

// ---- the reset ----

test('the reset brackets and their boundaries, in two time zones', () => {
  const now = Date.UTC(2026, 9, 1, 5, 0, 0) // Thu 12:00 in Bangkok, 01:00 in New York
  const bkk = { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' }
  const nyc = { locale: 'en-US', timeZone: 'America/New_York', hourCycle: 'h23' }
  const at = (ms: number) => new Date(now + ms).toISOString()
  const MIN = 60000
  expect(formatReset(at(30000), now, bkk)).toBe('in <1m')
  expect(formatReset(at(MIN), now, bkk)).toBe('in 1m')
  expect(formatReset(at(HOUR - 1000), now, bkk)).toBe('in 59m')
  expect(formatReset(at(HOUR), now, bkk)).toBe('in 1h (13:00)')
  expect(formatReset(at(3 * HOUR + 20 * MIN + 59000), now, bkk)).toBe('in 3h 20m (15:20)')
  expect(formatReset(at(3 * HOUR + 5 * MIN), now, bkk)).toBe('in 3h 05m (15:05)')
  expect(formatReset(at(3 * HOUR + 20 * MIN), now, nyc)).toBe('in 3h 20m (04:20)')
  expect(formatReset(at(24 * HOUR - MIN), now, bkk)).toBe('in 23h 59m (11:59)')
  expect(formatReset(at(24 * HOUR), now, bkk)).toBe('Fri 12:00')
  expect(formatReset(at(24 * HOUR), now, nyc)).toBe('Fri 01:00')
  expect(formatReset(at(6 * 24 * HOUR - MIN), now, bkk)).toBe('Wed 11:59')
  expect(formatReset(at(6 * 24 * HOUR), now, bkk)).toBe('Oct 7, 12:00')
  expect(formatReset(at(9 * 24 * HOUR + 6 * HOUR), now, bkk)).toBe('Oct 10, 18:00')
  expect(formatReset(at(0), now, bkk)).toBe('') // a reset that has passed shows nothing
  expect(formatReset(at(-5000), now, bkk)).toBe('')
  expect(formatReset('', now, bkk)).toBe('')
  expect(formatReset('not a date', now, bkk)).toBe('')
})

test('a reset time on another side of midnight is read in the requested zone', () => {
  const now = Date.UTC(2026, 9, 1, 17, 30, 0) // 00:30 Fri in Bangkok, 13:30 Thu in New York
  const at = new Date(now + 2 * HOUR).toISOString()
  expect(formatReset(at, now, { locale: 'en-US', timeZone: 'Asia/Bangkok', hourCycle: 'h23' })).toBe('in 2h (02:30)')
  expect(formatReset(at, now, { locale: 'en-US', timeZone: 'America/New_York', hourCycle: 'h23' })).toBe('in 2h (15:30)')
})

test('time left is recomputed on every redraw, not every refresh', async () => {
  const resets = { fiveHourResetsAt: new Date(3 * HOUR + 20 * 60000).toISOString() }
  const h = harness({ now: 0, run: statuslineRun([doc(resets)]) })
  await h.start()
  const text = async () => rowsOf(await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 120 } })).join('\n')
  expect(await text()).toContain('in 3h 20m')
  h.out.now = 3 * HOUR
  expect(await text()).toContain('in 20m')
})

// ---- the notices ----

test('a switch toasts once with its reason; the first refresh only sets the baseline', async () => {
  const h = harness({ run: statuslineRun([doc({ account: 'B', serving: 'B' }), doc({ account: 'C', serving: 'C', lastSwitch: { account: 'C', from: 'B', reason: 'limit' } }), doc({ account: 'C', serving: 'C' })]) })
  await h.start()
  expect(h.out.toasts).toEqual([])
  await h.tick()
  expect(h.out.toasts).toEqual(['chottag: now on C (was B: limit)'])
  await h.tick()
  expect(h.out.toasts.length).toBe(1)
})

test('a switch without a known reason leaves the reason out', async () => {
  const h = harness({ run: statuslineRun([doc({ account: 'B' }), doc({ account: 'C' })]) })
  await h.start()
  await h.tick()
  expect(h.out.toasts).toEqual(['chottag: now on C (was B)'])
})

test('an update toasts once per version per machine', async () => {
  const store = new Map<string, any>()
  const first = harness({ store, run: statuslineRun([doc({ updateAvailable: '0.9.1' })]) })
  await first.start()
  await first.tick()
  expect(first.out.toasts).toEqual(['chottag 0.9.1 is available. Run: chottag update'])
  expect(store.get('update-told')).toBe('0.9.1')
  const second = harness({ store, run: statuslineRun([doc({ updateAvailable: '0.9.1' })]) }) // another session
  await second.start()
  expect(second.out.toasts).toEqual([])
  const third = harness({ store, run: statuslineRun([doc({ updateAvailable: '0.9.2' })]) })
  await third.start()
  expect(third.out.toasts).toEqual(['chottag 0.9.2 is available. Run: chottag update'])
})

test('a remote account that needs a login toasts at most once an hour', async () => {
  const h = harness({ now: 1000, run: statuslineRun([doc({ remoteToken: 'needs-login' })]) })
  await h.start()
  expect(h.out.toasts).toEqual(['chottag: remote account A needs attention: run chottag login A'])
  expect(h.store.has('remote-told:A')).toBe(true)
  await h.tick()
  expect(h.out.toasts.length).toBe(1)
  h.out.now = 1000 + HOUR + 1
  await h.tick()
  expect(h.out.toasts.length).toBe(2)
})

test('a remote token that stays stale toasts on the second refresh, not the first', async () => {
  const h = harness({ run: statuslineRun([doc({ remoteToken: 'stale' }), doc({ remoteToken: 'stale' })]) })
  await h.start()
  expect(h.out.toasts).toEqual([])
  await h.tick()
  expect(h.out.toasts).toEqual(['chottag: remote account A needs attention: run chottag login A'])
  const recovers = harness({ run: statuslineRun([doc({ remoteToken: 'stale' }), doc(), doc({ remoteToken: 'stale' })]) })
  await recovers.start()
  await recovers.tick()
  await recovers.tick()
  expect(recovers.out.toasts).toEqual([])
})

// ---- connector refusals ----

test('a refused connector call gets a line naming the account it went out as', async () => {
  const h = harness({ run: statuslineRun([doc({ pool: 'work' })]) })
  await h.start()
  const r = await h.fire('tool.call', { tool: 'mcp__claude_ai_Gmail__search' }, async () => ({ result: '403 forbidden: not shared with you' }))
  expect(r.result).toBe('403 forbidden: not shared with you\n(chottag sent this connector call as account A, the remote account of pool work)')
  const ok = await h.fire('tool.call', { tool: 'mcp__claude_ai_Gmail__search' }, async () => ({ result: '3 messages' }))
  expect(ok.result).toBe('3 messages')
  const other = await h.fire('tool.call', { tool: 'Bash' }, async () => ({ result: 'forbidden' }))
  expect(other.result).toBe('forbidden')
})

test('a refusal whose result is not text is told by a toast instead', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  const r = await h.fire('tool.call', { tool: 'mcp__claude_ai_Drive__read' }, async () => ({ deny: 'insufficient permission' }))
  expect(r.deny).toBe('insufficient permission')
  expect(h.out.toasts).toEqual(['(chottag sent this connector call as account A, the remote account of pool default)'])
})

test('connector calls are left alone outside a chottag session', async () => {
  const h = harness({ env: { HTTPS_PROXY: '' }, run: statuslineRun([doc()]) })
  await h.start()
  const r = await h.fire('tool.call', { tool: 'mcp__claude_ai_Gmail__search' }, async () => ({ result: '403 forbidden' }))
  expect(r.result).toBe('403 forbidden')
})

// ---- /ct ----

test('/ct registers its command, and /chottag is not touched', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  expect(h.out.registered.map((c: any) => c.name)).toEqual(['ct'])
})

test('/ct parsing: the verbs, a valid NAME, and help for anything else', () => {
  expect(parseCt('')).toEqual({ verb: 'status' })
  expect(parseCt('status')).toEqual({ verb: 'status' })
  expect(parseCt('  next ')).toEqual({ verb: 'next' })
  expect(parseCt('pool')).toEqual({ verb: 'pool' })
  expect(parseCt('help')).toEqual({ verb: 'help' })
  expect(parseCt('tag work-2.a_b')).toEqual({ verb: 'tag', name: 'work-2.a_b' })
  for (const bad of ['tag --unpin', 'tag --json', 'tag -x', 'tag .hidden', 'tag _x', 'tag', 'tag a b', 'tag a;b', 'tag $(id)', 'tag ../x', 'tag ' + 'x'.repeat(33), 'status extra', 'next --force', 'pool add x', 'login', 'Status']) {
    expect(parseCt(bad)).toEqual({ verb: 'help' })
  }
})

test('/ct runs the CLI with an argument list, strips colour, and adds the session pool', async () => {
  const h = harness({
    run: (argv) => {
      if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(doc({ pool: 'work' })), stderr: '' }
      return { exitCode: 0, stdout: '\x1b[1mwork\x1b[0m  ok\n', stderr: '' }
    },
  })
  await h.start()
  const run = (args: string) => h.fire('command.run', { command: 'ct', args })
  expect((await run('')).text).toBe('work  ok')
  expect(h.out.runs.at(-1)).toEqual(['chottag', 'status'])
  await run('next')
  expect(h.out.runs.filter((a: string[]) => a[1] === 'next').at(-1)).toEqual(['chottag', 'next', '--pool', 'work'])
  await run('tag B')
  expect(h.out.runs.filter((a: string[]) => a[1] === 'tag').at(-1)).toEqual(['chottag', 'tag', 'B', '--pool', 'work'])
  await run('pool')
  expect(h.out.runs.filter((a: string[]) => a[1] === 'pool').at(-1)).toEqual(['chottag', 'pool'])
})

test('/ct in the default pool adds no --pool, and help runs nothing', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  const before = h.out.runs.length
  expect((await h.fire('command.run', { command: 'ct', args: 'help' })).text).toBe(CT_HELP)
  expect((await h.fire('command.run', { command: 'ct', args: 'tag a;b' })).text).toBe(CT_HELP)
  expect(h.out.runs.length).toBe(before)
  await h.fire('command.run', { command: 'ct', args: 'next' })
  expect(h.out.runs.filter((a: string[]) => a[1] === 'next').at(-1)).toEqual(['chottag', 'next'])
})

test('/ct reports a failing command by its output, and a missing binary plainly', async () => {
  const h = harness({ run: (argv) => (argv[1] === 'statusline' ? { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' } : { exitCode: 2, stdout: '', stderr: 'chottag: no such account\n' }) })
  await h.start()
  expect((await h.fire('command.run', { command: 'ct', args: 'tag nope' })).text).toBe('chottag: no such account')
  const none = harness({ run: () => { throw new Error('ENOENT') } })
  await none.start()
  expect((await none.fire('command.run', { command: 'ct', args: '' })).text).toContain('not found')
})

// ---- finding the binary ----

test('the binary: PATH first, then the install directory, or CHOTTAG_HOME in dev', async () => {
  const seen: string[] = []
  const h = harness({ run: (argv) => { seen.push(argv[0]); if (argv[0] === 'chottag') throw new Error('ENOENT'); return { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' } } })
  await h.start()
  expect(seen.slice(0, 2)).toEqual(['chottag', '/Users/alice/.chottag/bin/chottag'])
  await h.tick()
  expect(seen.at(-1)).toBe('/Users/alice/.chottag/bin/chottag') // the one that answered is remembered
  expect(seen.filter((b) => b === 'chottag').length).toBe(1)

  const dev: string[] = []
  const d = harness({ env: { CHOTTAG_HOME: '/Users/alice/dev-home' }, run: (argv) => { dev.push(argv[0]); if (argv[0] === 'chottag') throw new Error('ENOENT'); return { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' } } })
  await d.start()
  expect(dev[1]).toBe('/Users/alice/dev-home/bin/chottag')
})

test('every run is an argument list with a timeout, and no value of the proxy variable is shown', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  for (const argv of h.out.runs) expect(Array.isArray(argv)).toBe(true)
  for (const o of h.out.opts) expect(o).toEqual({ timeout: 5000 })
  const shown = JSON.stringify([h.out.toasts, h.out.logs, [...h.store.entries()], h.out.runs])
  expect(shown).not.toContain('0123456789abcdef')
  expect(shown).not.toContain('127.0.0.1')
})

// ---- fix round 1 ----

test('a mutating verb that times out is never run again on another binary', async () => {
  const seen: string[][] = []
  const h = harness({
    run: (argv) => {
      if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' }
      seen.push(argv)
      if (argv[0] === 'chottag') throw new Error('process still running at the 15000ms timeout')
      return { exitCode: 0, stdout: 'switched', stderr: '' }
    },
  })
  await h.start()
  const r = await h.fire('command.run', { command: 'ct', args: 'next' })
  expect(seen.length).toBe(1) // the second binary was never started
  expect(r.text).toContain('did not finish')
  expect(h.out.opts.at(-1)).toEqual({ timeout: 15000 })
  const tag = await h.fire('command.run', { command: 'ct', args: 'tag B' })
  expect(seen.length).toBe(2)
  expect(tag.text).toContain('did not finish')
})

test('a mutating verb falls back only when the first binary could not start', async () => {
  const seen: string[] = []
  const h = harness({
    run: (argv) => {
      if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' }
      seen.push(argv[0])
      if (argv[0] === 'chottag') throw new Error('spawn chottag ENOENT')
      return { exitCode: 0, stdout: 'switched', stderr: '' }
    },
  })
  await h.start()
  expect((await h.fire('command.run', { command: 'ct', args: 'next' })).text).toBe('switched')
  expect(seen).toEqual(['chottag', '/Users/alice/.chottag/bin/chottag'])
})

test('a mutating verb that fails any other way is not retried either', async () => {
  const seen: string[] = []
  const h = harness({ run: (argv) => { if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' }; seen.push(argv[0]); throw new Error('boom') } })
  await h.start()
  expect((await h.fire('command.run', { command: 'ct', args: 'next' })).text).toContain('failed: boom')
  expect(seen.length).toBe(1)
})

test('a non-zero exit of a mutating verb is shown, and not retried', async () => {
  const seen: string[] = []
  const h = harness({ run: (argv) => { if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(doc()), stderr: '' }; seen.push(argv[0]); return { exitCode: 2, stdout: '', stderr: 'refused' } } })
  await h.start()
  expect((await h.fire('command.run', { command: 'ct', args: 'next' })).text).toBe('refused')
  expect(seen.length).toBe(1)
})

test('a successful connector result that says "access" is left alone', async () => {
  const h = harness({ run: statuslineRun([doc()]) })
  await h.start()
  const ok = await h.fire('tool.call', { tool: 'mcp__claude_ai_Gmail__search' }, async () => ({ result: 'Your access request was approved. ' + 'x'.repeat(400) }))
  expect(ok.result.startsWith('Your access request')).toBe(true)
  expect(ok.result).not.toContain('chottag sent')
  const long = await h.fire('tool.call', { tool: 'mcp__claude_ai_Gmail__search' }, async () => ({ result: 'an email about 403 forbidden pages. ' + 'x'.repeat(400) }))
  expect(long.result).not.toContain('chottag sent') // a long, successful result is not a refusal
  const err = await h.fire('tool.call', { tool: 'mcp__claude_ai_Gmail__search' }, async () => ({ isError: true, result: 'Error 403: ' + 'x'.repeat(400) }))
  expect(err.result).toContain('chottag sent')
})

test('a short band gets fewer rows, or the one line', () => {
  const d = doc()
  for (const cols of [80, 140]) {
    for (const maxRows of [1, 2, 3, 20]) {
      const lay = layoutCard(d, 0, cols, undefined, maxRows)
      expect(lay.rows.length).toBeLessThanOrEqual(maxRows)
    }
  }
  expect(layoutCard(d, 0, 80, undefined, 1).mode).toBe('line')
})


test('the one line is truncated, not wrapped', async () => {
  const long = 'x'.repeat(32)
  const lay = layoutCard(doc({ account: long, serving: long }), 0, 20)
  expect(lay.mode).toBe('line')
  const h = harness({ run: statuslineRun([doc({ account: long, serving: long })]) })
  await h.start()
  const tree = await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 20 } })
  const texts = tree.children[0].children
  expect(texts.every((t: any) => t.wrap === 'truncate')).toBe(true)
})

test('a change made by /ct refreshes the card even while a timer refresh is running', async () => {
  let n = 0
  const h = harness({
    run: (argv) => {
      if (argv[1] === 'statusline') return { exitCode: 0, stdout: JSON.stringify(doc({ account: n++ < 1 ? 'B' : 'C', serving: 'C' })), stderr: '' }
      return { exitCode: 0, stdout: 'ok', stderr: '' }
    },
  })
  await h.start() // card shows B
  const timer = h.out.timers[0]()
  await h.fire('command.run', { command: 'ct', args: 'next' }) // refresh in flight: one more is queued
  await timer
  expect(rowsOf(await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 100 } })).join('\n')).toContain(' C ')
})

test('a notice that throws does not escape the refresh', async () => {
  const h = harness({ run: statuslineRun([doc({ updateAvailable: '0.9.1' })]) })
  h.$.ui.toast = async () => { throw new Error('toast failed') }
  await h.start()
  expect(h.out.logs.some((l: string) => l.includes('a notice failed'))).toBe(true)
})

test('rendered from the real 0.8.5 JSON shape: no remote, no "none"', async () => {
  const h = harness({ run: statuslineRun([OLD_0_8_5]) })
  await h.start()
  const text = rowsOf(await h.fire('ui.render', { component: 'AbovePrompt', props: { bodyColumns: 120 } })).join('\n')
  expect(text).toContain('Serving C')
  expect(text).toContain('11%')
  expect(text).not.toContain('Remote')
  expect(h.out.toasts).toEqual([])
})
