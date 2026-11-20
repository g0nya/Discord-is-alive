const halfHour = 30 * 60_000;
const day = 24 * 60 * 60_000;
const freshness = 45 * 60_000;
const probeNames = ["API", "Gateway", "CDN", "Updates", "YouTube", "YouTube Media"];
const dateTime = new Intl.DateTimeFormat("ru-RU", {
  timeZone: "Europe/Moscow", dateStyle: "short", timeStyle: "short",
});

export function digestContent(rows, now) {
  const groups = { healthy: 0, partial: 0, failure: 0, stale: 0 };
  const lines = [["ID установки", "Последний отчёт (МСК)", "Статус", "Версия", "Профиль", "Причина", ...probeNames]];
  for (const row of rows) {
    const report = JSON.parse(row.report);
    const checks = new Map(report.checks.map((item) => [item.name, item]));
    let group = "healthy";
    let label = "Discord и YouTube доступны";
    if (now - row.seen > freshness) {
      group = "stale";
      label = "Нет свежих данных";
    } else if (report.event === "failure") {
      group = "failure";
      label = "Сбой";
    } else if (probeNames.slice(4).some((name) => checks.get(name)?.code !== "ok")) {
      group = "partial";
      label = "Discord доступен, YouTube не подтверждён";
    }
    groups[group]++;
    lines.push([
      row.id, dateTime.format(row.seen), label, report.version, report.profile ?? "", report.cause ?? "",
      ...probeNames.map((name) => {
        const check = checks.get(name);
        return check ? (check.ms === undefined ? check.code : check.code + " " + check.ms + " ms") : "";
      }),
    ]);
  }
  const csv = "\uFEFF" + lines.map((line) => line.map((part) => '"' + part.replaceAll('"', '""') + '"').join(";")).join("\r\n");
  const caption = [
    "DiscordLink · сводка " + dateTime.format(now) + " МСК",
    "Установок за 24 часа: " + rows.length,
    "Discord и YouTube: " + groups.healthy,
    "Discord, YouTube не подтверждён: " + groups.partial,
    "Сбой: " + groups.failure,
    "Нет свежих данных (>45 мин): " + groups.stale,
  ].join("\n");
  return { csv, caption };
}

export async function sendDigest(env, rows, now, send = (url, init) => fetch(url, init)) {
  const { csv, caption } = digestContent(rows, now);
  const form = new FormData();
  form.set("chat_id", env.TELEGRAM_CHAT_ID);
  form.set("caption", caption);
  form.set("document", new Blob([csv], { type: "text/csv; charset=utf-8" }), "discordlink-" + new Date(now).toISOString().slice(0, 16).replace(":", "-") + "Z.csv");
  const timeout = new AbortController();
  const timer = setTimeout(() => timeout.abort(), 10_000);
  try {
    const response = await send("https://api.telegram.org/bot" + env.TELEGRAM_BOT_TOKEN + "/sendDocument", {
      method: "POST", body: form, redirect: "manual", signal: timeout.signal,
    });
    return response.ok && (await response.json()).ok === true;
  } catch {
    return false;
  } finally {
    clearTimeout(timer);
  }
}

export class InstallationReports {
  constructor(ctx, env) {
    this.sql = ctx.storage.sql;
    this.env = env;
    this.sql.exec("CREATE TABLE IF NOT EXISTS reports (id TEXT PRIMARY KEY, seen INTEGER NOT NULL, report TEXT NOT NULL)");
    this.sql.exec("CREATE TABLE IF NOT EXISTS digests (slot INTEGER PRIMARY KEY)");
  }

  async fetch(request) {
    const path = new URL(request.url).pathname;
    if (path === "/record") {
      const report = await request.json();
      this.sql.exec(
        "INSERT INTO reports (id, seen, report) VALUES (?, ?, ?) ON CONFLICT(id) DO UPDATE SET seen=excluded.seen, report=excluded.report",
        report.installation_id, Date.now(), JSON.stringify(report),
      );
      return new Response(null, { status: 204 });
    }
    if (path === "/digest") {
      const { scheduledTime } = await request.json();
      const now = Date.now(), slot = Math.floor(scheduledTime / halfHour);
      if (this.sending || this.sql.exec("SELECT 1 FROM digests WHERE slot = ?", slot).toArray().length) {
        return new Response(null, { status: 204 });
      }
      this.sending = true;
      try {
        this.sql.exec("DELETE FROM reports WHERE seen < ?", now - day);
        this.sql.exec("DELETE FROM digests WHERE slot < ?", slot - 48);
        const rows = this.sql.exec("SELECT id, seen, report FROM reports ORDER BY seen DESC, id").toArray();
        if (!await sendDigest(this.env, rows, now)) return new Response(null, { status: 502 });
        this.sql.exec("INSERT INTO digests (slot) VALUES (?)", slot);
        return new Response(null, { status: 204 });
      } finally {
        this.sending = false;
      }
    }
    return new Response(null, { status: 404 });
  }
}

export function reportStore(env) {
  return env.INSTALLATION_REPORTS.get(env.INSTALLATION_REPORTS.idFromName("discordlink-reports"));
}
