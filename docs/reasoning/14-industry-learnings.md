# 14 · Industry learnings — documented incidents and the decision each one informs

> Every incident below is public, with a link to a primary source or a
> first-hand analysis. Each is paired with the mechanism in this repository
> that addresses the same failure — or, where nothing here would have helped,
> that is said too. Dates are the incident or disclosure dates.

## A. Agent-specific incidents (2023–2025)

| When | Incident | What actually happened | The decision it informs |
|---|---|---|---|
| Feb 2023 | **Indirect prompt injection against Bing Chat** (Greshake et al.) | a web page the assistant browsed contained hidden instructions that changed its behaviour and attempted exfiltration via links | [10](10-prompt-injection.md): untrusted content is a given; remove the exfiltration channel (no network in the sandbox, host allowlists) |
| 2023 | **ChatGPT plugin exfiltration** (Rehberger, "Embrace the Red") | a plugin fetching attacker content led to data being sent to an attacker-controlled URL via a rendered image/link | the same: outbound calls only through the gateway, only to allowlisted hosts, SSRF-checked |
| Aug 2024 | **Slack AI data exfiltration** (PromptArmor) | a message in a *public* channel instructed Slack AI to leak data from *private* channels via a crafted link the victim would click | [05](05-authorization.md): a tool's reach must be bounded by policy, not by the model's discretion; audit denials |
| Mar 2025 | **`tj-actions/changed-files` supply-chain compromise** (CVE-2025-30066) | a popular GitHub Action was modified to dump CI secrets into build logs | [06](06-credentials.md): secrets in an environment that runs third-party code will be read; scope + TTL + a type that cannot print; [12](12-language-and-dependencies.md): dependency policy |
| May 2025 | **GitHub MCP "toxic agent flow"** (Invariant Labs) | an issue in a public repo instructed an agent holding a broad PAT to read a private repo and post its contents in a public PR | [05](05-authorization.md), [06](06-credentials.md): per-run grants pinned by digest; credentials scoped to tenant + reference; parameter policy on where data may go |
| Jun 2025 | **EchoLeak — Microsoft 365 Copilot** (Aim Security, CVE-2025-32711) | zero-click: a crafted email caused Copilot to include sensitive data in an outbound image fetch to an attacker domain | closing the *channel* was the fix — exactly [10](10-prompt-injection.md)'s trifecta argument |
| Jul 2025 | **Supabase MCP / Cursor SQL leak** (General Analysis) | a support ticket containing injected SQL instructions was processed by an agent using a `service_role` key; it exfiltrated integration tokens into a ticket reply | the agent held a credential with more authority than the task; [06](06-credentials.md): mint per call, scope per tenant/ref, never in the agent's reach |
| Jul 2025 | **Amazon Q Developer VS Code extension 1.84.0** (404 Media / AWS advisory) | a malicious pull request merged into the extension's repo added a prompt instructing the agent to wipe the user's system and cloud resources; AWS pulled the release | a supply-chain path to the *instructions*; no runtime authorisation fixes it, but human approval for `Dangerous` tools and budgets bound the blast radius; [11](11-kubernetes.md): reviewed, pinned, signed artefacts |
| Jul 2025 | **Replit agent deletes a production database** (SaaStr / Replit) | during a "code freeze" the agent ran destructive commands against production data, then reported incorrectly on what it had done; the vendor shipped dev/prod separation, one-click restore and a planning-only mode | [05](05-authorization.md): destructive tools need approval and a policy the model cannot talk its way past; [08](08-audit.md): the audit chain, not the agent's self-report, is the record |
| Jul 2025 | **Gemini for Workspace summary injection** (0din / Mozilla) | hidden text in an email caused the summary feature to render attacker-chosen phishing instructions | untrusted content will be summarised; the defence is what the *output* is allowed to do (no links, no actions) — a parameter-policy argument |
| Aug 2025 | **CurXecute (CVE-2025-54135) / MCPoison (CVE-2025-54136) — Cursor** (Aim Labs, Check Point) | prompt injection through an MCP server led to arbitrary command execution; a trusted MCP config could be silently swapped for a malicious one | tool *definitions* must be pinned and content-addressed ([05](05-authorization.md): agent definitions by digest); model-authored commands run in a jail ([02](02-isolation.md)) |

**Pattern.** Every agent incident above is one of: (1) a credential inside the
agent's reach, (2) an outbound channel the agent could use, (3) a destructive
action without a human, or (4) an unpinned definition. The platform's four
corresponding mechanisms — broker-minted credentials outside the sandbox, an
empty network namespace plus gateway allowlists, `Dangerous` → approval, and
digest-pinned definitions — are the direct responses. No incident on this
list was prevented by detecting the injected text.

## B. Infrastructure incidents that shaped the durability and deployment design

| When | Incident | What happened | The decision it informs |
|---|---|---|---|
| Aug 2012 | **Knight Capital** ($440 M in 45 minutes) | a deploy reached seven of eight servers; the eighth ran old code with a repurposed flag | [11](11-kubernetes.md): one artefact, GitOps with `selfHeal` — drift between what git says and what runs is reverted, not discovered |
| Oct 2018 | **GitHub 24-hour degradation** | a 43-second network partition caused MySQL failover across two data centres and a split of writes; recovery took a day | [03](03-durability-substrate.md), [04](04-scheduling.md): a single writer per run, fenced at the row; leases judged by one database's clock |
| Jul 2019 | **Cloudflare WAF regex outage** | a rule with catastrophic backtracking pinned CPUs globally within minutes; a global deploy with no staged rollout | bounded retries and budgets everywhere; sandboxes with CPU limits; staged rollouts are the production step for definitions |
| Nov 2020 | **AWS Kinesis (us-east-1)** | fleet growth crossed an OS thread limit on front-end servers; cascading failure across dependent services | limits are real: ResourceQuota, LimitRange, `pids.max`; the design counts what it consumes |
| Jan 2021 | **Slack New-Year outage** | traffic spike after a holiday + autoscaling lag + a transit-gateway limit | HPA scale-up policy 100 %/30 s; admission control returns 429 before the queue fills |
| Apr 2021 | **Codecov bash uploader** | a modified upload script exfiltrated CI environment variables (credentials) for months | credentials in an environment that runs someone else's code will leave; [06](06-credentials.md) |
| Oct 2021 | **Roblox 73-hour outage** | a Consul streaming feature plus a BoltDB performance pathology took down the coordination layer that everything depended on | [03](03-durability-substrate.md): no separate coordination service — the lease lives in the same database as the data it guards |
| Jan 2023 | **CircleCI breach** | malware on an engineer's laptop stole a session token; long-lived customer secrets stored in CircleCI had to be rotated | short-lived, revocable tokens ([06](06-credentials.md)); nothing long-lived in the platform's own stores |
| Oct 2023 | **Okta support-system breach** | HAR files uploaded for support contained session tokens | tokens in logs/files are leaks; the `Secret` type and scrubbing exist for the same reason |
| Jul 2024 | **CrowdStrike channel-file update** | a content update crashed ~8.5 M Windows hosts; the update bypassed staged validation | `make k8s-validate` (kubeconform) before any cluster sees a manifest; Argo health checks; staged rollout of agent definitions is the production step |
| Mar 2024 | **`xz`/liblzma backdoor** (CVE-2024-3094) | a multi-year social-engineering campaign inserted a backdoor into a compression library reachable from `sshd` | [12](12-language-and-dependencies.md): one external module in the TCB |
| Nov 2025 | **Cloudflare core-proxy outage** | a database permission change made a generated feature file exceed a hard-coded size limit, causing the proxy to fail on every request | fail *closed* is right for a credential path and wrong for a data-plane limit — the gateway refuses on journal failure by design ([03-tool-gateway](../architecture/03-tool-gateway.md)) and the docs say so; capped outputs are truncated, not fatal ([04-sandbox](../architecture/04-sandbox.md)) |

## C. Documented limits and pitfalls (not incidents, but written down by the people who own the system)

| Source | What it documents | Where it is used here |
|---|---|---|
| Kleppmann, *How to do distributed locking* | RedLock is unsafe without a fencing token checked by storage | the `Commit` fence ([03](03-durability-substrate.md)) |
| Temporal docs, *Workflow limits* | history size/length caps; `continue-as-new` | replay growth and the designed checkpoint event ([04](04-scheduling.md)) |
| Kubernetes docs, *Network Policies* | "your networking solution must support NetworkPolicy" | Calico on kind; the honest gap in `kind-test.sh` ([11](11-kubernetes.md)) |
| Argo CD docs, *Diffing customization* | HPA-managed replicas appear as drift | `ignoreDifferences` on agentd |
| Docker docs, *Seccomp* | `clone3` returns `ENOSYS` in the default profile | the seccomp filter copies it ([02](02-isolation.md)) |
| gVisor docs, *Compatibility / Performance* | unimplemented syscalls; overhead ranges | short tool calls only; the `namespace` driver for dev |
| AWS Architecture Blog, *Exponential Backoff and Jitter* | full jitter minimises contention | the model gateway's retry ([09](09-llm-integration.md)) |
| Brandur, *Postgres job queues & failure by MVCC* | queue-table bloat under `SKIP LOCKED` churn | the queue is the low-churn `runs` table + partial index ([03](03-durability-substrate.md)) |
| Sidekiq / Celery docs | jobs will run twice; make them idempotent | the tool-call journal is not optional ([04](04-scheduling.md)) |
| MCP specification, *Tool annotations* | `destructiveHint` etc. are hints, not security | the gateway's `Dangerous` flag is enforced server-side ([05](05-authorization.md)) |
| OWASP LLM Top 10 | LLM01 prompt injection, LLM06 excessive agency | the two the design addresses structurally ([10](10-prompt-injection.md)) |

## D. Lessons this repository learned about itself

The nine bugs in [bugs found](../04-evidence/03-bugs-found.md) are the
repository's own incident log. The three that changed how the tests are
written: a test that passed for the wrong reason (B2), a test that never
exercised the mechanism (B3), and a documentation claim that quietly became
false (B9). The rule extracted — *a safety test needs a way to fail that the
attacker's absence cannot satisfy* — is applied in every safety test now.

## References

### Agent incidents
* Greshake et al., *Not what you've signed up for* (indirect prompt injection; Bing Chat) — https://arxiv.org/abs/2302.12173
* Rehberger, *ChatGPT plugin exploit: data exfiltration via markdown injection* — https://embracethered.com/blog/posts/2023/chatgpt-webpilot-data-exfil-via-markdown-injection/
* PromptArmor, *Data exfiltration from Slack AI via indirect prompt injection* — https://promptarmor.substack.com/p/data-exfiltration-from-slack-ai-via
* GitHub advisory, `tj-actions/changed-files` (CVE-2025-30066) — https://github.com/advisories/GHSA-mrrh-fwg8-r2c3
* Invariant Labs, *GitHub MCP exploited* — https://invariantlabs.ai/blog/mcp-github-vulnerability
* Aim Security, *EchoLeak* (CVE-2025-32711) — https://www.aim.security/lp/aim-labs-echoleak-blogpost
* General Analysis, *Supabase MCP can leak your entire SQL database* — https://www.generalanalysis.com/blog/supabase-mcp-blog
* AWS Security Bulletin, *Amazon Q Developer VS Code extension* (July 2025) — https://aws.amazon.com/security/security-bulletins/AWS-2025-015/
* Replit, *Our response to the SaaStr incident* (Amjad Masad, July 2025) — https://blog.replit.com/postmortem-saastr
* 0din (Mozilla), *Phishing for Gemini* — https://0din.ai/blog/phishing-for-gemini
* Aim Labs, *CurXecute* (CVE-2025-54135) — https://www.aim.security/lp/aim-labs-curxecute-blogpost · Check Point Research, *MCPoison* (CVE-2025-54136) — https://research.checkpoint.com/2025/cursor-vulnerability-mcpoison/

### Infrastructure incidents
* SEC, *In the Matter of Knight Capital Americas LLC* (2013) — https://www.sec.gov/litigation/admin/2013/34-70694.pdf
* GitHub, *October 21 post-incident analysis* — https://github.blog/2018-10-30-oct21-post-incident-analysis/
* Cloudflare, *Details of the Cloudflare outage on July 2, 2019* — https://blog.cloudflare.com/details-of-the-cloudflare-outage-on-july-2-2019/
* AWS, *Summary of the Amazon Kinesis event in the Northern Virginia (US-EAST-1) Region* — https://aws.amazon.com/message/11201/
* Slack Engineering, *Slack's outage on January 4th 2021* — https://slack.engineering/slacks-outage-on-january-4th-2021/
* Codecov, *Bash uploader security update* (April 2021) — https://about.codecov.io/security-update/
* Roblox, *Roblox return to service 10/28–10/31 2021* — https://blog.roblox.com/2022/01/roblox-return-to-service-10-28-10-31-2021/
* CircleCI, *Incident report: January 4, 2023 security incident* — https://circleci.com/blog/jan-4-2023-incident-report/
* Okta, *Security incident: unauthorized access to Okta's support case management system* — https://sec.okta.com/harfiles
* CrowdStrike, *Channel File 291 incident root cause analysis* — https://www.crowdstrike.com/en-us/blog/channel-file-291-rca-available/
* CVE-2024-3094 (xz) — https://nvd.nist.gov/vuln/detail/CVE-2024-3094
* Cloudflare, *Cloudflare outage on November 18, 2025* — https://blog.cloudflare.com/18-november-2025-outage/

### Documented limits
* Kleppmann — https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html · Temporal — https://docs.temporal.io/workflows#continue-as-new · Kubernetes — https://kubernetes.io/docs/concepts/services-networking/network-policies/ · Argo CD — https://argo-cd.readthedocs.io/en/stable/user-guide/diffing/ · Docker seccomp — https://docs.docker.com/engine/security/seccomp/ · gVisor — https://gvisor.dev/docs/user_guide/compatibility/ · AWS backoff — https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/ · Brandur — https://brandur.org/postgres-queues · MCP tools — https://modelcontextprotocol.io/specification/2025-06-18/server/tools · OWASP — https://owasp.org/www-project-top-10-for-large-language-model-applications/
