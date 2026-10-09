// chottag's Claude Code mod (Claude Code 2.1.287 or newer): the /login and
// /logout guard, a status card above the prompt, notices, and /ct.
//
// It talks to chottag only through the CLI (`chottag statusline --json`,
// `chottag status` and the verbs /ct runs). It never reads chottag's files,
// and it never prints, logs or stores the proxy variable's value: the value is
// tested for its user part and dropped. Nothing here depends on it: the shim
// and the proxy are the guarantees.

// MOD_VERSION is this plugin's version, set by `scripts/release bump` and tested
// against plugin.json. A chottag newer than it means the plugin is behind.
export const MOD_VERSION = '0.10.2'
const VERSION_CHECK_MS = 600000
const VERSION = /^v?(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]*)?/
const CLEAN_VERSION = /^v?\d+\.\d+\.\d+[0-9A-Za-z.+-]{0,64}$/
// A describe suffix (a build after a release: -3-gabc1234, -dirty) is not a pre-release.
const DESCRIBE = /^-(\d+-g[0-9a-f]+(-dirty)?|dirty)$/

const CT_VERBS = ['status', 'next', 'tag', 'pool', 'names', 'help']
// 'model' was removed in 0.10.2; it still passes through so the CLI's own
// error shows instead of the help text.
const NAMES_MODES = ['on', 'off', 'model']
// The store's own account-name rule: it must start with a letter or digit, so a
// NAME can never read as an option (--unpin).
const NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$/
const ROUTED_USER = /^https?:\/\/chottag[.:@]/
const ANSI = /\x1b\[[0-9;]*[A-Za-z]/g
// A refusal: a 401/403-shaped error or a phrase an access error uses. A bare
// "access" is not one (a successful result may say it).
const REFUSAL = /\b40[13]\b|forbidden|not shared|access denied|permission denied|insufficient (permission|scope|privilege)/i
const SHORT_RESULT = 300
const TIMED_OUT = /time(d)?[ -]?out|still running/i
const CANNOT_START = /ENOENT|ENOTDIR|EACCES|not found|no such file|cannot find|could not (be )?(start|spawn)|failed to spawn/i
const READ_TIMEOUT_MS = 5000
const WRITE_TIMEOUT_MS = 15000
const REFRESH_MS = 15000
const FAILS_BEFORE_UNKNOWN = 3
const STALE_REFRESHES = 2
const HOUR_MS = 3600000
const BAR_CELLS = 8
const ACCENT = '▌ ' // the card's left border (a Box border prop for one side is not documented)
const GAP = 3
const RIGHT_MARGIN = 4 // Claude Code draws its [-] band control at the top right
const WIDE_COLUMNS = 100
const MEDIUM_COLUMNS = 60

export const GUARD_LOGIN =
  "`/login` here would change Claude Code's own login (your Home account), not a chottag account. " +
  'To add or repair a chottag account: `chottag login <name>` in a terminal. ' +
  'To change Home anyway: start a session with `CHOTTAG_BYPASS=1 claude`, or run `claude auth login` in a terminal.'
export const GUARD_LOGOUT = GUARD_LOGIN.replace('`/login` here would change', '`/logout` here would log out of')

export const CT_HELP = [
  'chottag, from this session:',
  '  /ct, /ct status   show the accounts and their limits',
  '  /ct next          serve the next account in rotation',
  '  /ct tag NAME      serve the account NAME',
  '  /ct pool          list the pools',
  '  /ct names [on|off]  show or set session names',
  '  /ct help          this list',
].join('\n')

// state is per module load: register() starts it clean, so a reload does too.
const freshState = () => ({
  isChottag: false, // is this session routed through chottag (from the proxy variable's user part)
  noColor: false,
  bin: null, // the chottag binary that last answered
  missing: false, // no binary could be started
  last: null, // the last good statusline document
  fails: 0, // refreshes that failed in a row
  running: null, // the refresh in flight
  again: false,
  remoteStale: {}, // account -> refreshes in a row with a stale token
  ctVersion: null, // the chottag version that last answered `chottag version --json`
  ctVersionAt: null, // when it was asked, in clock ms
  versionCheck: null, // the version check in flight
})
const state = freshState()

// ---- pure helpers (exported for the tests) ----

// isChottagProxy tests only the user part of the proxy URL. The value is
// neither returned nor kept.
export function isChottagProxy(value) {
  return ROUTED_USER.test(value || '')
}

export function stripAnsi(s) {
  return String(s || '').replace(ANSI, '')
}

// displayWidth counts terminal cells: wide (CJK, emoji) code points take 2,
// combining marks and joiners 0.
// Symbols with emoji presentation: wide although they sit in narrow blocks.
const EMOJI_WIDE = new Set([0x231a, 0x231b, 0x23e9, 0x23ea, 0x23eb, 0x23ec, 0x23f0, 0x23f3, 0x25fd, 0x25fe, 0x2614, 0x2615, 0x267f, 0x2693, 0x26a1, 0x26aa, 0x26ab, 0x26bd, 0x26be, 0x26c4, 0x26c5, 0x26ce, 0x26d4, 0x26ea, 0x26f2, 0x26f3, 0x26f5, 0x26fa, 0x26fd, 0x2705, 0x270a, 0x270b, 0x2728, 0x274c, 0x274e, 0x2753, 0x2754, 0x2755, 0x2757, 0x2795, 0x2796, 0x2797, 0x27b0, 0x27bf, 0x2b1b, 0x2b1c, 0x2b50, 0x2b55, 0x1f004, 0x1f0cf, 0x1f18e])
for (let c = 0x2648; c <= 0x2653; c++) EMOJI_WIDE.add(c)

export function displayWidth(s) {
  let w = 0
  for (const ch of String(s)) {
    const c = ch.codePointAt(0)
    if (c < 0x20 || (c >= 0x7f && c < 0xa0)) continue
    if ((c >= 0x300 && c <= 0x36f) || (c >= 0x200b && c <= 0x200f) || c === 0xfe0f) continue
    const wide =
      (c >= 0x1100 && c <= 0x115f) || (c >= 0x2e80 && c <= 0xa4cf) || (c >= 0xac00 && c <= 0xd7a3) ||
      (c >= 0xf900 && c <= 0xfaff) || (c >= 0xfe30 && c <= 0xfe6f) || (c >= 0xff00 && c <= 0xff60) ||
      (c >= 0xffe0 && c <= 0xffe6) || EMOJI_WIDE.has(c) || (c >= 0x1f200 && c <= 0x1f2ff) || (c >= 0x1f191 && c <= 0x1f19a) || (c >= 0x1f300 && c <= 0x1f64f) || (c >= 0x1f680 && c <= 0x1f6ff) || (c >= 0x1f900 && c <= 0x1f9ff) || (c >= 0x1fa70 && c <= 0x1faff) ||
      (c >= 0x20000 && c <= 0x3fffd)
    w += wide ? 2 : 1
  }
  return w
}

export function parseCt(args) {
  const parts = String(args || '').trim().split(/\s+/).filter(Boolean)
  const verb = parts.length === 0 ? 'status' : parts[0]
  if (CT_VERBS.includes(verb) && verb !== 'tag' && verb !== 'names' && parts.length <= 1) return { verb }
  if (verb === 'names' && parts.length === 1) return { verb }
  if (verb === 'names' && parts.length === 2 && NAMES_MODES.includes(parts[1])) return { verb, name: parts[1] }
  if (verb === 'tag' && parts.length === 2 && NAME.test(parts[1])) return { verb, name: parts[1] }
  return { verb: 'help' }
}

const WINDOWS = [
  { key: 'five', name: '5-hour', pct: 'fiveHourPct', reset: 'fiveHourResetsAt', near: '5h' },
  { key: 'seven', name: 'weekly', pct: 'sevenDayPct', reset: 'sevenDayResetsAt', near: '7d' },
]
const REMOTE_NOTE = 'Remote Control, connectors and artifacts use it'
const NO_REMOTE = 'none (uses your own Claude login)'

// formatReset says when a window resets, in local time (R151): "in 42m" under
// an hour (rounded down, "in <1m" under a minute), "in 3h 20m (01:40)" under a
// day, "Mon 18:00" within 6 days, "Oct 9, 18:00" beyond that. '' when unknown.
// The clock style comes from the system locale. opts ({ locale, timeZone,
// hourCycle }) exists for the tests; production passes none, so the system's
// locale and zone apply.
export function formatReset(iso, nowMs, opts) {
  if (!iso) return ''
  const at = new Date(iso)
  if (isNaN(at.getTime())) return ''
  const o = opts || {}
  const fmt = (extra) => new Intl.DateTimeFormat(o.locale, { timeZone: o.timeZone, hourCycle: o.hourCycle, ...extra })
  const clock = fmt({ hour: 'numeric', minute: '2-digit' }).format(at)
  const left = at.getTime() - nowMs
  if (left <= 0) return '' // already past: unknown, not "now"
  if (left < 60000) return 'in <1m'
  if (left < HOUR_MS) return 'in ' + Math.floor(left / 60000) + 'm'
  if (left < 24 * HOUR_MS) {
    const h = Math.floor(left / HOUR_MS)
    const m = Math.floor((left % HOUR_MS) / 60000)
    return 'in ' + h + 'h' + (m ? ' ' + String(m).padStart(2, '0') + 'm' : '') + ' (' + clock + ')'
  }
  if (left < 6 * 24 * HOUR_MS) return fmt({ weekday: 'short' }).format(at) + ' ' + clock
  return fmt({ month: 'short', day: 'numeric' }).format(at) + ', ' + clock
}

function pctText(p) {
  return typeof p === 'number' ? Math.round(p) + '%' : '–'
}

function bar(p) {
  const filled = typeof p === 'number' ? Math.min(BAR_CELLS, Math.max(0, Math.round((p / 100) * BAR_CELLS))) : 0
  return '█'.repeat(filled) + '░'.repeat(BAR_CELLS - filled)
}

function useColor(p, limited) {
  if (limited || (typeof p === 'number' && p >= 90)) return 'red'
  if (typeof p === 'number' && p >= 70) return 'yellow'
  return 'green'
}

function chrome(doc) {
  return doc && doc.label ? 'yellow' : 'blue' // chottag blue; amber for a dev install
}

export function cardTitle(doc) {
  let t = 'c» chottag'
  if (doc.label) t += ' ' + doc.label
  if (doc.pool && doc.pool !== 'default') t += ' · pool ' + doc.pool
  return t
}

// versionNewer is whether chottag's version a is newer than the plugin's b,
// comparing MAJOR.MINOR.PATCH only. False when either does not parse (a dev build).
export function versionNewer(a, b) {
  const x = String(a || '').match(VERSION)
  const y = String(b || '').match(VERSION)
  if (!x || !y) return false
  // A chottag pre-release (0.9.1-rc.1) has no plugin release to move to.
  if (x[4] && !DESCRIBE.test(x[4])) return false
  for (let i = 1; i <= 3; i++) {
    if (Number(x[i]) !== Number(y[i])) return Number(x[i]) > Number(y[i])
  }
  return false
}

// pluginBehind is the chottag version when the plugin is older than it, else ''.
export function pluginBehind(ctVersion) {
  return versionNewer(ctVersion, MOD_VERSION) ? String(ctVersion).replace(/^v/, '').match(/^\d+\.\d+\.\d+/)[0] : ''
}

const sameName = (a, b) => !!a && !!b && a.toLowerCase() === b.toLowerCase()

// shortLeft is the reset in the one-line form: "42m", "3h20m", else the long
// form's own text ("Mon 18:00", "Oct 9, 18:00").
function shortLeft(iso, nowMs, fmt) {
  const at = new Date(iso)
  const left = at.getTime() - nowMs
  if (!iso || isNaN(at.getTime()) || left <= 0) return ''
  if (left < 60000) return '<1m'
  if (left < HOUR_MS) return Math.floor(left / 60000) + 'm'
  if (left < 24 * HOUR_MS) {
    const h = Math.floor(left / HOUR_MS)
    const m = Math.floor((left % HOUR_MS) / 60000)
    return h + 'h' + (m ? String(m).padStart(2, '0') + 'm' : '')
  }
  return formatReset(iso, nowMs, fmt)
}

const groupWidth = (g) => g.reduce((n, s) => n + displayWidth(s.t), 0)

// flow lays the segments out left to right with a GAP between them (no
// stretching, no padding). A segment that does not fit on the row starts the
// next one; a segment is never split. It returns the rows, or null when one
// segment is wider than a row.
function flow(segs, accent, usable) {
  const rows = []
  let row = null
  let w = 0
  for (const seg of segs) {
    if (!seg.length) continue
    const sw = groupWidth(seg)
    if (displayWidth(accent.t) + sw > usable) return null
    if (row && w + GAP + sw <= usable) {
      row.push({ t: ' '.repeat(GAP) }, ...seg)
      w += GAP + sw
    } else {
      if (row) rows.push(row)
      row = [accent, ...seg]
      w = displayWidth(accent.t) + sw
    }
  }
  if (row) rows.push(row)
  return rows
}

// layoutCard turns a statusline document into rows of segments
// ({ t, color?, bold?, dim? }) for the band (R152, R153). The segments flow
// left to right with a fixed gap, no row wider than bodyColumns - 4. mode is
// 'wide' (2 rows, bodyColumns >= 100), 'medium' (3 rows, >= 60) or 'line' (one
// row). The richest variant that fits is used (bars and full resets; no bars;
// short resets), else the line. maxRows is the band's row limit. nowMs and fmt
// feed formatReset. behind, when set, is the chottag version the plugin is
// older than: the card gets a segment saying to update the plugin.
export function layoutCard(doc, nowMs, columns, fmt, maxRows, behind) {
  const cols = typeof columns === 'number' ? columns : 80
  const tone = chrome(doc)
  const accent = { t: ACCENT, color: tone }
  const usable = cols - RIGHT_MARGIN
  const session = doc.account
  const serving = doc.serving || doc.account
  // An older chottag (before 0.9.0) has no remote, no per-window resets and no
  // nearWindow: use what it has, and hide what it cannot tell.
  const remoteKnown = doc.remote !== undefined
  const both = remoteKnown && sameName(serving, doc.remote) && sameName(session, serving)
  const legacy = doc.fiveHourResetsAt === undefined && doc.sevenDayResetsAt === undefined && !!doc.resetsAt
  const reset5 = doc.fiveHourResetsAt || ''
  const reset7 = doc.sevenDayResetsAt || ''

  const groups = (bars, full) => {
    const win = (w, reset) => {
      const p = doc[w.pct]
      const c = useColor(p, doc.limited)
      const b = doc.nearWindow === w.near
      const g = [{ t: w.name + ' ', bold: b }]
      if (bars) g.push({ t: bar(p), color: c, bold: b }, { t: ' ' })
      g.push({ t: pctText(p), color: c, bold: b })
      const at = reset
      const r = full ? formatReset(at, nowMs, fmt) : shortLeft(at, nowMs, fmt)
      if (r) g.push({ t: full ? ' · resets ' + r : ' · ↻ ' + r, bold: b })
      return g
    }
    const title = [{ t: cardTitle(doc), bold: true, color: tone }]
    let account
    if (session !== serving) account = [{ t: 'This session ', dim: true }, { t: session, bold: true, color: tone }, { t: '  pool serves ' + serving, dim: true }]
    else account = [{ t: both ? 'Serving + remote ' : 'Serving ', dim: true }, { t: serving, bold: true, color: tone }]
    let remote = []
    if (remoteKnown && !both) {
      remote = doc.remote
        ? [{ t: 'Remote ', dim: true }, { t: doc.remote, bold: true, color: tone }, ...(full ? [{ t: ' · ' + REMOTE_NOTE, dim: true }] : [])]
        : [{ t: 'Remote none (uses your own Claude login)', dim: true }]
    }
    const ready = typeof doc.okAccounts === 'number' && typeof doc.rotationAccounts === 'number' ? [{ t: doc.okAccounts + ' of ' + doc.rotationAccounts + ' accounts ready', dim: true }] : []
    const update = doc.updateAvailable ? [{ t: 'update ' + doc.updateAvailable + ' available', dim: true }] : []
    // An older chottag has one reset and cannot say which window it is: its own segment.
    const nr = legacy ? formatReset(doc.resetsAt, nowMs, fmt) : ''
    const next = nr ? [{ t: full ? 'next reset ' + nr : 'next ↻ ' + shortLeft(doc.resetsAt, nowMs, fmt), dim: true }] : []
    const plugin = behind ? [{ t: 'plugin ' + MOD_VERSION + ' · chottag ' + behind + ': update the plugin', color: 'yellow' }] : []
    return [title, account, win(WINDOWS[0], reset5), win(WINDOWS[1], reset7), next, remote, ready, update, plugin]
  }
  const rowLimit = cols >= WIDE_COLUMNS ? 2 : cols >= MEDIUM_COLUMNS ? 3 : 0
  if (rowLimit && (typeof maxRows !== 'number' || maxRows >= 1)) {
    for (const [bars, full] of [[true, true], [false, true], [false, false]]) {
      const rows = flow(groups(bars, full), accent, usable)
      if (rows && rows.length <= Math.min(rowLimit, typeof maxRows === 'number' ? maxRows : rowLimit)) return { mode: cols >= WIDE_COLUMNS ? 'wide' : 'medium', rows }
    }
  }
  const left = shortLeft(doc.resetsAt, nowMs, fmt)
  const line = [session, '5h ' + pctText(doc.fiveHourPct), '7d ' + pctText(doc.sevenDayPct), ...(left ? ['↻ ' + left] : [])].join(' · ')
  return { mode: 'line', rows: [[{ t: 'c» ', bold: true, color: tone }, { t: line }]] }
}

// bandModel decides what the band shows from the module state.
export function bandModel(s) {
  if (s.missing) return { kind: 'none' }
  if (!s.isChottag) return { kind: 'note', text: 'c» chottag · off: this session is not routed through chottag', dim: true }
  if (s.fails >= FAILS_BEFORE_UNKNOWN) return { kind: 'note', text: 'c» ?', dim: true }
  if (!s.last) return { kind: 'note', text: 'c» …', dim: true }
  if (s.last.daemon === 'down') return { kind: 'note', text: 'c» chottag · down: the daemon is not answering', color: 'red' }
  if (s.last.session !== 'routed' || s.last.daemon !== 'up') return { kind: 'note', text: 'c» chottag · off: this session is not routed through chottag', dim: true }
  if (!s.last.account) return { kind: 'note', text: 'c» chottag · up: no account is serving', dim: true }
  return { kind: 'card' }
}

// ---- talking to chottag ----

async function candidates($) {
  const home = (await $.env.get('HOME')) || ''
  const dev = await $.env.get('CHOTTAG_HOME')
  const fallback = (dev || home + '/.chottag') + '/bin/chottag'
  const list = ['chottag', fallback]
  return state.bin ? [state.bin, ...list.filter((b) => b !== state.bin)] : list
}

// runChottag runs the CLI without a shell and resolves to the result, null when
// no binary could be started, or { error, timedOut } when a mutating verb
// failed in a way that must not be retried. A read falls through to the next
// candidate on any failure. A mutating verb (next, tag) does so only when the
// program could not start: after a timeout or any other error the first run
// may already have changed something, and a second run would change it again.
async function runChottag($, args, mutating) {
  let started = false
  for (const bin of await candidates($)) {
    try {
      const r = await $.process.run([bin, ...args], { timeout: mutating ? WRITE_TIMEOUT_MS : READ_TIMEOUT_MS })
      state.bin = bin
      state.missing = false
      return r
    } catch (err) {
      const msg = String(err && err.message ? err.message : err)
      if (!mutating) continue // this path does not start here, or timed out: try the next one
      if (!TIMED_OUT.test(msg) && CANNOT_START.test(msg)) continue // never started: nothing ran
      started = true
      return { error: msg, timedOut: TIMED_OUT.test(msg) }
    }
  }
  if (!state.bin && !started) state.missing = true
  return null
}

async function toastOnce($, key, text, minGapMs) {
  const now = await $.clock.now()
  const told = await $.store.get(key)
  if (told !== undefined && told !== null && (minGapMs === 0 || now - Number(told) < minGapMs)) return
  await $.store.set(key, minGapMs === 0 ? text : now)
  await $.ui.toast(text)
}

// notices compares one refresh with the previous one.
async function notices($, prev, cur) {
  if (prev && prev.account && cur.account && prev.account !== cur.account) {
    const ls = cur.lastSwitch
    const why = ls && ls.account === cur.account && ls.from === prev.account && ls.reason ? ': ' + ls.reason : ''
    await $.ui.toast('chottag: now on ' + cur.account + ' (was ' + prev.account + why + ')')
  }
  if (cur.updateAvailable) {
    const told = await $.store.get('update-told')
    if (told !== cur.updateAvailable) {
      await $.store.set('update-told', cur.updateAvailable)
      await $.ui.toast('chottag ' + cur.updateAvailable + ' is available. Run: chottag update')
    }
  }
  if (cur.remote) {
    const n = cur.remoteToken === 'stale' ? (state.remoteStale[cur.remote] || 0) + 1 : 0
    state.remoteStale = { [cur.remote]: n }
    if (cur.remoteToken === 'needs-login' || n >= STALE_REFRESHES) {
      await toastOnce($, 'remote-told:' + cur.remote, 'chottag: remote account ' + cur.remote + ' needs attention: run chottag login ' + cur.remote, HOUR_MS)
    }
  }
}

// checkVersion asks chottag its version (at most every ten minutes) and, when
// the plugin is older than it, toasts once per chottag version (keyed in the
// store). The card shows the same through state.ctVersion.
async function checkVersion($) {
  const now = await $.clock.now()
  if (state.ctVersionAt !== null && now - state.ctVersionAt < VERSION_CHECK_MS) return
  state.ctVersionAt = now
  const r = await runChottag($, ['version', '--json'])
  let doc = null
  try {
    doc = r && r.stdout ? JSON.parse(r.stdout) : null
  } catch (_) {
    doc = null
  }
  if (!doc || doc.ok !== true || typeof doc.chottag !== 'string') return
  if (!CLEAN_VERSION.test(doc.chottag)) return // the text is shown: only a version-shaped one is kept
  state.ctVersion = doc.chottag
  const behind = pluginBehind(doc.chottag)
  if (!behind) return
  if ((await $.store.get('plugin-told')) !== behind) {
    await $.store.set('plugin-told', behind)
    await $.ui.toast('chottag ' + behind + ' is installed but this plugin is ' + MOD_VERSION + '. Ask Claude Code to update the chottag plugin.')
  }
}

// refresh runs one refresh at a time. A call while one is running returns that
// run's promise; with again set (after a change the user made) it also queues
// one more run, so the card never stays a tick behind.
function refresh($, again) {
  if (state.running) {
    if (again) state.again = true
    return state.running
  }
  state.running = (async () => {
    try {
      do {
        state.again = false
        await refreshOnce($)
      } while (state.again)
    } finally {
      state.running = null
    }
  })()
  return state.running
}

async function refreshOnce($) {
  try {
    const r = await runChottag($, ['statusline', '--json'])
    let doc = null
    if (r && r.stdout) {
      try {
        doc = JSON.parse(r.stdout)
      } catch (_) {
        doc = null
      }
    }
    if (!doc || doc.ok !== true) {
      state.fails++ // keep the last line; after three in a row the band says "?"
    } else {
      state.fails = 0
      const prev = state.last
      state.last = doc
      try {
        await notices($, prev, doc)
      } catch (err) {
        await $.ui.log('chottag: a notice failed: ' + err)
      }
      // Off this refresh and the render path: the card redraws now, and again
      // when the version is known. state.versionCheck is the run in flight.
      if (!state.versionCheck) {
        state.versionCheck = checkVersion($)
          .catch((err) => $.ui.log('chottag: the version check failed: ' + err))
          .finally(() => {
            state.versionCheck = null
            $.ui.invalidate('ui.render')
          })
          .catch(() => {}) // a log that cannot be written has nowhere left to go
      }
    }
  } finally {
    $.ui.invalidate('ui.render')
  }
}

// ---- the hooks ----

// padFor is the card's top margin (R155): one empty line, which counts as a row
// against the band's row limit. When the content rows already fill maxRows the
// padding is the first thing dropped.
function padFor(contentRows, maxRows) {
  return typeof maxRows !== 'number' || contentRows + 1 <= maxRows ? { marginTop: 1 } : {}
}

function toTree(el, model, doc, nowMs, cols, maxRows, noColor, behind) {
  const { Box, Text } = el
  const tint = (c) => (noColor ? undefined : c)
  const text = (s, wrap) => {
    const p = { children: [s.t] }
    if (wrap) p.wrap = wrap
    if (s.bold) p.bold = true
    if (s.dim) p.dimColor = true
    if (s.color && tint(s.color)) p.color = s.color
    return Text(p)
  }
  if (model.kind === 'note') {
    const p = { wrap: 'truncate', children: [model.text] }
    if (model.dim) p.dimColor = true
    if (model.color && tint(model.color)) p.color = model.color
    const accent = { t: ACCENT, color: chrome(doc) }
    return Box({ flexDirection: 'row', ...padFor(1, maxRows), children: [text(accent), Text(p)] })
  }
  const lay = layoutCard(doc, nowMs, cols, undefined, maxRows, behind)
  const wrap = lay.mode === 'line' ? 'truncate' : undefined // a long name must not wrap the one line
  // One empty line above the card (R155): the margin sits on its outer Box.
  const single = lay.rows.length === 1
  const pad = padFor(lay.rows.length, maxRows)
  const rows = lay.rows.map((row, i) => Box({ key: 'chottag-row-' + i, flexDirection: 'row', ...(single ? pad : {}), children: row.map((s) => text(s, wrap)) }))
  return single ? rows[0] : Box({ flexDirection: 'column', ...pad, children: rows })
}

export function register(on) {
  Object.assign(state, freshState())
  on('session.start', async ($, e, next) => {
    state.isChottag = isChottagProxy(await $.env.get('HTTPS_PROXY'))
    state.noColor = !!(await $.env.get('NO_COLOR'))
    // The first refresh must not delay the session: the timer keeps it fresh.
    const failed = (err) => $.ui.log('chottag: refresh failed: ' + err)
    refresh($).catch(failed)
    $.clock.every(REFRESH_MS, () => refresh($).catch(failed))
    try {
      // Registered last: a refused name throws. (/chottag is the skill's.)
      await $.command.register({ name: 'ct', description: 'chottag: status, next, tag NAME, pool, names', argumentHint: '[status|next|tag NAME|pool|names [on|off]|help]' })
    } catch (err) {
      await $.ui.log('chottag: could not register /ct: ' + err)
    }
    return next(e)
  })

  // The guard: in a chottag session, /login and /logout would change Home's login.
  on('command.run', { command: 'login' }, async ($, e, next) => (state.isChottag ? { text: GUARD_LOGIN } : next(e)))
  on('command.run', { command: 'logout' }, async ($, e, next) => (state.isChottag ? { text: GUARD_LOGOUT } : next(e)))

  on('command.run', { command: 'ct' }, async ($, e) => {
    const cmd = parseCt(e.args)
    if (cmd.verb === 'help') return { text: CT_HELP }
    const argv = [cmd.verb]
    if (cmd.name) argv.push(cmd.name)
    const pool = state.last && state.last.pool
    if ((cmd.verb === 'next' || cmd.verb === 'tag') && pool && pool !== 'default') argv.push('--pool', pool)
    const mutating = cmd.verb === 'next' || cmd.verb === 'tag' || (cmd.verb === 'names' && !!cmd.name)
    const r = await runChottag($, argv, mutating)
    if (!r) return { text: 'chottag was not found, on PATH or in its install directory.' }
    if (r.error !== undefined) {
      return { text: r.timedOut ? 'chottag ' + argv.join(' ') + ' did not finish in ' + WRITE_TIMEOUT_MS / 1000 + ' seconds. It may still have run: check /ct status.' : 'chottag ' + argv.join(' ') + ' failed: ' + r.error }
    }
    if (mutating) await refresh($, true).catch(() => {})
    const out = stripAnsi(r.stdout).trim() || stripAnsi(r.stderr).trim()
    return { text: out || 'chottag ' + argv.join(' ') + ' exited ' + r.exitCode + ' with no output.' }
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const others = await next(e)
    const model = bandModel(state)
    if (model.kind === 'none') return others
    const el = $.ui.resolve(e)
    const mine = toTree(el, model, state.last, await $.clock.now(), e.props && e.props.bodyColumns, e.props && e.props.maxRows, state.noColor, pluginBehind(state.ctVersion))
    return el.Box({ flexDirection: 'column', children: others ? [mine, others] : [mine] })
  })

  // A refused connector call: say which account it went out as.
  on('tool.call', { tool: /^mcp__claude_ai_/ }, async ($, e, next) => {
    const result = await next(e)
    const doc = state.last
    if (!state.isChottag || !doc || !doc.remote || !result) return result
    const body = typeof result.result === 'string' ? result.result : typeof result.deny === 'string' ? result.deny : ''
    if (!REFUSAL.test(body) || !(result.isError || typeof result.deny === 'string' || body.length <= SHORT_RESULT)) return result
    const note = '(chottag sent this connector call as account ' + doc.remote + ', the remote account of pool ' + (doc.pool || 'default') + ')'
    if (typeof result.result === 'string') return { ...result, result: result.result + '\n' + note }
    await $.ui.toast(note) // a result of another shape cannot carry the line
    return result
  })
}
