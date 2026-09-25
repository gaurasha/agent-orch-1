import type { ReactNode } from 'react'

export function Badge({ kind }: { kind: string }) {
  return <span className={`badge ${kind}`}>{kind.replace(/_/g, ' ').toLowerCase()}</span>
}

export function Card({ label, value, small }: { label: string; value: ReactNode; small?: boolean }) {
  return (
    <div className="card">
      <div className="label">{label}</div>
      <div className={`value${small ? ' small' : ''}`}>{value}</div>
    </div>
  )
}

// Meter shows usage against a limit. Colour crosses to warn at 70% and error at
// 90% so an operator sees a run approaching its budget before it is killed,
// rather than finding out from the failure.
export function Meter({ used, limit }: { used: number; limit: number }) {
  if (!limit) return <span className="mono">{used} / unlimited</span>
  const pct = Math.min(100, (used / limit) * 100)
  const cls = pct >= 90 ? 'err' : pct >= 70 ? 'warn' : ''
  return (
    <div>
      <div className="bar"><span className={cls} style={{ width: `${pct}%` }} /></div>
      <div className="mono" style={{ fontSize: 11, color: 'var(--muted)', marginTop: 3 }}>
        {fmtNum(used)} / {fmtNum(limit)}
      </div>
    </div>
  )
}

export function fmtNum(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`
  return String(Math.round(n * 100) / 100)
}

export function fmtUSD(n: number): string {
  if (n === 0) return '$0'
  if (n < 0.01) return `$${n.toFixed(5)}`
  return `$${n.toFixed(2)}`
}

export function fmtAge(iso?: string): string {
  if (!iso) return '-'
  const ms = Date.now() - new Date(iso).getTime()
  if (ms < 1000) return 'now'
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
}

export function fmtTime(iso?: string): string {
  if (!iso) return '-'
  return new Date(iso).toLocaleTimeString([], { hour12: false })
}

export function ErrorNotice({ error }: { error: unknown }) {
  if (!error) return null
  const msg = error instanceof Error ? error.message : String(error)
  return <div className="notice err">{msg}</div>
}
