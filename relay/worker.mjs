import { reportStore } from "./digest.mjs";
export { InstallationReports } from "./digest.mjs";

const checks = new Set(["API", "Gateway", "CDN", "Updates", "YouTube", "YouTube Media"]);
const profiles = new Set(["split", "update", "overlap", "disorder"]);
const codes = new Map([
  ["ok", "доступен"], ["dns", "ошибка DNS"], ["timeout", "тайм-аут"],
  ["tls", "ошибка TLS"], ["http", "ошибка HTTP"], ["connection", "ошибка соединения"],
]);

const causes = new Map([
  ["startup", "ошибка запуска"], ["engine_download", "не удалось подготовить движок"],
  ["engine_exit", "движок остановился"], ["no_profile", "профили не прошли проверку"],
]);

function matchesKeys(obj, required, optional = []) {
  return obj !== null && typeof obj === "object" && !Array.isArray(obj)
    && required.every((key) => Object.hasOwn(obj, key))
    && Object.keys(obj).every((key) => required.includes(key) || optional.includes(key));
}

function formatReport(report) {
  if (!matchesKeys(report, ["event", "version", "checks"], ["profile", "cause", "installation_id", "include_in_digest", "snapshot"])
      || !["ready", "failure"].includes(report.event)
      || typeof report.version !== "string" || !/^\d{1,3}\.\d{1,3}\.\d{1,3}$/.test(report.version)
      || (Object.hasOwn(report, "profile") && !profiles.has(report.profile))
      || (Object.hasOwn(report, "cause") && (report.event !== "failure" || !causes.has(report.cause)))
      || (Object.hasOwn(report, "installation_id") && (typeof report.installation_id !== "string" || !/^[a-f0-9]{16}$/.test(report.installation_id)))
      || (Object.hasOwn(report, "include_in_digest") && (report.include_in_digest !== true || !report.installation_id))
      || (Object.hasOwn(report, "snapshot") && (report.snapshot !== true || report.include_in_digest !== true))
      || !Array.isArray(report.checks) || report.checks.length > checks.size) return null;

  const lines = [
    report.event === "ready" ? "DiscordLink · работает" : "DiscordLink · сбой",
    "Версия " + report.version + (report.profile ? " · профиль " + report.profile : ""),
  ];
  if (report.installation_id) lines.push("Установка — " + report.installation_id);
  if (report.cause) lines.push("Причина — " + causes.get(report.cause));
  const seen = new Set();
  for (const check of report.checks) {
    if (!matchesKeys(check, ["name", "code"], ["ms"])
        || !checks.has(check.name) || seen.has(check.name) || !codes.has(check.code)
        || (Object.hasOwn(check, "ms")
            && (check.code !== "ok" || !Number.isInteger(check.ms) || check.ms < 0 || check.ms > 10000))) return null;
    seen.add(check.name);
    lines.push(check.name + " — " + codes.get(check.code) + (check.ms !== undefined ? ", " + check.ms + " мс" : ""));
  }
  return lines.join("\n");
}

async function readReport(request) {
  const reader = request.body?.getReader();
  if (!reader) return null;
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let body = "", size = 0;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      size += value.length;
      if (size > 2048) {
        await reader.cancel();
        return null;
      }
      body += decoder.decode(value, { stream: true });
    }
    return JSON.parse(body + decoder.decode());
  } catch {
    return null;
  } finally {
    reader.releaseLock();
  }
}

function authorized(header, key) {
  if (typeof key !== "string" || !/^[A-Za-z0-9_-]{32,128}$/.test(key)) return false;
  const expected = "Bearer " + key;
  if (header?.length !== expected.length) return false;
  let diff = 0;
  for (let i = 0; i < expected.length; i++) diff |= header.charCodeAt(i) ^ expected.charCodeAt(i);
  return diff === 0;
}

export async function handle(request, env, send = (url, init) => fetch(url, init)) {
  const path = new URL(request.url).pathname;
  const publicReport = path === "/v1/public-report";
  if (!publicReport && path !== "/v1/report") return new Response(null, { status: 404 });
  if (request.method !== "POST") return new Response(null, { status: 405, headers: { Allow: "POST" } });
  if (!publicReport && !authorized(request.headers.get("Authorization"), env.RELAY_KEY)) return new Response(null, { status: 401 });
  if (request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== "application/json") {
    return new Response(null, { status: 415 });
  }

  const source = request.headers.get("CF-Connecting-IP") ?? "unknown";
  if (!(await env.SOURCE_LIMIT.limit({ key: source })).success) return new Response(null, { status: 429 });

  const report = await readReport(request);
  const text = formatReport(report);
  if (!text) return new Response(null, { status: 400 });
  if (publicReport && report.event === "ready" && (!report.profile || report.checks.length !== checks.size)) {
    return new Response(null, { status: 400 });
  }
  if (report.include_in_digest) {
    const result = await reportStore(env).fetch("https://reports.internal/record", {
      method: "POST", body: JSON.stringify(report),
    });
    if (!result.ok || report.snapshot) return result;
  }
  if (!/^\d+:[A-Za-z0-9_-]+$/.test(env.TELEGRAM_BOT_TOKEN ?? "")
      || !/^-?\d+$/.test(env.TELEGRAM_CHAT_ID ?? "")) return new Response(null, { status: 503 });
  if (!(await env.DELIVERY_LIMIT.limit({ key: "telegram" })).success) return new Response(null, { status: 429 });

  const timeout = new AbortController();
  const timer = setTimeout(() => timeout.abort(), 5000);
  try {
    const response = await send("https://api.telegram.org/bot" + env.TELEGRAM_BOT_TOKEN + "/sendMessage", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ chat_id: env.TELEGRAM_CHAT_ID, text, link_preview_options: { is_disabled: true } }),
      redirect: "manual",
      signal: timeout.signal,
    });
    if (response.ok && (await response.json()).ok === true) return new Response(null, { status: 204 });
    return new Response(null, { status: 502, headers: { "X-DiscordLink-Telegram-Status": String(response.status) } });
  } catch {
    // Token and upstream response body are excluded from logs and replies.
    return new Response(null, { status: 502, headers: { "X-DiscordLink-Telegram-Status": timeout.signal.aborted ? "timeout" : "transport" } });
  } finally {
    clearTimeout(timer);
  }
}

export default {
  fetch: (request, env) => handle(request, env),
  async scheduled(controller, env) {
    const result = await reportStore(env).fetch("https://reports.internal/digest", {
      method: "POST", body: JSON.stringify({ scheduledTime: controller.scheduledTime }),
    });
    if (!result.ok) throw new Error("Telegram digest delivery failed (" + result.status + ")");
  },
};
