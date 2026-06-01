package main

// indexHTML is the entire read-only dashboard: a single page that polls the
// flowd read API for runs/runners/egress and live-tails a selected run's log via
// SSE. No build step, no framework — vanilla JS keeps the dashboard light
// (decision 7: metadata + logs only).
//
// All dynamic values are inserted with textContent / DOM nodes (never
// innerHTML), so an attacker-influenced field (a crafted branch name, egress
// host, or PR URL) cannot inject script into the admin's browser.
const indexHTML = `<!DOCTYPE html>
<html lang="fi">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Flow — dashboard</title>
<style>
  :root { font-family: system-ui, sans-serif; }
  body { margin: 0; background: #0f1115; color: #e6e6e6; }
  header { padding: 12px 20px; background: #1a1d24; border-bottom: 1px solid #2a2f3a; }
  h1 { font-size: 16px; margin: 0; font-weight: 600; }
  main { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; padding: 16px; }
  section { background: #161922; border: 1px solid #2a2f3a; border-radius: 8px; padding: 12px; }
  h2 { font-size: 13px; text-transform: uppercase; letter-spacing: .05em; color: #8a93a6; margin: 0 0 8px; }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th, td { text-align: left; padding: 4px 6px; border-bottom: 1px solid #232733; }
  th { color: #8a93a6; font-weight: 500; }
  tr.run { cursor: pointer; }
  tr.run:hover { background: #1d2130; }
  .status { font-weight: 600; }
  .completed, .merged { color: #4ade80; }
  .blocked, .timed_out, .pr_conflicted, .lost_race { color: #f87171; }
  .awaiting_clarification { color: #fbbf24; }
  .initialized { color: #60a5fa; }
  .denied { color: #f87171; }
  .allowed { color: #4ade80; }
  #log { grid-column: 1 / 3; }
  pre#logbody { background: #0b0d12; padding: 10px; border-radius: 6px; max-height: 320px;
    overflow: auto; font-size: 12px; line-height: 1.5; margin: 0; }
  .muted { color: #5b6577; }
  a { color: #60a5fa; }
</style>
</head>
<body>
<header><h1>Flow — read-only dashboard</h1></header>
<main>
  <section>
    <h2>Runs</h2>
    <table><thead><tr><th>Issue</th><th>Remote</th><th>Status</th><th>State</th><th>PR</th></tr></thead>
    <tbody id="runs"></tbody></table>
  </section>
  <section>
    <h2>Runners</h2>
    <table><thead><tr><th>Host</th><th>Status</th><th>Cap</th><th>Active</th><th>Heartbeat</th></tr></thead>
    <tbody id="runners"></tbody></table>
  </section>
  <section>
    <h2>Egress log</h2>
    <table><thead><tr><th>Host</th><th>Result</th><th>Time</th></tr></thead>
    <tbody id="egress"></tbody></table>
  </section>
  <section id="log">
    <h2>Live log <span id="logrun" class="muted">— valitse run</span></h2>
    <pre id="logbody"></pre>
  </section>
</main>
<script>
const $ = (id) => document.getElementById(id);
let es = null;

// cell builds a <td> with safe text content; cls is an optional class.
function cell(text, cls) {
  const td = document.createElement('td');
  td.textContent = text == null ? '-' : String(text);
  if (cls) td.className = cls;
  return td;
}

// linkCell builds a <td> containing an anchor with a safe href + text. href is
// only used when it is an http(s) URL, so a crafted value cannot become a
// javascript: URI.
function linkCell(href, label) {
  const td = document.createElement('td');
  if (href && /^https?:\/\//.test(href)) {
    const a = document.createElement('a');
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    a.textContent = label;
    td.appendChild(a);
  } else {
    td.textContent = '-';
  }
  return td;
}

function replaceRows(tbody, rows, buildRow) {
  tbody.replaceChildren();
  for (const r of rows) tbody.appendChild(buildRow(r));
}

async function refresh() {
  try {
    const [runs, runners, egress] = await Promise.all([
      fetch('/v1/runs').then(r => r.json()),
      fetch('/v1/runners').then(r => r.json()),
      fetch('/v1/egress').then(r => r.json()),
    ]);
    renderRuns(runs.runs || []);
    renderRunners(runners.runners || []);
    renderEgress(egress.egress || []);
  } catch (e) { /* central may be momentarily unavailable */ }
}

function renderRuns(rows) {
  replaceRows($('runs'), rows, (r) => {
    const tr = document.createElement('tr');
    tr.className = 'run';
    tr.appendChild(cell('#' + r.issue_number));
    tr.appendChild(cell(r.remote));
    tr.appendChild(cell(r.status, 'status ' + r.status));
    tr.appendChild(cell(r.current_state));
    tr.appendChild(linkCell(r.pr_url, 'PR'));
    tr.addEventListener('click', () => tailLog(r.id));
    return tr;
  });
}

function renderRunners(rows) {
  replaceRows($('runners'), rows, (r) => {
    const tr = document.createElement('tr');
    tr.appendChild(cell(r.hostname));
    tr.appendChild(cell(r.status, 'status'));
    tr.appendChild(cell(r.capacity));
    tr.appendChild(cell(r.active_leases));
    tr.appendChild(cell(r.last_heartbeat ? new Date(r.last_heartbeat).toLocaleTimeString() : '-'));
    return tr;
  });
}

function renderEgress(rows) {
  replaceRows($('egress'), rows, (r) => {
    const tr = document.createElement('tr');
    tr.appendChild(cell(r.host));
    tr.appendChild(cell(r.allowed ? 'allowed' : 'DENIED', r.allowed ? 'allowed' : 'denied'));
    tr.appendChild(cell(new Date(r.ts).toLocaleTimeString()));
    return tr;
  });
}

function tailLog(runID) {
  if (es) es.close();
  $('logbody').textContent = '';
  $('logrun').textContent = '— run ' + runID;
  es = new EventSource('/v1/runs/' + encodeURIComponent(runID) + '/logs');
  es.onmessage = (ev) => appendLog(ev.data);
  es.addEventListener('cycle_review_decision', (ev) => appendLog(ev.data));
  es.addEventListener('implementer_result', (ev) => appendLog(ev.data));
  es.onerror = () => { /* keep the last buffer; SSE auto-reconnects */ };
}

function appendLog(data) {
  const body = $('logbody');
  body.textContent += data + '\n'; // textContent: log payload is never HTML
  body.scrollTop = body.scrollHeight;
}

refresh();
setInterval(refresh, 4000);
</script>
</body>
</html>
`
