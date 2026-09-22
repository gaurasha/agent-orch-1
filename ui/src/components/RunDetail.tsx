import { useEffect, useRef, useState } from 'react'
import { api, streamRun } from '../api'
import type { Run, RunEvent } from '../types'
import { Badge, ErrorNotice, Meter, fmtNum, fmtTime, fmtUSD } from './common'

// The run detail view is the "what is agent X doing right now" answer. It is
// driven by the same event log the agent's model context is rebuilt from, so
// what an operator sees is literally what the agent saw - not a parallel
// telemetry stream that can drift from reality.
export default function RunDetail({ id, onBack }: { id: string; onBack: () => void }) {
  const [run, setRun] = useState<Run | null>(null)
  const [events, setEvents] = useState<RunEvent[]>([])
  const [error, setError] = useState<unknown>(null)
  const [resumeText, setResumeText] = useState('Approved. Please continue.')
  const [busy, setBusy] = useState(false)
  const seen = useRef(new Set<number>())

  useEffect(() => {
    seen.current = new Set()
    setEvents([])
    let alive = true

    api.events(id).then((d) => {
      if (!alive) return
      setRun(d.run)
      for (const e of d.events) seen.current.add(e.seq)
      setEvents(d.events)
    }, setError)

    const close = streamRun(id, {
      onRun: (r) => { if (alive) setRun(r) },
      onEvent: (e) => {
        if (!alive || seen.current.has(e.seq)) return
        seen.current.add(e.seq)
        setEvents((prev) => [...prev, e].sort((a, b) => a.seq - b.seq))
      },
    })
    return () => { alive = false; close() }
  }, [id])

  const act = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    try { await fn(); setError(null) } catch (e) { setError(e) } finally { setBusy(false) }
  }

  if (error && !run) return <><button className="back" onClick={onBack}>← Runs</button><ErrorNotice error={error} /></>
  if (!run) return <div className="empty">Loading…</div>

  const elapsed = (Date.now() - new Date(run.created_at).getTime()) / 1000

  return (
    <div>
      <button className="back" onClick={onBack}>← Runs</button>
      <h1 style={{ marginTop: 8 }}>
        {run.agent_name} <Badge kind={run.state} />
      </h1>
      <p className="subtitle mono">
        {run.id} · tenant {run.tenant_id} · triggered by {run.triggering_user}
        {run.lease_owner ? ` · worker ${run.lease_owner}` : ' · no worker holding a lease'}
      </p>
      {run.status_reason && <div className="notice">{run.status_reason}</div>}
      <ErrorNotice error={error} />

      <div className="row" style={{ marginBottom: 16 }}>
        {run.state === 'WAITING_HUMAN' && (
          <>
            <input
              value={resumeText}
              onChange={(e) => setResumeText(e.target.value)}
              style={{ minWidth: 340 }}
              placeholder="Your reply to the agent"
            />
            <button className="action primary" disabled={busy}
                    onClick={() => act(() => api.resume(run.id, resumeText))}>
              Resume
            </button>
          </>
        )}
        {run.state === 'WAITING_APPROVAL' && (
          <button className="action primary" disabled={busy}
                  onClick={() => act(() => api.approve(run.id))}>
            Approve the held tool call
          </button>
        )}
        {!['SUCCEEDED', 'FAILED', 'CANCELLED'].includes(run.state) && (
          <button className="action danger" disabled={busy}
                  onClick={() => act(() => api.cancel(run.id))}>
            Cancel
          </button>
        )}
      </div>

      <h2>Cost and budget</h2>
      <div className="cards">
        <div className="card">
          <div className="label">Spend</div>
          <div className="value small">{fmtUSD(run.usage.cost_usd)}</div>
          <Meter used={run.usage.cost_usd} limit={run.budget.max_cost_usd} />
        </div>
        <div className="card">
          <div className="label">Steps</div>
          <div className="value small">{run.usage.steps}</div>
          <Meter used={run.usage.steps} limit={run.budget.max_steps} />
        </div>
        <div className="card">
          <div className="label">Tool calls</div>
          <div className="value small">{run.usage.tool_calls}</div>
          <Meter used={run.usage.tool_calls} limit={run.budget.max_tool_calls} />
        </div>
        <div className="card">
          <div className="label">Tokens</div>
          <div className="value small">{fmtNum(run.usage.input_tokens + run.usage.output_tokens)}</div>
          <Meter used={run.usage.input_tokens + run.usage.output_tokens} limit={run.budget.max_tokens} />
        </div>
        <div className="card">
          <div className="label">Elapsed</div>
          <div className="value small">{Math.round(elapsed)}s</div>
          <Meter used={elapsed} limit={run.budget.max_wall_seconds} />
        </div>
      </div>

      <h2>Timeline ({events.length} events)</h2>
      <div className="timeline">
        {events.map((e) => <EventRow key={e.seq} ev={e} />)}
        {events.length === 0 && <div className="empty">No events yet.</div>}
      </div>
    </div>
  )
}

function EventRow({ ev }: { ev: RunEvent }) {
  const p = ev.payload
  const isErr = p.is_error || ev.type === 'TOOL_DENIED'
  return (
    <div className={`ev ${ev.type}${isErr ? ' error' : ''}`}>
      <div className="ev-head">
        <span className="ev-type">{ev.type.replace(/_/g, ' ')}</span>
        {p.tool && <span className="mono">{p.tool}</span>}
        {p.decision && <Badge kind={p.decision} />}
        <span className="spacer" />
        <span className="ev-meta">
          {p.model && `${p.model} · `}
          {p.cost_usd ? `${fmtUSD(p.cost_usd)} · ` : ''}
          {p.duration_ms != null ? `${p.duration_ms}ms · ` : ''}
          #{ev.seq} · {fmtTime(ev.created_at)}
        </span>
      </div>
      {p.text && <div>{p.text}</div>}
      {p.reason && <div style={{ color: 'var(--warn)', fontSize: 12 }}>{p.reason}</div>}
      {p.args && Object.keys(p.args).length > 0 && (
        <pre>{JSON.stringify(p.args, null, 2)}</pre>
      )}
      {p.result && <pre>{p.result}</pre>}
      {p.worker && <div className="ev-meta" style={{ marginTop: 4 }}>worker: {p.worker}</div>}
    </div>
  )
}
