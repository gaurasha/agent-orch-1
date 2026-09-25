import type {
  AgentDefinition, AuditRecord, Overview, Run, RunEvent, TenantQuota,
} from './types'

// The API key identifies the caller AND its tenant. It is held in
// localStorage for the demo; a real console would use an OIDC session and
// never see a long-lived credential at all.
const KEY_STORAGE = 'agentorch.apiKey'

export function getApiKey(): string {
  return localStorage.getItem(KEY_STORAGE) ?? 'demo-operator-key'
}

export function setApiKey(k: string) {
  localStorage.setItem(KEY_STORAGE, k)
}

export class ApiError extends Error {
  constructor(message: string, readonly status: number) {
    super(message)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${getApiKey()}`,
      ...(init?.headers ?? {}),
    },
  })
  if (!res.ok) {
    // Surface the server's own message: it is written for a human and is
    // almost always the actual reason (quota, policy, tenancy).
    let detail = res.statusText
    try {
      const body = await res.json()
      if (body?.error) detail = body.error
    } catch {
      /* non-JSON error body; keep the status text */
    }
    throw new ApiError(detail, res.status)
  }
  return res.json() as Promise<T>
}

export const api = {
  overview: () => request<Overview>('/v1/overview'),
  runs: (params: { state?: string; tenant?: string; limit?: number } = {}) => {
    const q = new URLSearchParams()
    if (params.state) q.set('state', params.state)
    if (params.tenant) q.set('tenant', params.tenant)
    q.set('limit', String(params.limit ?? 100))
    return request<{ runs: Run[] }>(`/v1/runs?${q}`)
  },
  run: (id: string) => request<Run>(`/v1/runs/${id}`),
  events: (id: string, since = 0) =>
    request<{ events: RunEvent[]; run: Run }>(`/v1/runs/${id}/events?since=${since}`),
  agents: (tenant?: string) =>
    request<{ agents: AgentDefinition[] }>(`/v1/agents${tenant ? `?tenant=${tenant}` : ''}`),
  createRun: (body: { agent_digest?: string; agent_name?: string; input?: string }) =>
    request<Run>('/v1/runs', { method: 'POST', body: JSON.stringify(body) }),
  resume: (id: string, input: string) =>
    request<{ status: string }>(`/v1/runs/${id}/resume`, {
      method: 'POST', body: JSON.stringify({ input }),
    }),
  approve: (id: string) =>
    request<{ status: string }>(`/v1/runs/${id}/approve`, { method: 'POST', body: '{}' }),
  cancel: (id: string) =>
    request<{ status: string }>(`/v1/runs/${id}/cancel`, { method: 'POST', body: '{}' }),
  audit: (params: { tenant?: string; run_id?: string; limit?: number } = {}) => {
    const q = new URLSearchParams()
    if (params.tenant) q.set('tenant', params.tenant)
    if (params.run_id) q.set('run_id', params.run_id)
    q.set('limit', String(params.limit ?? 300))
    return request<{
      records: AuditRecord[]; count: number
      chain_verified?: boolean; chain_error?: string
    }>(`/v1/audit?${q}`)
  },
  quota: () => request<{ tenants: TenantQuota[]; spare_tokens: number }>('/v1/quota'),
}

// streamRun opens an SSE connection for live run updates.
//
// EventSource cannot set headers, so the API key travels as a query parameter
// on this one route. That is a real weakness (keys land in access logs) and is
// called out in the README's known gaps; the production answer is a short-lived
// signed stream token minted by the control plane.
export function streamRun(
  id: string,
  handlers: { onRun?: (r: Run) => void; onEvent?: (e: RunEvent) => void; onDone?: () => void },
): () => void {
  const es = new EventSource(`/v1/runs/${id}/stream?api_key=${encodeURIComponent(getApiKey())}`)
  es.addEventListener('run', (m) => handlers.onRun?.(JSON.parse((m as MessageEvent).data)))
  es.addEventListener('event', (m) => handlers.onEvent?.(JSON.parse((m as MessageEvent).data)))
  es.addEventListener('done', () => { handlers.onDone?.(); es.close() })
  es.onerror = () => {
    // EventSource retries on its own; closing here would defeat that. Only a
    // fully closed stream is terminal.
    if (es.readyState === EventSource.CLOSED) handlers.onDone?.()
  }
  return () => es.close()
}
