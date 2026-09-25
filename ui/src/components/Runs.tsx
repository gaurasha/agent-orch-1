import { useEffect, useState } from 'react'
import { api } from '../api'
import type { AgentDefinition, Run } from '../types'
import { Badge, ErrorNotice, fmtAge, fmtUSD } from './common'

export default function Runs({ onOpen }: { onOpen: (id: string) => void }) {
  const [runs, setRuns] = useState<Run[]>([])
  const [agents, setAgents] = useState<AgentDefinition[]>([])
  const [filter, setFilter] = useState('')
  const [error, setError] = useState<unknown>(null)
  const [launching, setLaunching] = useState(false)
  const [selectedAgent, setSelectedAgent] = useState('')

  useEffect(() => {
    let alive = true
    const load = () =>
      api.runs({ state: filter || undefined, limit: 200 }).then(
        (d) => { if (alive) { setRuns(d.runs ?? []); setError(null) } },
        (e) => { if (alive) setError(e) },
      )
    load()
    const t = setInterval(load, 1500)
    return () => { alive = false; clearInterval(t) }
  }, [filter])

  useEffect(() => {
    api.agents().then((d) => {
      setAgents(d.agents ?? [])
      if (d.agents?.[0]) setSelectedAgent(d.agents[0].digest)
    }, setError)
  }, [])

  const launch = async () => {
    if (!selectedAgent) return
    setLaunching(true)
    try {
      const run = await api.createRun({ agent_digest: selectedAgent, input: 'Begin.' })
      onOpen(run.id)
    } catch (e) {
      setError(e)
    } finally {
      setLaunching(false)
    }
  }

  return (
    <div>
      <h1>Runs</h1>
      <p className="subtitle">Every agent run this key can see, newest first.</p>
      <ErrorNotice error={error} />

      <div className="row" style={{ marginBottom: 14 }}>
        <select value={filter} onChange={(e) => setFilter(e.target.value)}>
          <option value="">All states</option>
          <option value="QUEUED,RUNNING">Active</option>
          <option value="WAITING_HUMAN,WAITING_APPROVAL">Waiting on a human</option>
          <option value="SUCCEEDED">Succeeded</option>
          <option value="FAILED">Failed</option>
        </select>
        <div className="spacer" />
        <select value={selectedAgent} onChange={(e) => setSelectedAgent(e.target.value)}>
          {agents.map((a) => (
            <option key={a.digest} value={a.digest}>
              {a.tenant_id} / {a.name}
            </option>
          ))}
        </select>
        <button className="action primary" onClick={launch} disabled={launching || !selectedAgent}>
          {launching ? 'Starting…' : 'Launch run'}
        </button>
      </div>

      <table>
        <thead>
          <tr>
            <th>State</th><th>Agent</th><th>Tenant</th><th>Triggered by</th>
            <th className="num">Steps</th><th className="num">Tools</th>
            <th className="num">Cost</th><th>Worker</th><th>Age</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id} onClick={() => onOpen(r.id)} style={{ cursor: 'pointer' }}>
              <td><Badge kind={r.state} /></td>
              <td>{r.agent_name}</td>
              <td className="mono">{r.tenant_id}</td>
              <td className="mono">{r.triggering_user}</td>
              <td className="num">{r.usage.steps}</td>
              <td className="num">{r.usage.tool_calls}</td>
              <td className="num">{fmtUSD(r.usage.cost_usd)}</td>
              {/* An empty worker on a parked run is the point: it holds nothing. */}
              <td className="mono">{r.lease_owner || '-'}</td>
              <td>{fmtAge(r.created_at)}</td>
            </tr>
          ))}
          {runs.length === 0 && (
            <tr><td colSpan={9} className="empty">No runs match this filter.</td></tr>
          )}
        </tbody>
      </table>
    </div>
  )
}
