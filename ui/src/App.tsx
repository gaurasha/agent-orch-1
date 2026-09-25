import { useState } from 'react'
import Overview from './components/Overview'
import Runs from './components/Runs'
import RunDetail from './components/RunDetail'
import Audit from './components/Audit'
import Quota from './components/Quota'
import { getApiKey, setApiKey } from './api'

type View = 'overview' | 'runs' | 'audit' | 'quota'

const KEYS = [
  { key: 'demo-operator-key', label: 'operator (all tenants)' },
  { key: 'acme-key', label: 'alice @ acme' },
  { key: 'globex-key', label: 'bob @ globex' },
]

export default function App() {
  const [view, setView] = useState<View>('overview')
  const [runId, setRunId] = useState<string | null>(null)
  const [apiKey, setKey] = useState(getApiKey())

  // Switching key changes both the identity and the tenant scope. Remounting
  // the whole tree (via the `key` prop) rather than trying to invalidate
  // caches guarantees no other tenant's data survives the switch on screen -
  // which is the kind of leak a console must not have.
  const changeKey = (k: string) => {
    setApiKey(k)
    setKey(k)
    setRunId(null)
  }

  const nav = (v: View) => { setView(v); setRunId(null) }

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          Agent Orchestration
          <small>operator console</small>
        </div>
        <nav className="nav">
          <button className={view === 'overview' && !runId ? 'active' : ''} onClick={() => nav('overview')}>Overview</button>
          <button className={view === 'runs' ? 'active' : ''} onClick={() => nav('runs')}>Runs</button>
          <button className={view === 'quota' && !runId ? 'active' : ''} onClick={() => nav('quota')}>Quota &amp; fairness</button>
          <button className={view === 'audit' && !runId ? 'active' : ''} onClick={() => nav('audit')}>Audit log</button>
        </nav>
        <div className="keybox">
          <label htmlFor="apikey">Acting as</label>
          <select id="apikey" value={apiKey} onChange={(e) => changeKey(e.target.value)}>
            {KEYS.map((k) => <option key={k.key} value={k.key}>{k.label}</option>)}
          </select>
        </div>
      </aside>

      <main className="main" key={apiKey}>
        {runId
          ? <RunDetail id={runId} onBack={() => setRunId(null)} />
          : view === 'overview' ? <Overview />
          : view === 'runs' ? <Runs onOpen={setRunId} />
          : view === 'quota' ? <Quota />
          : <Audit />}
      </main>
    </div>
  )
}
