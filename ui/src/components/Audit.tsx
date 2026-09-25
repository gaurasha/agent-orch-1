import { useEffect, useState } from 'react'
import { api } from '../api'
import type { AuditRecord } from '../types'
import { Badge, ErrorNotice, fmtTime } from './common'

// The auditor's view. The important element here is the chain verification
// banner: an audit log you cannot check is just a log.
export default function Audit() {
  const [records, setRecords] = useState<AuditRecord[]>([])
  const [verified, setVerified] = useState<boolean | undefined>()
  const [chainError, setChainError] = useState<string | undefined>()
  const [tenant, setTenant] = useState('acme')
  const [toolFilter, setToolFilter] = useState('')
  const [error, setError] = useState<unknown>(null)

  useEffect(() => {
    let alive = true
    const load = () => api.audit({ tenant, limit: 500 }).then((d) => {
      if (!alive) return
      setRecords(d.records ?? [])
      setVerified(d.chain_verified)
      setChainError(d.chain_error)
      setError(null)
    }, (e) => { if (alive) setError(e) })
    load()
    const t = setInterval(load, 3000)
    return () => { alive = false; clearInterval(t) }
  }, [tenant])

  const shown = toolFilter
    ? records.filter((r) => r.tool.includes(toolFilter))
    : records

  return (
    <div>
      <h1>Audit log</h1>
      <p className="subtitle">
        Every tool call, allowed or denied, attributed to an agent, a tenant and the
        human who triggered it. Records are hash-chained per tenant.
      </p>
      <ErrorNotice error={error} />

      {verified === true && (
        <div className="notice ok">
          Hash chain verified over {records.length} records. No record has been
          modified, reordered or removed since it was written.
        </div>
      )}
      {verified === false && (
        <div className="notice err">
          CHAIN VERIFICATION FAILED — {chainError}
        </div>
      )}

      <div className="row" style={{ marginBottom: 14 }}>
        <select value={tenant} onChange={(e) => setTenant(e.target.value)}>
          <option value="acme">acme</option>
          <option value="globex">globex</option>
        </select>
        <input placeholder="filter by tool, e.g. exec."
               value={toolFilter} onChange={(e) => setToolFilter(e.target.value)} />
        <span className="ev-meta">{shown.length} of {records.length} records</span>
      </div>

      <table>
        <thead>
          <tr>
            <th className="num">#</th><th>Time</th><th>Decision</th><th>Tool</th>
            <th>Agent</th><th>Triggered by</th><th>Arguments</th><th>Reason</th>
          </tr>
        </thead>
        <tbody>
          {shown.map((r) => (
            <tr key={`${r.tenant_id}-${r.seq}`}>
              <td className="num mono">{r.seq}</td>
              <td className="mono">{fmtTime(r.ts)}</td>
              <td><Badge kind={r.decision} /></td>
              <td className="mono">{r.tool}</td>
              <td>{r.agent_name}</td>
              <td className="mono">{r.triggering_user}</td>
              <td className="mono" style={{ maxWidth: 340, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                {JSON.stringify(r.args_redacted ?? {}).slice(0, 160)}
              </td>
              <td style={{ color: 'var(--muted)', maxWidth: 260 }}>{r.reason}</td>
            </tr>
          ))}
          {shown.length === 0 && <tr><td colSpan={8} className="empty">No audit records.</td></tr>}
        </tbody>
      </table>
    </div>
  )
}
