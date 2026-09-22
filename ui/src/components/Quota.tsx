import { useEffect, useState } from 'react'
import { api } from '../api'
import type { TenantQuota } from '../types'
import { ErrorNotice, fmtNum } from './common'

// The fairness panel. "Which tenant is being throttled right now, and why"
// should be one glance during an incident, not a log dig.
export default function Quota() {
  const [tenants, setTenants] = useState<TenantQuota[]>([])
  const [spare, setSpare] = useState(0)
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    let alive = true
    const load = () => api.quota().then((d) => {
      if (!alive) return
      setTenants(d.tenants ?? [])
      setSpare(d.spare_tokens ?? 0)
      setError(null)
    }, (e) => { if (alive) setError(e) })
    load()
    const t = setInterval(load, 1000)
    return () => { alive = false; clearInterval(t) }
  }, [])

  return (
    <div>
      <h1>LLM quota and fairness</h1>
      <p className="subtitle">
        The provider quota is the scarcest and most expensive resource. Each tenant has a
        guaranteed share proportional to its weight; unused capacity flows into a shared
        burst pool. A tenant can never be starved by another's load.
      </p>
      <ErrorNotice error={error} />

      <div className="notice">
        Shared burst pool available now: <strong className="mono">{fmtNum(spare)}</strong> tokens.
        This is capacity nobody's guarantee is currently claiming.
      </div>

      <table>
        <thead>
          <tr>
            <th>Tenant</th><th className="num">Weight</th><th className="num">Guaranteed TPM</th>
            <th style={{ width: 240 }}>Bucket</th><th className="num">In flight</th><th>Status</th>
          </tr>
        </thead>
        <tbody>
          {tenants.map((t) => {
            const pct = t.capacity_tokens > 0 ? (t.available_now / t.capacity_tokens) * 100 : 0
            const cls = pct < 25 ? 'err' : pct < 50 ? 'warn' : ''
            return (
              <tr key={t.tenant_id}>
                <td className="mono">{t.tenant_id}</td>
                <td className="num">{t.weight}</td>
                <td className="num">{fmtNum(t.guaranteed_tpm)}</td>
                <td>
                  <div className="bar"><span className={cls} style={{ width: `${pct}%` }} /></div>
                  <div className="mono" style={{ fontSize: 11, color: 'var(--muted)', marginTop: 3 }}>
                    {fmtNum(t.available_now)} / {fmtNum(t.capacity_tokens)} tokens available
                  </div>
                </td>
                <td className="num">{t.in_flight}</td>
                <td>
                  {t.throttled
                    ? <span className="badge FAILED">throttled</span>
                    : <span className="badge SUCCEEDED">admitting</span>}
                </td>
              </tr>
            )
          })}
          {tenants.length === 0 && <tr><td colSpan={6} className="empty">No tenants configured.</td></tr>}
        </tbody>
      </table>
    </div>
  )
}
