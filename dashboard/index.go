package main

// indexHTML is the entire dashboard: a single page that polls the flowd read
// API for runs/runners/egress, live-tails a selected run's log, and (for
// admins) exposes the §7 admin controls — runner list, secrets form,
// merge-policy form (issue #11).
//
// No build step, no framework — vanilla JS keeps the dashboard light
// (decision 7: metadata + logs only).
//
// Identity model:
//   - First load: no session token in sessionStorage → render device-flow
//     login (POST /v1/auth/device/start, show user_code + verification_uri,
//     poll /v1/auth/device/poll). On success store session_token in
//     sessionStorage and reload state.
//   - Subsequent calls: every fetch carries `Authorization: Bearer <token>`.
//     SSE goes through fetch + ReadableStream (EventSource cannot send custom
//     headers) so the same bearer authenticates the log tail.
//   - GET /v1/me returns {user_id, github_login, role, capabilities[]}. The UI
//     renders admin panels only when the capability list contains the matching
//     §7 capability — role strings are never the gate. This keeps the wire
//     contract (§7 table) the single source of truth.
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
  header { padding: 12px 20px; background: #1a1d24; border-bottom: 1px solid #2a2f3a;
    display: flex; justify-content: space-between; align-items: center; }
  header h1 { font-size: 16px; margin: 0; font-weight: 600; }
  header .who { font-size: 12px; color: #8a93a6; }
  header .who strong { color: #e6e6e6; font-weight: 600; }
  header button { background: #2a2f3a; color: #e6e6e6; border: 1px solid #3a4050;
    padding: 4px 10px; border-radius: 4px; font-size: 12px; cursor: pointer; margin-left: 8px; }
  header button:hover { background: #353b48; }
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

  /* login view */
  #login { padding: 48px 16px; max-width: 480px; margin: 0 auto; text-align: center; }
  #login p { color: #8a93a6; line-height: 1.5; }
  #login .code { font-family: ui-monospace, "SF Mono", monospace; font-size: 32px;
    letter-spacing: .15em; color: #e6e6e6; padding: 12px 18px; border: 1px solid #3a4050;
    border-radius: 6px; display: inline-block; margin: 12px 0; }
  #login a.cta { display: inline-block; padding: 10px 18px; background: #2563eb;
    color: white; border-radius: 6px; text-decoration: none; font-weight: 600; }
  #login a.cta:hover { background: #1d4ed8; }
  #login .err { color: #f87171; margin-top: 16px; }

  /* admin forms */
  .admin-grid { grid-column: 1 / 3; display: grid; grid-template-columns: 1fr 1fr;
    gap: 16px; }
  form.adminform { display: grid; gap: 8px; }
  form.adminform label { display: grid; gap: 4px; font-size: 12px; color: #8a93a6; }
  form.adminform input, form.adminform select, form.adminform textarea {
    background: #0b0d12; color: #e6e6e6; border: 1px solid #2a2f3a; border-radius: 4px;
    padding: 6px 8px; font-size: 13px; font-family: inherit; }
  form.adminform button { background: #2563eb; color: white; border: none;
    padding: 8px 12px; border-radius: 4px; cursor: pointer; font-size: 13px;
    font-weight: 600; justify-self: start; }
  form.adminform button:hover { background: #1d4ed8; }
  form.adminform .msg { font-size: 12px; min-height: 1.2em; }
  form.adminform .msg.ok { color: #4ade80; }
  form.adminform .msg.err { color: #f87171; }
  form.adminform .checkbox { display: flex; gap: 6px; align-items: center;
    flex-direction: row; color: #e6e6e6; }
</style>
</head>
<body>

<div id="root"></div>

<script>
const $ = (id) => document.getElementById(id);
const SESSION_KEY = 'flow.sessionToken';
let logStream = null;       // {abort: () => void} | null — current SSE reader.
let me = null;              // /v1/me payload once authenticated.

// ---- auth helpers ---------------------------------------------------------

function token() { return sessionStorage.getItem(SESSION_KEY) || ''; }

function setToken(t) {
  if (t) sessionStorage.setItem(SESSION_KEY, t);
  else sessionStorage.removeItem(SESSION_KEY);
}

// authFetch wraps fetch with the session bearer; on 401 it clears the token and
// re-renders the login view so an expired session can't silently swallow calls.
async function authFetch(url, opts) {
  const headers = Object.assign({}, (opts && opts.headers) || {});
  const t = token();
  if (t) headers['Authorization'] = 'Bearer ' + t;
  const resp = await fetch(url, Object.assign({}, opts, { headers }));
  if (resp.status === 401) {
    setToken('');
    me = null;
    renderLogin('Session expired — log in again.');
    throw new Error('unauthenticated');
  }
  return resp;
}

function hasCap(c) {
  return me && Array.isArray(me.capabilities) && me.capabilities.indexOf(c) !== -1;
}

// ---- DOM helpers ----------------------------------------------------------

function el(tag, attrs, children) {
  const node = document.createElement(tag);
  if (attrs) {
    for (const k of Object.keys(attrs)) {
      if (k === 'class') node.className = attrs[k];
      else if (k === 'text') node.textContent = attrs[k];
      else if (k.startsWith('on') && typeof attrs[k] === 'function')
        node.addEventListener(k.slice(2), attrs[k]);
      else node.setAttribute(k, attrs[k]);
    }
  }
  if (children) for (const c of children) if (c != null) node.appendChild(
    typeof c === 'string' ? document.createTextNode(c) : c);
  return node;
}

function cell(text, cls) {
  const td = document.createElement('td');
  td.textContent = text == null ? '-' : String(text);
  if (cls) td.className = cls;
  return td;
}

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

// ---- login view ----------------------------------------------------------

async function renderLogin(errMsg) {
  if (logStream) { logStream.abort(); logStream = null; }
  const root = $('root');
  root.replaceChildren();

  const view = el('div', { id: 'login' }, [
    el('h1', { text: 'Flow — kirjaudu sisään' }),
    el('p', { text: 'Aloita GitHub-laitevirtuaalinen kirjautuminen.' }),
    el('button', { onclick: startLogin, text: 'Aloita kirjautuminen' }),
  ]);
  if (errMsg) view.appendChild(el('div', { class: 'err', text: errMsg }));
  root.appendChild(view);
}

async function startLogin() {
  const root = $('root');
  root.replaceChildren(el('div', { id: 'login' }, [
    el('p', { text: 'Pyydetään koodia GitHubilta…' }),
  ]));

  let start;
  try {
    const resp = await fetch('/v1/auth/device/start', { method: 'POST' });
    if (!resp.ok) {
      const txt = await resp.text();
      renderLogin('device/start: ' + resp.status + ' ' + txt);
      return;
    }
    start = await resp.json();
  } catch (e) {
    renderLogin('device/start network error: ' + e.message);
    return;
  }

  // Render the user code + verification URI; user opens it in another tab.
  const view = el('div', { id: 'login' }, [
    el('h1', { text: 'Kirjaudu GitHubilla' }),
    el('p', { text: 'Avaa allaoleva linkki ja syötä koodi GitHubin sivulla.' }),
    el('div', { class: 'code', text: start.user_code }),
    el('p', null, [
      el('a', { class: 'cta', href: start.verification_uri, target: '_blank',
                rel: 'noopener noreferrer', text: 'Avaa GitHub' }),
    ]),
    el('p', { class: 'muted', text: 'Odotetaan vahvistusta…' }),
  ]);
  $('root').replaceChildren(view);

  // Poll on the GitHub-suggested interval (default 5 s on the server side).
  const interval = (start.interval || 5) * 1000;
  let pollTimer = null;
  const poll = async () => {
    try {
      const resp = await fetch('/v1/auth/device/poll', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ device_code: start.device_code }),
      });
      if (resp.status === 429) {
        // GitHub asked us to slow_down — widen the interval by 5 s.
        clearInterval(pollTimer);
        pollTimer = setInterval(poll, interval + 5000);
        return;
      }
      if (resp.status === 410) {
        clearInterval(pollTimer);
        renderLogin('Koodi vanheni. Aloita uudelleen.');
        return;
      }
      if (resp.status === 403) {
        clearInterval(pollTimer);
        renderLogin('Kirjautuminen peruutettiin GitHubissa.');
        return;
      }
      if (!resp.ok) {
        clearInterval(pollTimer);
        const txt = await resp.text();
        renderLogin('device/poll: ' + resp.status + ' ' + txt);
        return;
      }
      const out = await resp.json();
      if (out.pending) return; // keep polling
      if (out.session_token) {
        clearInterval(pollTimer);
        setToken(out.session_token);
        bootstrap();
      }
    } catch (e) {
      // Transient network error; the timer keeps trying.
    }
  };
  pollTimer = setInterval(poll, interval);
  poll();
}

// ---- dashboard view ------------------------------------------------------

function renderShell() {
  $('root').replaceChildren();

  const head = el('header', null, [
    el('h1', { text: 'Flow — dashboard' }),
    el('div', { class: 'who' }, [
      'Kirjautunut: ',
      el('strong', { text: me.github_login || me.user_id }),
      ' (' + me.role + ')',
      el('button', { onclick: logout, text: 'Kirjaudu ulos' }),
    ]),
  ]);

  const mainEl = el('main', { id: 'main' });

  // Read panels — every authenticated user.
  mainEl.appendChild(panelTable('Runs', 'runs',
    ['Issue', 'Remote', 'Status', 'State', 'PR']));
  mainEl.appendChild(panelTable('Runners', 'runners',
    ['Host', 'Status', 'Cap', 'Active', 'Heartbeat']));
  mainEl.appendChild(panelTable('Egress log', 'egress',
    ['Host', 'Result', 'Time']));

  const logSec = el('section', { id: 'log' }, [
    el('h2', null, [
      'Live log ',
      el('span', { id: 'logrun', class: 'muted', text: '— valitse run' }),
    ]),
    el('pre', { id: 'logbody' }),
  ]);
  mainEl.appendChild(logSec);

  // Admin-only controls. Each panel is rendered iff the matching §7 capability
  // is in /v1/me's capabilities[]. Developer sees nothing here.
  if (hasCap('secrets.manage') || hasCap('merge_policy.manage')) {
    const admin = el('div', { class: 'admin-grid' });
    if (hasCap('secrets.manage')) admin.appendChild(renderSecretsForm());
    if (hasCap('merge_policy.manage')) admin.appendChild(renderMergePolicyForm());
    mainEl.appendChild(admin);
  }

  $('root').appendChild(head);
  $('root').appendChild(mainEl);
}

function panelTable(title, id, headers) {
  const sec = el('section', null, [el('h2', { text: title })]);
  const tbl = el('table');
  const thead = el('thead', null, [el('tr', null,
    headers.map((h) => el('th', { text: h })))]);
  const tbody = el('tbody', { id });
  tbl.appendChild(thead);
  tbl.appendChild(tbody);
  sec.appendChild(tbl);
  return sec;
}

function logout() {
  setToken('');
  me = null;
  if (logStream) { logStream.abort(); logStream = null; }
  renderLogin();
}

// ---- read-panel rendering -----------------------------------------------

async function refresh() {
  try {
    // GET /v1/runners is admin-only; the developer view 403s. Skip the call so
    // the developer's network tab isn't littered with red rows.
    const runnersP = hasCap('runners.manage.shared')
      ? authFetch('/v1/runners').then((r) => r.json())
      : Promise.resolve({ runners: [] });
    const [runs, runners, egress] = await Promise.all([
      authFetch('/v1/runs').then((r) => r.json()),
      runnersP,
      authFetch('/v1/egress').then((r) => r.json()),
    ]);
    renderRuns(runs.runs || []);
    renderRunners(runners.runners || []);
    renderEgress(egress.egress || []);
  } catch (e) { /* either central is down or authFetch already redirected to login */ }
}

function renderRuns(rows) {
  const tbody = $('runs');
  if (!tbody) return;
  replaceRows(tbody, rows, (r) => {
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
  const tbody = $('runners');
  if (!tbody) return;
  replaceRows(tbody, rows, (r) => {
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
  const tbody = $('egress');
  if (!tbody) return;
  replaceRows(tbody, rows, (r) => {
    const tr = document.createElement('tr');
    tr.appendChild(cell(r.host));
    tr.appendChild(cell(r.allowed ? 'allowed' : 'DENIED', r.allowed ? 'allowed' : 'denied'));
    tr.appendChild(cell(new Date(r.ts).toLocaleTimeString()));
    return tr;
  });
}

// ---- SSE log tail --------------------------------------------------------
//
// EventSource doesn't support custom Authorization headers, so we read the SSE
// stream via fetch + ReadableStream and parse the wire format ourselves. The
// parser is intentionally minimal: data:-lines accumulate into a frame, blank
// lines flush it. Named events (event: cycle_review_decision) are surfaced
// distinct from default messages.

function tailLog(runID) {
  if (logStream) logStream.abort();
  const body = $('logbody');
  if (body) body.textContent = '';
  const runLabel = $('logrun');
  if (runLabel) runLabel.textContent = '— run ' + runID;

  const ctrl = new AbortController();
  logStream = ctrl;
  authFetch('/v1/runs/' + encodeURIComponent(runID) + '/logs', {
    headers: { 'Accept': 'text/event-stream' },
    signal: ctrl.signal,
  }).then(async (resp) => {
    if (!resp.ok || !resp.body) return;
    const reader = resp.body.getReader();
    const dec = new TextDecoder();
    let buf = '';
    let eventName = '';
    let dataLines = [];
    const flush = () => {
      if (dataLines.length === 0) { eventName = ''; return; }
      appendLog(dataLines.join('\n'));
      eventName = '';
      dataLines = [];
    };
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf('\n')) !== -1) {
        const line = buf.slice(0, nl).replace(/\r$/, '');
        buf = buf.slice(nl + 1);
        if (line === '') { flush(); continue; }
        if (line.startsWith(':')) continue; // comment / heartbeat
        if (line.startsWith('event:')) { eventName = line.slice(6).trim(); continue; }
        if (line.startsWith('data:')) { dataLines.push(line.slice(5).replace(/^ /, '')); continue; }
      }
    }
  }).catch(() => { /* aborted or network blip — reconnection is a future improvement */ });
}

function appendLog(data) {
  const body = $('logbody');
  if (!body) return;
  body.textContent += data + '\n';
  body.scrollTop = body.scrollHeight;
}

// ---- admin forms ---------------------------------------------------------

// renderSecretsForm submits to POST /v1/secrets (issue #10, may not be
// implemented yet — the form degrades to "endpoint missing" until then;
// allowed by the cycle review: "Secrets-lomake voidaan rakentaa kontraktia
// vasten").
function renderSecretsForm() {
  const msg = el('div', { class: 'msg' });
  const sec = el('section');
  sec.appendChild(el('h2', { text: 'Secrets (admin)' }));
  const form = el('form', { class: 'adminform' }, [
    el('label', null, ['Avain (esim. GH_TOKEN)',
      el('input', { name: 'key', required: '', autocomplete: 'off' })]),
    el('label', null, ['Arvo',
      el('input', { name: 'value', type: 'password', required: '', autocomplete: 'off' })]),
    el('label', null, ['Toimitustapa',
      (() => {
        const s = el('select', { name: 'delivery' });
        for (const d of ['env', 'proxy']) s.appendChild(el('option', { value: d, text: d }));
        return s;
      })()]),
    el('button', { type: 'submit', text: 'Tallenna' }),
    msg,
  ]);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    msg.className = 'msg';
    msg.textContent = 'Tallennetaan…';
    const data = Object.fromEntries(new FormData(form));
    try {
      const resp = await authFetch('/v1/secrets', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key: data.key, value: data.value, delivery: data.delivery }),
      });
      if (resp.status === 404) {
        msg.className = 'msg err';
        msg.textContent = 'Endpoint puuttuu vielä (issue #10 — secrets broker).';
        return;
      }
      if (!resp.ok) {
        const t = await resp.text();
        msg.className = 'msg err';
        msg.textContent = resp.status + ' ' + t;
        return;
      }
      msg.className = 'msg ok';
      msg.textContent = 'Tallennettu.';
      form.reset();
    } catch (e) {
      msg.className = 'msg err';
      msg.textContent = e.message;
    }
  });
  sec.appendChild(form);
  return sec;
}

// renderMergePolicyForm submits to PUT /v1/projects/{id}/merge-policy. Project
// id is a free-form input — listing projects in the dashboard is a follow-up;
// an admin running this form already knows the id from "flowctl project list"
// or from the runs table.
function renderMergePolicyForm() {
  const msg = el('div', { class: 'msg' });
  const sec = el('section');
  sec.appendChild(el('h2', { text: 'Merge-policy (admin)' }));
  const form = el('form', { class: 'adminform' }, [
    el('label', null, ['Project ID (uuid)',
      el('input', { name: 'project_id', required: '', autocomplete: 'off',
                    pattern: '[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}' })]),
    el('label', null, ['Merge label (esim. auto-merge)',
      el('input', { name: 'label', autocomplete: 'off' })]),
    el('label', { class: 'checkbox' }, [
      el('input', { type: 'checkbox', name: 'conflict_resolution' }),
      'Auto-rebase BEHIND/DIRTY PRs',
    ]),
    el('button', { type: 'submit', text: 'Päivitä' }),
    msg,
  ]);
  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    msg.className = 'msg';
    msg.textContent = 'Päivitetään…';
    const fd = new FormData(form);
    const projectID = fd.get('project_id');
    const payload = {};
    const label = fd.get('label');
    if (label !== null && label !== '') payload.label = label;
    payload.conflict_resolution = fd.get('conflict_resolution') === 'on';
    try {
      const resp = await authFetch('/v1/projects/' + encodeURIComponent(projectID) + '/merge-policy', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      });
      if (!resp.ok) {
        const t = await resp.text();
        msg.className = 'msg err';
        msg.textContent = resp.status + ' ' + t;
        return;
      }
      msg.className = 'msg ok';
      msg.textContent = 'Päivitetty.';
    } catch (e) {
      msg.className = 'msg err';
      msg.textContent = e.message;
    }
  });
  sec.appendChild(form);
  return sec;
}

// ---- bootstrap -----------------------------------------------------------

async function bootstrap() {
  if (!token()) { renderLogin(); return; }
  try {
    const resp = await authFetch('/v1/me');
    if (!resp.ok) {
      setToken('');
      renderLogin('Could not resolve identity (' + resp.status + ').');
      return;
    }
    me = await resp.json();
  } catch (e) {
    return; // authFetch already redirected to login on 401
  }
  renderShell();
  refresh();
  setInterval(refresh, 4000);
}

bootstrap();
</script>
</body>
</html>
`
