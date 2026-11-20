import test from "node:test";
import assert from "node:assert/strict";
import { handle } from "./worker.mjs";

const key = "a".repeat(40);
const limiter = { limit: async () => ({ success: true }) };
const env = {
  RELAY_KEY: key,
  TELEGRAM_BOT_TOKEN: "999:fake_test_token",
  TELEGRAM_CHAT_ID: "234",
  SOURCE_LIMIT: limiter,
  DELIVERY_LIMIT: limiter,
};
const report = { event: "ready", version: "0.3.2", installation_id: "1032547698abcdef", profile: "split", checks: [
  { name: "API", code: "ok", ms: 42 }, { name: "YouTube", code: "timeout" },
] };
function request(body, auth = key) {
  return new Request("https://relay.example/v1/report", {
    method: "POST",
    headers: { Authorization: "Bearer " + auth, "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

test("sends only approved fields to the configured chat", async () => {
  const response = await handle(request(report), env, async (url, init) => {
    assert.equal(url, "https://api.telegram.org/bot999:fake_test_token/sendMessage");
    const output = JSON.parse(init.body);
    assert.equal(output.chat_id, "234");
    assert.match(output.text, /Установка — 1032547698abcdef/);
    assert.match(output.text, /API — доступен, 42 мс/);
    assert.match(output.text, /YouTube — тайм-аут/);
    return Response.json({ ok: true });
  });
  assert.equal(response.status, 204);
});

test("rejects bad authentication, unexpected fields, excess bytes and rate-limited requests", async () => {
  const send = () => assert.fail("must not contact Telegram");
  assert.equal((await handle(request(report, "wrong"), env, send)).status, 401);
  assert.equal((await handle(request({ ...report, chat_id: "123" }), env, send)).status, 400);
  assert.equal((await handle(request("x".repeat(2048)), env, send)).status, 400);
  const limited = { ...env, DELIVERY_LIMIT: { limit: async () => ({ success: false }) } };
  assert.equal((await handle(request(report), limited, send)).status, 429);
});

test("public build accepts a complete report without a private key", async () => {
  const body = { ...report, checks: ["API", "Gateway", "CDN", "Updates", "YouTube", "YouTube Media"]
    .map((name) => ({ name, code: "ok", ms: 42 })) };
  const response = await handle(new Request("https://relay.example/v1/public-report", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  }), env, async () => Response.json({ ok: true }));
  assert.equal(response.status, 204);
});

test("periodic status updates storage without sending an individual Telegram message", async () => {
  const { handle } = await import("./worker.mjs");
  const body = { ...report, event: "failure", include_in_digest: true, snapshot: true, cause: "no_profile" };
  const destination = {
    ...env,
    INSTALLATION_REPORTS: {
      idFromName(name) { assert.equal(name, "discordlink-reports"); return name; },
      get() {
        return {
          fetch(url, init) {
            assert.equal(url, "https://reports.internal/record");
            assert.deepEqual(JSON.parse(init.body), body);
            return new Response(null, { status: 204 });
          },
        };
      },
    },
  };
  assert.equal((await handle(request(body), destination, () => assert.fail("unexpected Telegram message"))).status, 204);
});

test("digest marks stale installations and sends a CSV to the configured chat", async () => {
  const { sendDigest, digestContent } = await import("./digest.mjs");
  const now = Date.parse("2026-11-20T21:00:00Z");
  const rows = [
    { id: "1032547698abcdef", seen: now, report: JSON.stringify(report) },
    { id: "999999aaaaabbbbb", seen: now - 46 * 60_000, report: JSON.stringify(report) },
  ];
  const result = digestContent(rows, now);
  assert.match(result.caption, /Нет свежих данных \(>45 мин\): 1/);
  assert.match(result.csv, /999999aaaaabbbbb/);
  assert.match(result.csv, /YouTube Media/);
  assert.equal(await sendDigest(env, rows, now, async (url, init) => {
    assert.match(url, /\/sendDocument$/);
    assert.equal(init.body.get("chat_id"), env.TELEGRAM_CHAT_ID);
    assert.match(await init.body.get("document").text(), /1032547698abcdef/);
    return Response.json({ ok: true });
  }), true);
});
