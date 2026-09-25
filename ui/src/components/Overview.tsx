import { useEffect, useState } from 'react'
import { api } from '../api'
import type { Overview as OverviewData } from '../types'
import { Card, ErrorNotice, fmtNum, fmtUSD } from './common'

// The dashboard answers the operator question from the brief at the fleet
// level: what is running, and what has it cost.
export default function Overview() {
  const [data, setData] = useState<OverviewData | null>(null)
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    let alive = true
    const load = () => api.overview().then(
      (d) => { if (alive) { setData(d); setError(null) } },
      (e) => { if (alive) setError(e) },
    )
    load()
    const t = setInterval(load, 2000)
    return () => { alive = false; clearInterval(t) }
  }, [])

  if (error && !data) return <ErrorNotice error={error} />
  if (!data) return <div className="empty">Loading…</div>

  const states = data.runs_by_state ?? {}
  const active = (states.RUNNING ?? 0) + (states.QUEUED ?? 0)
  const waiting = (states.WAITING_HUMAN ?? 0) + (states.WAITING_APPROVAL ?? 0)

  return (
    <div>
      <h1>Fleet overview</h1>
      <p className="subtitle">
        Live state across every tenant this key can see. Refreshes every 2 seconds.
      </p>
      <ErrorNotice error={error} />

      <div className="cards">
        <Card label="Active" value={active} />
        <Card label="Waiting on a human" value={waiting} />
        <Card label="Succeeded" value={states.SUCCEEDED ?? 0} />
        <Card label="Failed" value={states.FAILED ?? 0} />
        <Card label="Total spend" value={fmtUSD(data.total_cost_usd)} small />
        <Card label="Tokens" value={fmtNum(data.total_tokens)} small />
      </div>

      <h2>Cost by tenant</h2>
      <table>
        <thead>
          <tr><th>Tenant</th><th className="num">Runs</th><th className="num">Tool calls</th>
              <th className="num">Tokens</th><th className="num">Spend</th></tr>
        </thead>
        <tbody>
          {Object.entries(data.by_tenant ?? {}).map(([tenant, v]) => (
            <tr key={tenant}>
              <td className="mono">{tenant}</td>
              <td className="num">{v.runs}</td>
              <td className="num">{v.tool_calls}</td>
              <td className="num">{fmtNum(v.tokens)}</td>
              <td className="num">{fmtUSD(v.cost_usd)}</td>
            </tr>
          ))}
          {Object.keys(data.by_tenant ?? {}).length === 0 && (
            <tr><td colSpan={5} className="empty">No runs yet.</td></tr>
          )}
        </tbody>
      </table>

      <h2>Runs by state</h2>
      <div className="cards">
        {Object.entries(states).map(([s, n]) => <Card key={s} label={s} value={n} />)}
        {Object.keys(states).length === 0 && <div className="empty">No runs yet.</div>}
      </div>
    </div>
  )
}
