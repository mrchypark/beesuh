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
  test.each([undefined, "0", "1"])("permanently rejects stale conductor_emit definitions when CONDUCTOR_LEGACY_EMIT_UNSAFE=%s", async (value) => {
    configure()
    if (value === undefined) delete process.env.CONDUCTOR_LEGACY_EMIT_UNSAFE
    else process.env.CONDUCTOR_LEGACY_EMIT_UNSAFE = value
    const calls: Request[] = []
    const savedFetch = globalThis.fetch
    globalThis.fetch = async (input, init) => {
      calls.push(new Request(input, init))
      throw new Error("network must not run")
    }
    try {
      // Call the loaded legacy handler directly, as an already-open OpenCode
      // session can retain its stale tool definition after configuration changes.
      const attempts = await Promise.allSettled([
        emit.execute({ name: "stale-conductor_emit", data: { order_id: "o_1" } }, context()),
        emit.execute({ name: "stale-conductor_emit", data: { order_id: "o_1" } }, { ...context(), messageID: "msg_2" }),
      ])
      expect(attempts.map((attempt) => attempt.status)).toEqual(["rejected", "rejected"])
      expect(attempts.map((attempt) => String((attempt as PromiseRejectedResult).reason))).toEqual([
        "Error: Legacy Conductor emission is permanently disabled. Use the Beesuh native Conductor boundary for consequential work.",
        "Error: Legacy Conductor emission is permanently disabled. Use the Beesuh native Conductor boundary for consequential work.",
      ])
    } finally { globalThis.fetch = savedFetch }
    expect(calls).toHaveLength(0)
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
