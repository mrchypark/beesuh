import { afterEach, describe, expect, mock, test } from "bun:test"

const schema = { string: () => ({ min() { return this } }), unknown: () => ({}) }
mock.module("@opencode-ai/plugin", () => ({ tool: Object.assign((input: unknown) => input, { schema }) }))
const { emit, get_run } = await import("./tools/conductor")
const keys = ["CONDUCTOR_URL", "CONDUCTOR_TOKEN", "CONDUCTOR_ALLOWED_EVENTS", "CONDUCTOR_EVENT_BINDINGS", "CONDUCTOR_WORKSPACE_ID", "CONDUCTOR_LEGACY_EMIT_UNSAFE"] as const
const original = Object.fromEntries(keys.map((key) => [key, process.env[key]]))

afterEach(() => { for (const key of keys) process.env[key] = original[key] })
function context() { return { sessionID: "ses_1", messageID: "msg_1", agent: "build", directory: ".", worktree: ".", abort: new AbortController().signal, metadata() {}, async ask() {} } }
function configure() {
  process.env.CONDUCTOR_URL = "https://conductor.example/base"
  process.env.CONDUCTOR_TOKEN = "secret"
  process.env.CONDUCTOR_ALLOWED_EVENTS = "order.created"
  process.env.CONDUCTOR_EVENT_BINDINGS = JSON.stringify({ "order.created": { handler_key: "payload.echo", handler_version: "1", operation_key: "payload.echo" } })
  process.env.CONDUCTOR_WORKSPACE_ID = "workspace"
  process.env.CONDUCTOR_LEGACY_EMIT_UNSAFE = "1"
}

describe("Conductor OpenCode tools", () => {
  test("disables legacy emission by default so a retry cannot create a second effect", async () => {
    configure()
    delete process.env.CONDUCTOR_LEGACY_EMIT_UNSAFE
    const calls: Request[] = []
    const savedFetch = globalThis.fetch
    globalThis.fetch = async (input, init) => {
      calls.push(new Request(input, init))
      throw new Error("lost acknowledgement")
    }
    try {
      await expect(emit.execute({ name: "order.created", data: { order_id: "o_1" } }, context())).rejects.toThrow("Legacy Conductor emission is disabled")
      await expect(emit.execute({ name: "order.created", data: { order_id: "o_1" } }, { ...context(), messageID: "msg_2" })).rejects.toThrow("Legacy Conductor emission is disabled")
    } finally { globalThis.fetch = savedFetch }
    expect(calls).toHaveLength(0)
  })

  test("queues one bound execution with trusted linkage", async () => {
    configure()
    const calls: Request[] = []
    const savedFetch = globalThis.fetch
    globalThis.fetch = async (input, init) => {
      const request = new Request(input, init); calls.push(request.clone())
      const key = (await request.json() as { events: Array<{ idempotency_key: string }> }).events[0].idempotency_key
      return new Response(JSON.stringify({ accepted: true, events: [{ execution_id: "exec_1", execution_request_id: key, status: "queued" }] }), { status: 202 })
    }
    try { await emit.execute({ name: "order.created", data: { order_id: "o_1" } }, context()) } finally { globalThis.fetch = savedFetch }
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe("https://conductor.example/base/v1/events")
    expect(calls[0].headers.get("authorization")).toBe("Bearer secret")
    const event = (await calls[0].json() as { events: Array<{ name: string; data: { _compos: Record<string, unknown> }; idempotency_key: string }> }).events[0]
    expect(event.name).toBe("order.created")
    expect(event.idempotency_key).toMatch(/^beesuh_[a-f0-9]{64}$/)
    expect(event.data._compos).toEqual({ workspace_id: "workspace", execution_request_id: event.idempotency_key, handler_key: "payload.echo", handler_version: "1", operation_key: "payload.echo", actor: { type: "service", id: "beesuh" } })
  })

  test("rejects model routing metadata before the network", async () => {
    configure()
    const savedFetch = globalThis.fetch
    globalThis.fetch = async () => { throw new Error("network must not run") }
    try {
      await expect(emit.execute({ name: "order.created", data: { _compos: {} } }, context())).rejects.toThrow("reserved metadata")
      await expect(emit.execute({ name: "order.created", data: [] }, context())).rejects.toThrow("JSON object")
    } finally { globalThis.fetch = savedFetch }
  })

  test("rejects a malformed acceptance response", async () => {
    configure()
    const savedFetch = globalThis.fetch
    globalThis.fetch = async () => new Response("{}", { status: 202 })
    try { await expect(emit.execute({ name: "order.created", data: {} }, context())).rejects.toThrow("response is invalid") } finally { globalThis.fetch = savedFetch }
  })

  test("reads a returned execution ID with a trailing-slash base path", async () => {
    configure()
    process.env.CONDUCTOR_URL += "/"
    const savedFetch = globalThis.fetch
    globalThis.fetch = async (input, init) => {
      expect(new Request(input, init).url).toBe("https://conductor.example/base/v1/executions/exec_1")
      expect(init?.method).toBe("GET")
      return new Response('{"id":"exec_1","status":"completed"}')
    }
    try { expect(await get_run.execute({ run_id: "exec_1" }, context())).toBe('{"id":"exec_1","status":"completed"}') } finally { globalThis.fetch = savedFetch }
  })
})
