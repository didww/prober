// Small formatters for the trace table.

// Microseconds as milliseconds, at a fixed precision so a column of them lines
// up. Null (a lost or pending probe) shows as a dash.
export function ms(us: number | null | undefined): string {
  if (us == null) return '—'
  return (us / 1000).toFixed(us < 10000 ? 1 : 0)
}

export function lossPct(v: number): string {
  if (v === 0) return '0'
  if (v === 100) return '100'
  return v.toFixed(v < 10 ? 1 : 0)
}

// A colour for an RTT cell, green (fast) through amber to red (slow), plus a
// distinct colour for loss. Returns a CSS custom-property-driven class hint via
// an inline style string kept out of the component.
export function rttColor(us: number | null): string {
  if (us == null) return 'var(--cell-loss)'
  const msVal = us / 1000
  // 0–40ms good, 40–150ms fair, 150ms+ poor, on a smooth ramp.
  if (msVal <= 40) return 'var(--cell-good)'
  if (msVal <= 100) return 'var(--cell-ok)'
  if (msVal <= 200) return 'var(--cell-warn)'
  return 'var(--cell-bad)'
}

// --- mtr-style report -------------------------------------------------------

import type { SiteState } from './runStore'

function rjust(s: string, w: number): string {
  return s.length >= w ? s : ' '.repeat(w - s.length) + s
}
function ljust(s: string, w: number): string {
  return s.length >= w ? s : s + ' '.repeat(w - s.length)
}
function ms1(us: number): string {
  return (us / 1000).toFixed(1)
}

// A local-time timestamp with a numeric offset, like mtr's Start line:
// "2026-09-16T23:45:12+0300".
function startStamp(d = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0')
  const off = -d.getTimezoneOffset()
  const sign = off >= 0 ? '+' : '-'
  const oh = p(Math.floor(Math.abs(off) / 60))
  const om = p(Math.abs(off) % 60)
  return (
    `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}` +
    `T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}${sign}${oh}${om}`
  )
}

// mtrReport renders one site's hops the way `mtr --report` prints them, so the
// text pasted into a ticket reads exactly like a run of the tool. The HOST line
// carries the site, since that is the vantage the trace ran from.
export function mtrReport(s: SiteState): string {
  const host = (h: SiteState['hops'][number]) =>
    h.addresses.length ? h.addresses[0].name || h.addresses[0].ip : '???'

  const names = s.hops.map(host)
  // The vantage: the site, and the source address the probes left from.
  const src = s.source ? `${s.site} (${s.source})` : s.site
  const hostW = Math.max(20, src.length, ...names.map((n) => n.length))

  const cols = (loss: string, snt: string, rcvd: string, last: string, avg: string, best: string, wrst: string, dev: string) =>
    rjust(loss, 6) + rjust(snt, 6) + rjust(rcvd, 6) + rjust(last, 6) + rjust(avg, 6) + rjust(best, 6) + rjust(wrst, 6) + rjust(dev, 6)

  const lines: string[] = []
  lines.push(`Start: ${startStamp()}`)
  lines.push(ljust('HOST: ' + src, 8 + hostW) + cols('Loss%', 'Snt', 'Rcvd', 'Last', 'Avg', 'Best', 'Wrst', 'StDev'))

  for (const h of s.hops) {
    const prefix = rjust(String(h.ttl), 3) + '.|-- '
    lines.push(
      prefix +
        ljust(host(h), hostW) +
        cols(
          `${h.loss_pct.toFixed(1)}%`,
          String(h.sent),
          String(h.received),
          ms1(h.last_us),
          ms1(h.avg_us),
          ms1(h.best_us),
          ms1(h.worst_us),
          ms1(h.stdev_us),
        ),
    )
  }
  return lines.join('\n') + '\n'
}

// copyText writes to the clipboard, falling back to a hidden textarea where the
// async Clipboard API is unavailable (older browsers, or a non-secure context
// that is not localhost). Returns whether it succeeded.
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* fall through to the textarea */
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.position = 'fixed'
    ta.style.opacity = '0'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}
