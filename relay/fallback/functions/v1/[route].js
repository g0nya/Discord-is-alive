export function onRequest(context) {
  const path = new URL(context.request.url).pathname;
  if (path !== "/v1/public-report" && path !== "/v1/report") {
    return new Response(null, { status: 404 });
  }
  return context.env.REPORTS.fetch(context.request);
}
