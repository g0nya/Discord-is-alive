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
const report = { event: "ready", version: "0.3.0", profile: "split", checks: [
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
