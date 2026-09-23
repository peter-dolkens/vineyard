// Synthetic demo data for the listing screenshots. Nothing here is a real machine, session or person.
(function () {
  const now = Date.now();
  const ago = (s) => now - s * 1000;
  const iso = (s) => new Date(ago(s)).toISOString();
  let n = 0;
  const id = () => `demo-${++n}`;

  const usage = (input, read, create, out) => ({
    input_tokens: input,
    cache_read_input_tokens: read,
    cache_creation_input_tokens: create,
    output_tokens: out,
    cache_creation: { ephemeral_5m_input_tokens: 0, ephemeral_1h_input_tokens: create },
  });
  const user = (s, text) => ({ type: 'user', uuid: id(), timestamp: iso(s), message: { role: 'user', content: [{ type: 'text', text }] } });
  const asst = (s, model, content, u) => ({ type: 'assistant', uuid: id(), timestamp: iso(s), message: { role: 'assistant', model, content, usage: u } });
  const result = (s, toolId, content, isError) => ({
    type: 'user',
    uuid: id(),
    timestamp: iso(s),
    message: { role: 'user', content: [{ type: 'tool_result', tool_use_id: toolId, content, is_error: !!isError }] },
  });

  const MODELS = [
    { value: 'default', resolvedModel: 'claude-opus-5', displayName: 'Default (recommended)', description: 'Opus 5 · Best for everyday work', supportedEffortLevels: ['low', 'medium', 'high', 'max'] },
    { value: 'opus', resolvedModel: 'claude-opus-5', displayName: 'Opus 5', description: 'Most capable', supportedEffortLevels: ['low', 'medium', 'high', 'max'] },
    { value: 'sonnet', resolvedModel: 'claude-sonnet-5', displayName: 'Sonnet 5', description: 'Fast and capable', supportedEffortLevels: ['low', 'medium', 'high'] },
    { value: 'haiku', resolvedModel: 'claude-haiku-4-5-20251001', displayName: 'Haiku 4.5', description: 'Fastest', supportedEffortLevels: [] },
  ];
  const COMMANDS = [
    { name: 'compact', description: 'Clear conversation history but keep a summary in context', argumentHint: '[instructions]' },
    { name: 'clear', description: 'Clear conversation history and free up context' },
    { name: 'review', description: 'Review a pull request' },
    { name: 'init', description: 'Initialize a new CLAUDE.md file with codebase documentation' },
    { name: 'cost', description: 'Show the total cost and duration of the current session' },
  ];
  const accountUsage = (five, week, weekModel) => ({
    status: 'allowed',
    windows: {
      five_hour: { utilization: five, resetsAt: now + 2.4 * 3600e3 },
      seven_day: { utilization: week, resetsAt: now + 3.2 * 86400e3 },
      seven_day_opus: { utilization: weekModel, resetsAt: now + 3.2 * 86400e3 },
    },
    at: ago(40),
  });

  // ---- 1. Managed session, remote build server, waiting on a permission prompt -------------------
  const perm = (() => {
    const t = {};
    const entries = [
      user(620, 'Add per-customer rate limiting to the checkout endpoint. Use the existing Redis client, 20 requests per minute, and return 429 with a Retry-After header. Add tests.'),
      asst(605, 'claude-opus-5', [
        { type: 'thinking', thinking: 'The checkout route lives under src/routes. Before writing anything I should see how the Redis client is constructed and whether there is an existing middleware pattern to follow, so the limiter looks like the rest of the codebase.' },
        { type: 'text', text: "I'll look at how the checkout route and the Redis client are wired before adding anything." },
        { type: 'tool_use', id: (t.read = id()), name: 'Read', input: { file_path: 'src/routes/checkout.ts' } },
      ], usage(1180, 38400, 2900, 210)),
      result(600, t.read, "     1\timport { Router } from 'express';\n     2\timport { redis } from '../lib/redis';\n     3\timport { createOrder } from '../services/orders';\n     4\t\n     5\texport const checkout = Router();\n     6\t\n     7\tcheckout.post('/checkout', async (req, res) => {\n     8\t  const order = await createOrder(req.customer.id, req.body);\n     9\t  res.status(201).json(order);\n    10\t});"),
      asst(590, 'claude-opus-5', [
        { type: 'tool_use', id: (t.grep = id()), name: 'Grep', input: { pattern: 'export function .*Middleware', path: 'src/middleware' } },
      ], usage(640, 41300, 410, 90)),
      result(586, t.grep, 'src/middleware/auth.ts:12:export function authMiddleware(): RequestHandler {\nsrc/middleware/requestId.ts:5:export function requestIdMiddleware(): RequestHandler {'),
      asst(560, 'claude-opus-5', [
        { type: 'text', text: 'The middleware folder exports factory functions returning an Express `RequestHandler`, so the limiter will follow the same shape. A sliding window in Redis (`ZADD` + `ZREMRANGEBYSCORE` in a `MULTI`) gives an accurate 20-per-minute count per customer without a Lua script.' },
        { type: 'tool_use', id: (t.write = id()), name: 'Write', input: { file_path: 'src/middleware/rateLimit.ts', content: "import type { RequestHandler } from 'express';\nimport { redis } from '../lib/redis';\n\nexport function rateLimitMiddleware(limit = 20, windowMs = 60_000): RequestHandler {\n  return async (req, res, next) => {\n    const key = `ratelimit:checkout:${req.customer.id}`;\n    const now = Date.now();\n    const [, , count] = await redis.multi().zremrangebyscore(key, 0, now - windowMs).zadd(key, now, `${now}`).zcard(key).pexpire(key, windowMs).exec();\n    if (Number(count[1]) > limit) {\n      res.set('Retry-After', String(Math.ceil(windowMs / 1000)));\n      return res.status(429).json({ error: 'rate_limited' });\n    }\n    next();\n  };\n}\n" } },
      ], usage(720, 41800, 1900, 640)),
      result(552, t.write, 'File created successfully at: src/middleware/rateLimit.ts'),
      asst(540, 'claude-opus-5', [
        { type: 'tool_use', id: (t.edit = id()), name: 'Edit', input: { file_path: 'src/routes/checkout.ts', old_string: "checkout.post('/checkout', async (req, res) => {", new_string: "checkout.post('/checkout', rateLimitMiddleware(), async (req, res) => {" } },
      ], usage(310, 43900, 380, 120)),
      result(536, t.edit, 'The file src/routes/checkout.ts has been updated successfully.'),
      asst(120, 'claude-opus-5', [
        { type: 'text', text: 'Middleware and route wiring are in place, with a test for the 21st request. Running the checkout suite to make sure nothing regressed before I summarise the change.' },
        { type: 'tool_use', id: (t.bash = id()), name: 'Bash', input: { command: 'npm test -- --run checkout', description: 'Run the checkout test suite' } },
      ], usage(240, 45400, 1200, 160)),
    ];
    const agent = {
      id: 'forge/aaaa1111',
      sessionId: '2f5c9a1e-7d3b-4c8e-9f0a-1b2c3d4e5f60',
      alive: true,
      title: 'Add rate limiting to checkout',
      state: 'permission',
      stateDetail: 'Bash: Run the checkout test suite',
      model: 'claude-opus-5',
      effort: 'high',
      permissionMode: 'default',
      contextTokens: 46840,
      contextWindow: 200000,
      workspacePath: '/home/dev/projects/storefront-api',
      lastActivityAt: ago(120),
      startedAt: ago(700),
      gitBranch: 'feat/checkout-rate-limit',
      version: '2.1.280',
      pendingTools: [{ id: t.bash, name: 'Bash', summary: 'Run the checkout test suite' }],
      managed: {
        exited: false,
        turns: 1,
        costUsd: 0.62,
        permissionMode: 'default',
        model: 'claude-opus-5',
        effort: 'high',
        models: MODELS,
        commands: COMMANDS,
        account: 'dev@example.com',
        accountPlan: 'Max',
        usage: accountUsage(0.41, 0.23, 0.17),
        pending: {
          requestId: 'req-1',
          toolName: 'Bash',
          input: { command: 'npm test -- --run checkout', description: 'Run the checkout test suite' },
          description: 'Run the checkout test suite',
          suggestions: [{ type: 'addRules', rules: [{ toolName: 'Bash', ruleContent: 'npm test:*' }], behavior: 'allow', destination: 'localSettings' }],
        },
      },
    };
    return { agent, machine: { id: 'forge', name: 'forge', online: true, usage: accountUsage(0.41, 0.23, 0.17) }, entries };
  })();

  // ---- 2. Managed session fanning out to subagents, a background task, usage at 85 % --------------
  const map = (() => {
    const t = {};
    const entries = [
      user(400, 'Migrate the auth test suites in packages/auth from Jest to Vitest. Keep behaviour identical; fan out one agent per directory and watch the run in the background.'),
      asst(380, 'claude-opus-5', [
        { type: 'text', text: 'Three directories under `packages/auth/__tests__`, each independent. Fanning out one agent per directory, plus a background watcher on the Vitest run so I see failures as they land.' },
        { type: 'tool_use', id: (t.a1 = id()), name: 'Agent', input: { description: 'Convert login tests', subagent_type: 'general-purpose', prompt: 'Convert packages/auth/__tests__/login/*.test.ts from Jest to Vitest…' } },
        { type: 'tool_use', id: (t.a2 = id()), name: 'Agent', input: { description: 'Convert token refresh tests', subagent_type: 'general-purpose', prompt: 'Convert packages/auth/__tests__/refresh/*.test.ts…' } },
        { type: 'tool_use', id: (t.a3 = id()), name: 'Agent', input: { description: 'Audit Jest-only matchers', subagent_type: 'Explore', prompt: 'List every matcher or global used in packages/auth tests that Vitest does not provide…' } },
        { type: 'tool_use', id: (t.b = id()), name: 'Bash', input: { command: 'npx vitest --watch packages/auth', description: 'Watch the Vitest run', run_in_background: true } },
      ], usage(1420, 30100, 4100, 520)),
      result(376, t.b, 'Command running in background with ID: b7c1. Output is being written to: /tmp/claude/tasks/b7c1.output'),
      result(190, t.a3, 'Found 4 Jest-only usages: jest.mock in 3 files (→ vi.mock), expect.addSnapshotSerializer once (→ expect.addSnapshotSerializer is supported), jest.useFakeTimers twice (→ vi.useFakeTimers), and one toMatchInlineSnapshot that Vitest formats differently.'),
      asst(180, 'claude-opus-5', [
        { type: 'text', text: 'The audit is back: four Jest-only usages, all with direct Vitest equivalents. Passing that list to the two conversion agents so they fix them as they go.' },
        { type: 'tool_use', id: (t.m = id()), name: 'SendMessage', input: { to: 'Convert login tests', message: 'Replace jest.mock with vi.mock and jest.useFakeTimers with vi.useFakeTimers while you convert.' } },
      ], usage(640, 35800, 700, 140)),
      result(178, t.m, 'Message sent.'),
    ];
    const agent = {
      id: 'orchard/bbbb2222',
      sessionId: '8a1d2e3f-4b5c-4d6e-8f70-9a0b1c2d3e4f',
      alive: true,
      title: 'Migrate auth tests to Vitest',
      state: 'tool',
      stateDetail: 'Agent: Convert login tests',
      model: 'claude-opus-5',
      effort: 'medium',
      permissionMode: 'acceptEdits',
      contextTokens: 37200,
      contextWindow: 200000,
      workspacePath: '/Users/dev/code/mobile-app',
      lastActivityAt: ago(5),
      startedAt: ago(420),
      gitBranch: 'chore/vitest-auth',
      version: '2.1.280',
      pendingTools: [{ id: t.a1, name: 'Agent', summary: 'Convert login tests' }, { id: t.a2, name: 'Agent', summary: 'Convert token refresh tests' }],
      subagents: [
        { agentId: 'a1', type: 'general-purpose', description: 'Convert login tests', model: 'claude-sonnet-5', state: 'tool', stateDetail: 'Edit: login.test.ts', startedAt: ago(378), lastActivityAt: ago(9), contextTokens: 22100, toolUseId: t.a1, depth: 1 },
        { agentId: 'a2', type: 'general-purpose', description: 'Convert token refresh tests', model: 'claude-sonnet-5', state: 'working', startedAt: ago(378), lastActivityAt: ago(2), contextTokens: 18400, toolUseId: t.a2, depth: 1 },
        { agentId: 'a2x', parentAgentId: 'a2', type: 'Explore', description: 'Find refresh-token fixtures', model: 'claude-haiku-4-5-20251001', state: 'done', startedAt: ago(300), lastActivityAt: ago(240), contextTokens: 6100, depth: 2 },
        { agentId: 'a3', type: 'Explore', description: 'Audit Jest-only matchers', model: 'claude-haiku-4-5-20251001', state: 'done', startedAt: ago(378), lastActivityAt: ago(190), contextTokens: 9800, toolUseId: t.a3, depth: 1 },
      ],
      tasks: [{ toolUseId: t.b, taskId: 'b7c1', description: 'Watch the Vitest run', kind: 'shell', state: 'running', startedAt: ago(376) }],
      managed: {
        exited: false,
        turns: 1,
        costUsd: 1.84,
        permissionMode: 'acceptEdits',
        model: 'claude-opus-5',
        effort: 'medium',
        models: MODELS,
        commands: COMMANDS,
        account: 'dev@example.com',
        accountPlan: 'Max',
        usage: accountUsage(0.85, 0.44, 0.39),
      },
    };
    return { agent, machine: { id: 'orchard', name: 'orchard', online: true, usage: accountUsage(0.85, 0.44, 0.39) }, entries };
  })();

  // ---- 3. Managed session asking a question -------------------------------------------------------
  const question = (() => {
    const t = {};
    const entries = [
      user(300, 'Upgrade every Terraform provider in infra/ to its latest major version, fix whatever breaks in plan, and open a PR.'),
      asst(290, 'claude-opus-5', [
        { type: 'tool_use', id: (t.b1 = id()), name: 'Bash', input: { command: 'cd infra && terraform providers', description: 'List the providers in use' } },
      ], usage(900, 21000, 1800, 110)),
      result(286, t.b1, 'Providers required by configuration:\n.\n├── provider[registry.terraform.io/hashicorp/aws] ~> 5.40\n├── provider[registry.terraform.io/cloudflare/cloudflare] ~> 4.30\n└── provider[registry.terraform.io/hashicorp/random] ~> 3.5'),
      asst(240, 'claude-opus-5', [
        { type: 'text', text: 'Cloudflare 5.x and random 3.7 upgrade cleanly. The AWS 5 to 6 jump removes the deprecated inline `acl` arguments on `aws_s3_bucket`, and eleven buckets in `infra/storage.tf` still use them. That is a design choice rather than a mechanical fix, so I want your call before I touch it.' },
        { type: 'tool_use', id: (t.q = id()), name: 'AskUserQuestion', input: {
          questions: [{
            header: 'S3 ACLs',
            question: 'AWS provider 6.x removes the inline acl arguments on aws_s3_bucket. How should I handle the eleven buckets that still use them?',
            multiSelect: false,
            options: [
              { label: 'Migrate to aws_s3_bucket_acl resources (Recommended)', description: 'One new resource per bucket, no behaviour change, plan shows adds only' },
              { label: 'Pin the AWS provider at 5.x for now', description: 'Upgrade Cloudflare and random today, leave S3 for a follow-up PR' },
              { label: 'Drop the ACLs', description: 'All eleven buckets already have bucket-owner-enforced ownership, so the ACLs are inert' },
            ],
          }],
        } },
      ], usage(1240, 22600, 2100, 380)),
    ];
    const agent = {
      id: 'forge/cccc3333',
      sessionId: 'c3d4e5f6-a7b8-4c9d-8e0f-1a2b3c4d5e6f',
      alive: true,
      title: 'Upgrade Terraform providers',
      state: 'question',
      stateDetail: 'AskUserQuestion',
      model: 'claude-opus-5',
      effort: 'high',
      permissionMode: 'plan',
      contextTokens: 26300,
      contextWindow: 200000,
      workspacePath: '/home/dev/projects/infra',
      lastActivityAt: ago(240),
      startedAt: ago(320),
      gitBranch: 'chore/provider-upgrades',
      version: '2.1.280',
      pendingTools: [{ id: t.q, name: 'AskUserQuestion', summary: 'How should I handle the eleven buckets…' }],
      managed: {
        exited: false,
        turns: 1,
        costUsd: 0.31,
        permissionMode: 'plan',
        model: 'claude-opus-5',
        effort: 'high',
        models: MODELS,
        commands: COMMANDS,
        account: 'dev@example.com',
        accountPlan: 'Max',
        usage: accountUsage(0.41, 0.23, 0.17),
        pending: {
          requestId: 'req-2',
          toolName: 'AskUserQuestion',
          input: entries[entries.length - 1].message.content[1].input,
        },
      },
    };
    return { agent, machine: { id: 'forge', name: 'forge', online: true, usage: accountUsage(0.41, 0.23, 0.17) }, entries };
  })();

  window.__scenarios = { perm, map, question };
})();
