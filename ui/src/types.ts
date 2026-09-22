// Mirrors the Go types in backend/internal/types. Kept hand-written rather
// than generated: the API surface is small, and a generator would be a build
// dependency for very little benefit. If this drifts, the typecheck in CI
// catches the shape but not the semantics - so treat backend/internal/types
// as the source of truth.

export type RunState =
  | 'QUEUED' | 'RUNNING' | 'WAITING_HUMAN' | 'WAITING_APPROVAL'
  | 'SUCCEEDED' | 'FAILED' | 'CANCELLED'

export type Priority = 'interactive' | 'normal' | 'batch'

export interface Usage {
  steps: number
  tool_calls: number
  input_tokens: number
  output_tokens: number
  cost_usd: number
  sandbox_ms: number
}

export interface Budget {
  max_steps: number
  max_tool_calls: number
  max_tokens: number
  max_cost_usd: number
  max_wall_seconds: number
}

export interface Run {
  id: string
  tenant_id: string
  agent_name: string
  def_digest: string
  triggering_user: string
  state: RunState
  status_reason?: string
  next_seq: number
  step: number
  budget: Budget
  usage: Usage
  priority: Priority
  lease_owner?: string
  lease_expires_at?: string
  wake_at?: string
  created_at: string
  updated_at: string
}

export type EventType =
  | 'RUN_CREATED' | 'USER_MESSAGE' | 'MODEL_REQUEST' | 'MODEL_RESPONSE'
  | 'TOOL_CALL' | 'TOOL_RESULT' | 'TOOL_DENIED' | 'APPROVAL_NEEDED'
  | 'APPROVAL_GIVEN' | 'HUMAN_PAUSE' | 'HUMAN_RESUME' | 'LEASE_LOST'
  | 'RUN_FINISHED' | 'NOTE'

export interface EventPayload {
  text?: string
  model?: string
  input_tokens?: number
  output_tokens?: number
  cost_usd?: number
  stop_reason?: string
  tool_call_id?: string
  tool?: string
  args?: Record<string, unknown>
  idem_key?: string
  result?: string
  is_error?: boolean
  duration_ms?: number
  decision?: string
  reason?: string
  worker?: string
}

export interface RunEvent {
  run_id: string
  seq: number
  type: EventType
  payload: EventPayload
  created_at: string
}

export interface AgentDefinition {
  digest: string
  tenant_id: string
  name: string
  spec: {
    system_prompt: string
    model: string
    tools: string[]
    tool_params?: Record<string, unknown>
    budget: Budget
    priority: Priority
  }
  created_at: string
}

export interface TenantQuota {
  tenant_id: string
  weight: number
  guaranteed_tpm: number
  available_now: number
  capacity_tokens: number
  utilization_pct: number
  in_flight: number
  throttled: boolean
}

export interface AuditRecord {
  tenant_id: string
  seq: number
  ts: string
  run_id: string
  agent_name: string
  triggering_user: string
  tool: string
  decision: string
  reason?: string
  args_redacted?: Record<string, unknown>
  result_meta?: Record<string, unknown>
  prev_hash: string
  hash: string
}

export interface Overview {
  runs_by_state: Record<string, number>
  by_tenant: Record<string, { runs: number; cost_usd: number; tokens: number; tool_calls: number }>
  total_cost_usd: number
  total_tokens: number
  quota: TenantQuota[]
  spare_tokens: number
  generated_at: string
}
