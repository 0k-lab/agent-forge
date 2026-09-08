"use strict";

const statusColumns = Object.freeze({
  pending: 'Ready', retry_wait: 'Ready', leased: 'Working',
  delivering: 'Review & CI', succeeded: 'Done', failed: 'Blocked'
});
const columns = ['Backlog', 'Ready', 'Working', 'Review & CI', 'Done', 'Blocked'];
const backlogs = new Map();
let backlogRequest = 0, backlogLoad = Promise.resolve();
const byId = id => document.getElementById(id);
let token = '', overview = null, selectedJob = '', submitPending = false, refreshing = false;
let detailEpoch = 0, detailRequest = 0, startRequest = 0;
let session = 0, selectedTask = null, selectedActivity = null;
const resumePending = new Set();

function add(parent, tag, text, className = '') {
  const node = document.createElement(tag);
  node.textContent = text;
  node.className = className;
  parent.appendChild(node);
  return node;
}
function button(parent, text, action, className = '') {
  const node = add(parent, 'button', text, className);
  node.type = 'button';
  node.addEventListener('click', action);
  return node;
}
function safeLink(parent, value, label) {
  if (!value) return;
  try {
    const url = new URL(value);
    if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash) return;
    const link = add(parent, 'a', label);
    link.href = url.href;
    link.target = '_blank';
    link.rel = 'noopener noreferrer';
  } catch { /* Absent or unsafe source: no link. */ }
}
function stamp(value) {
  if (!value || value.startsWith('0001-')) return 'Not reported';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? 'Not reported' : date.toLocaleString();
}
function duration(job) {
  const end = ['succeeded', 'failed'].includes(job.status) ? new Date(job.updated_at).getTime() : Date.now();
  const seconds = Math.max(0, Math.floor((end - new Date(job.created_at).getTime()) / 1000));
  if (!Number.isFinite(seconds)) return 'Not reported';
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  return `${Math.floor(seconds / 3600)}h ${Math.floor(seconds % 3600 / 60)}m`;
}
function fields(parent, pairs) {
  const list = add(parent, 'dl', '');
  for (const [label, value] of pairs) {
    add(list, 'dt', label);
    add(list, 'dd', value || 'Not reported');
  }
}
function notice(message, error = false) {
  byId('notice').textContent = message;
  byId('notice').className = error ? 'error' : '';
}
function lockWorkspace(message = 'Workspace locked.') {
  session++;
  resumePending.clear();
  token = ''; overview = null; selectedJob = ''; selectedTask = null; selectedActivity = null; backlogs.clear(); backlogRequest++;
  byId('backlog-notice').textContent = 'Select a project to load its public issue backlog.';
  byId('backlog-refresh').disabled = true;
  byId('token').value = '';
  byId('auth-panel').hidden = false;
  byId('lock').hidden = true;
  for (const id of ['new-task', 'refresh', 'project-filter']) byId(id).disabled = true;
  for (const id of ['board', 'workers', 'worker-summary', 'detail-content', 'task-project', 'runs', 'runs-summary', 'project-nav']) byId(id).replaceChildren();
  for (const id of ['task-modal', 'detail-modal']) if (byId(id).open) byId(id).close();
  byId('task-form').reset();
  byId('project-filter').replaceChildren();
  add(byId('project-filter'), 'option', 'All projects').value = '';
  byId('project-info').textContent = 'Connect to see configured projects.';
  notice(message, message.startsWith('Unauthorized'));
}
async function api(path, body) {
  const currentSession = session;
  let response;
  try {
    response = await fetch(path, {
      method: body === undefined ? 'GET' : 'POST',
      headers: {Authorization: `Bearer ${token}`, ...(body === undefined ? {} : {'Content-Type': 'application/json'})},
      ...(body === undefined ? {} : {body: JSON.stringify(body)}),
      cache: 'no-store', credentials: 'omit'
    });
  } catch {
    throw new Error(body === undefined ? 'Offline — unable to reach the gate. Reconnecting every 5 seconds.' : 'Connection lost. Check the board before submitting again; the task may have been created.');
  }
  if (currentSession !== session) throw new Error('Workspace session changed.');
  if (response.status === 401) {
    lockWorkspace('Unauthorized — enter a valid owner token.');
    byId('token').focus();
    throw new Error('Unauthorized — enter a valid owner token.');
  }
  if (!response.ok) {
    const messages = {400: 'Invalid task. Check the project, title, instructions, source URL and checks.', 404: 'Task not found.', 409: 'Delivery is not awaiting review. Refresh the run.', 413: 'Task detail or request exceeds the supported limit.', 422: 'Public source preparation is unavailable. Check the project configuration and default branch.', 502: 'Repository preparation failed. The public source could not be fetched.'};
    throw new Error(messages[response.status] || `Request failed (${response.status}). Try refreshing.`);
  }
  const data = await response.json();
  if (currentSession !== session) throw new Error('Workspace session changed.');
  return data;
}
function projectInfo() {
  const selected = overview?.projects.find(p => p.id === byId('project-filter').value);
  const target = byId('project-info'); target.replaceChildren();
  if (!selected) {
    add(target, 'p', overview ? `${overview.projects.length} configured projects` : 'Connect to see configured projects.');
    return;
  }
  for (const text of [`Branch · ${selected.default_branch}`, `Pool · ${selected.worker_pool}`, `Plugin agent · ${selected.agent}`, `Public source · ${selected.public_source ? 'Available' : 'Unavailable'}`, `Delivery · ${selected.delivery ? 'Available' : 'Unavailable'}`]) add(target, 'p', text);
}
function taskLane(task) {
  if (task.issue?.state === 'closed') return 'Done';
  if (task.issue?.labels?.includes('blocked')) return 'Blocked';
  if (task.latest_run && statusColumns[task.latest_run.status]) return statusColumns[task.latest_run.status];
  return task.issue?.labels?.includes('ready-for-agent') ? 'Ready' : 'Backlog';
}
function taskKey(task) { return `${task.project} ${task.source_ref}`; }
function boardTasks() {
  const tasks = new Map((overview?.tasks || []).map(task => [taskKey(task), {...task}]));
  const filter = byId('project-filter').value;
  for (const [project, backlog] of backlogs) {
    if (!backlog.available || (filter && project !== filter)) continue;
    for (const issueTask of backlog.tasks) {
      const local = tasks.get(taskKey(issueTask));
      const task = {...issueTask};
      if (local?.latest_run && (!task.latest_run || local.latest_run.created_at >= task.latest_run.created_at)) {
        task.latest_run = local.latest_run; task.run_count = local.run_count;
      }
      tasks.set(taskKey(task), task);
    }
  }
  for (const task of tasks.values()) {
    // Recent run updates also refresh issues outside the bounded local-task list.
    for (const run of overview?.jobs || []) {
      if (run.project === task.project && run.source_ref === task.source_ref && (!task.latest_run || run.id === task.latest_run.id || run.created_at > task.latest_run.created_at)) task.latest_run = run;
    }
  }
  return [...tasks.values()];
}
function renderBoard() {
  const focusedTask = document.activeElement?.dataset?.jobId;
  const board = byId('board'); board.replaceChildren();
  const filter = byId('project-filter').value;
  const tasks = boardTasks().filter(task => !filter || task.project === filter);
  const empty = {'Backlog':'No unscheduled open issues in the loaded project feeds.', 'Ready':'No issues labeled ready-for-agent or queued linked runs.', 'Working':'No linked coding run is working.', 'Review & CI':'No linked run is awaiting delivery or CI.', 'Done':'No closed issues or successful linked runs loaded.', 'Blocked':'No blocked issues or failed linked runs loaded.'};
  for (const name of columns) {
    const column = add(board, 'section', '', 'column'); column.setAttribute('aria-label', name);
    const heading = add(column, 'h2', '', 'column-heading');
    add(heading, 'span', '', 'dot').setAttribute('aria-hidden', 'true'); add(heading, 'span', name);
    const cards = tasks.filter(task => taskLane(task) === name);
    add(heading, 'span', String(cards.length), 'count');
    if (!cards.length) add(column, 'p', overview ? empty[name] : 'Connect to load tasks.', 'empty');
    for (const task of cards) {
      const card = add(column, 'article', '', 'card');
      const taskButton = button(card, task.title, () => openTaskDetail(task), 'card-title');
      taskButton.dataset.jobId = taskKey(task);
      if (focusedTask === taskKey(task)) taskButton.focus({preventScroll:true});
      const meta = add(card, 'div', '', 'card-meta');
      add(meta, 'span', task.project || 'Unassigned project', 'tag');
      add(meta, 'p', task.issue ? `Issue #${task.issue.number}` : 'Linked local task · issue state not loaded');
      const run = task.latest_run;
      if (run) {
        add(meta, 'p', `Latest run · ${run.worker_id || 'Awaiting worker'} · ${run.agent || 'Agent not reported'}`);
        if (run.delivery) add(meta, 'p', `Delivery · ${deliveryPhase(run.delivery.phase)}`);
        add(meta, 'p', `Updated ${stamp(run.updated_at)} · ${duration(run)} elapsed`);
      } else add(meta, 'p', 'No Forge runs yet.');
      safeLink(meta, task.source_ref, 'Open source ↗');
    }
  }
}
function runState(status) {
  return {pending:'Queued',retry_wait:'Waiting for another attempt',leased:'Working',delivering:'Review & CI',succeeded:'Succeeded',failed:'Failed'}[status] || 'Not reported';
}
function deliveryPhase(phase) {
  return {awaiting_review:'Awaiting independent review',pending:'Waiting to publish',publishing:'Publishing candidate',ci:'Waiting for CI',merging:'Merging pull request',retry_wait:'Waiting for another delivery attempt',merged:'Merged',failed:'Delivery stopped'}[phase] || 'Not reported';
}
function ciState(state) { return {pending:'Pending',success:'Passed',failure:'Failed',failed:'Failed'}[state] || 'Not reported'; }
function renderRuns() {
  const project = byId('project-filter').value, status = byId('run-status').value;
  const all = overview?.jobs || [];
  const runs = all.filter(run => (!project || run.project === project) && (!status || run.status === status));
  byId('runs-summary').textContent = `${runs.length} recent runs · ${runs.filter(run => !run.source_ref).length} unlinked${overview?.jobs_truncated ? ' · Latest 100 runs only' : ''}. Runs are execution history, not product tasks.`;
  const root = byId('runs'); root.replaceChildren();
  if (!runs.length) add(root, 'p', 'No runs match this project and status.', 'empty');
  for (const run of runs) {
    const card = add(root, 'article', '', 'card');
    button(card, `Run/Job · ${run.title}`, () => openDetail(run.id), 'card-title');
    add(card, 'p', `${run.project || 'Unassigned project'} · ${runState(run.status)} · ${stamp(run.created_at)}`, 'hint');
    if (run.source_ref) safeLink(card, run.source_ref, 'Linked source ↗');
    else add(card, 'p', 'Unlinked run — no product task source.', 'hint');
  }
}
function renderProjectNav() {
  const target = byId('project-nav'), selected = byId('project-filter').value;
  const focused = document.activeElement?.dataset?.project;
  target.replaceChildren();
  for (const [id, label] of [['', 'All projects'], ...(overview?.projects || []).map(p => [p.id, p.id])]) {
    const node = button(target, label, () => selectProject(id), selected === id ? 'nav active' : 'nav');
    node.dataset.project = id; node.setAttribute('aria-current', selected === id ? 'true' : 'false');
    node.disabled = !overview;
    if (focused === id) node.focus({preventScroll:true});
  }
}
function selectProject(id) {
  byId('project-filter').value = id;
  renderProjectNav(); projectInfo(); renderBoard(); renderRuns(); renderWorkers();
  return loadBacklog();
}
function backlogProjects() {
  const selected = byId('project-filter').value;
  return (overview?.projects || []).filter(p => p.public_source && (!selected || p.id === selected));
}
function backlogNotice(loading = false) {
  const projects = backlogProjects(), missing = projects.filter(p => backlogs.get(p.id)?.available === false);
  const count = projects.reduce((n,p) => n + (backlogs.get(p.id)?.available ? backlogs.get(p.id).tasks.length : 0), 0);
  const scope = byId('project-filter').value || 'All projects';
  const unavailable = missing.length ? ` Backlog unavailable: ${missing.slice(0,5).map(p => p.id).join(', ')}${missing.length > 5 ? ` (+${missing.length - 5} more)` : ''}. Linked local tasks remain visible.` : '';
  const limited = projects.some(p => backlogs.get(p.id)?.truncated) ? ' Feeds limited to 50 upstream records each.' : '';
  byId('backlog-notice').textContent = projects.length ? `${scope} · ${count} public issue records${loading ? ' · Loading…' : ' · Backlogs refresh every 5 min.'}${limited}${unavailable}` : `${scope} · Public issue backlog unavailable: no public-source projects configured.`;
}
function loadBacklog(force = false) {
  const request = ++backlogRequest, currentSession = session;
  const projects = backlogProjects();
  byId('backlog-refresh').disabled = !token || !projects.length;
  const current = () => request === backlogRequest && currentSession === session && !!token;
  backlogNotice(true);
  // Serialize batches so navigation cannot multiply the three-request ceiling.
  const batch = async () => {
    let next = 0;
    const worker = async () => {
      while (current() && next < projects.length) {
        const project = projects[next++];
        if (!force && Date.now() - (backlogs.get(project.id)?.checkedAt || 0) < 300000) continue;
        let entry;
        try {
          const data = await api(`/v1/control/projects/${encodeURIComponent(project.id)}/issues`);
          entry = {available:true, tasks:data.tasks, truncated:data.truncated, checkedAt:Date.now()};
        } catch { entry = {available:false, tasks:[], checkedAt:Date.now()}; }
        if (!current()) return;
        backlogs.set(project.id,entry); renderBoard(); backlogNotice(true);
      }
    };
    await Promise.all(Array.from({length:Math.min(3,projects.length)}, worker));
    if (current()) { renderBoard(); backlogNotice(); }
  };
  backlogLoad = backlogLoad.then(batch, batch);
  return backlogLoad;
}
function renderWorkers() {
  const filter = byId('project-filter').value;
  const project = overview.projects.find(p => p.id === filter);
  const workers = overview.workers.filter(w => !project || w.pool === project.worker_pool);
  const connected = workers.filter(w => w.connected).length;
  const occupied = workers.filter(w => w.occupied).length;
  const free = workers.filter(w => w.connected && !w.occupied).length;
  byId('worker-summary').textContent = `${workers.length} slots · ${connected} connected · ${occupied} occupied · ${free} connected and free${overview.workers_truncated ? ' · Runtime history limited' : ''}`;
  const target = byId('workers'); target.replaceChildren();
  if (!workers.length) add(target, 'p', 'No worker slots available for this project.', 'empty');
  for (const worker of workers) {
    const card = add(target, 'article', '', 'card worker-card');
    const heading = add(card, 'div', '', 'worker-heading');
    add(heading, 'h2', worker.base_id || worker.id);
    add(heading, 'span', worker.connected ? '● Connected' : '○ Offline', 'worker-state');
    const agents = [...new Set(overview.projects.filter(p => p.worker_pool === worker.pool).map(p => p.agent))];
    fields(card, [['Slot', String(worker.slot)], ['Pool', worker.pool], ['Capacity', worker.occupied ? 'Occupied · 1 of 1' : worker.connected ? 'Free · 1 of 1' : 'Offline · unavailable'], ['Plugin agent', worker.agent || (agents.length ? `Configured for pool: ${agents.join(', ')}` : 'Not reported')], ['Last seen / heartbeat', stamp(worker.last_seen)]]);
    if (worker.active_job_id) {
      const job = overview.jobs.find(j => j.id === worker.active_job_id);
      button(card, job ? job.title : 'Inspect assigned run', () => openDetail(worker.active_job_id));
    } else add(card, 'p', 'No assigned active job', 'hint');
  }
}
function renderOverview(data) {
  overview = data;
  const filter = byId('project-filter'), previous = filter.value;
  filter.replaceChildren(); add(filter, 'option', 'All projects').value = '';
  for (const project of data.projects) add(filter, 'option', project.id).value = project.id;
  filter.value = data.projects.some(p => p.id === previous) ? previous : '';
  filter.disabled = false;
  byId('new-task').disabled = !data.projects.some(p => p.public_source);
  byId('refresh').disabled = false;
  byId('auth-panel').hidden = true; byId('lock').hidden = false;
  for (const id of backlogs.keys()) if (!data.projects.some(p => p.id === id && p.public_source)) backlogs.delete(id);
  renderProjectNav(); projectInfo(); renderBoard(); renderWorkers(); renderRuns();
}
async function refresh() {
  if (!token || refreshing) return;
  refreshing = true;
  const currentSession = session;
  notice(overview ? 'Refreshing…' : 'Loading projects, worker slots and work…');
  try {
    const data = await api('/v1/control/overview');
    if (currentSession !== session) return;
    renderOverview(data);
    notice(`${(data.tasks || []).length} linked local tasks (up to 100 sources) · ${data.jobs.length} recent runs${data.jobs_truncated ? ' · Showing the latest 100; older runs are outside this view' : ''} · Updated ${new Date().toLocaleTimeString()} · Refreshes every 5s`);
    await loadBacklog();
    if (byId('detail-modal').open) {
      if (selectedTask) await loadTaskDetail(selectedTask);
      else if (selectedJob) await loadDetail(selectedJob);
    }
  } catch (error) { if (currentSession === session) notice(error.message, true); }
  finally { refreshing = false; }
}
function section(parent, title) {
  const node = add(parent, 'section', '', 'detail-section'); add(node, 'h3', title); return node;
}
function elapsedMS(ms) {
  if (!Number.isFinite(ms) || ms < 0) return 'an unreported duration';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${Math.round(ms / 1000)}s`;
  return `${Math.round(ms / 60000)} min`;
}
function evidenceSummary(evidence, job) {
  const check = `Check ${(evidence.check_index ?? 0) + 1}`, elapsed = elapsedMS(evidence.duration_ms);
  if (evidence.reason === 'scoped_check_passed') return `${check} passed in ${elapsed}.`;
  if (evidence.reason === 'scoped_check_failed') return `${check} failed after ${elapsed}.`;
  if (evidence.reason === 'scoped_check_timeout') return `${check} timed out after ${elapsed}.`;
  if (evidence.reason === 'plugin_protocol_failed') {
    return job.plugin_timeout_ms > 0 && evidence.duration_ms >= job.plugin_timeout_ms
      ? `Coding agent timed out after ${elapsed}.`
      : `Coding agent did not return a valid result after ${elapsed}.`;
  }
  const phrases = {
    cleanup_failed:'Temporary workspace cleanup needs attention.',
    plugin_start_failed:'The coding agent could not start.', plugin_reported_failure:'The coding agent reported a failure.',
    plugin_failed:'The coding agent did not finish.', no_changes:'The coding agent produced no changes.',
    invalid_workspace_change:'Workspace changes did not pass validation.', candidate_commit_failed:'Forge could not save a candidate commit.',
    clone_failed:'Forge could not clone the public source.', fetch_failed:'Forge could not refresh the public source.',
    base_unavailable:'The pinned starting commit was unavailable.', invalid_task:'The run instructions failed validation.',
    invalid_repository:'The repository failed validation.', source_policy_invalid:'The public source policy failed validation.',
    repository_state_unsafe:'Repository safety checks did not pass.', runtime_setup_failed:'The execution environment could not be prepared.',
    worktree_setup_failed:'The working copy could not be prepared.'
  };
  return phrases[evidence.reason] || 'Forge reported execution evidence; see the technical reason below.';
}
function attemptOutcome(attempt, job, hasNext = false) {
  const evidence = attempt.evidence || [];
  if (attempt.status === 'succeeded' && evidence.some(e => e.reason === 'cleanup_failed')) return 'Work completed, but temporary workspace cleanup needs attention.';
  const problem = evidence.find(e => e.reason !== 'scoped_check_passed' && e.reason !== 'cleanup_failed');
  if (problem) return evidenceSummary(problem, job);
  if (attempt.failure_code === 'execution_failed') return attempt.failure_disposition === 'retryable' && (hasNext || job.status === 'retry_wait')
    ? 'Coding agent did not finish; Forge scheduled another attempt.' : 'Coding agent did not finish; no further attempt is scheduled.';
  if (attempt.status === 'leased') return 'Coding agent is working; a result has not been reported.';
  if (attempt.candidate_sha) return 'Produced a candidate commit.';
  if (attempt.status === 'succeeded') return 'Run completed successfully.';
  if (attempt.status === 'failed' || attempt.status === 'expired') return 'This attempt did not complete successfully.';
  return 'No outcome reported yet.';
}
function runAction(job) {
  const states = {pending:'Waiting for an available worker.', retry_wait:'Forge scheduled another attempt.', leased:'Coding agent is working.', delivering:'Forge is delivering the candidate for review and CI.', succeeded:'Run completed successfully.', failed:'Run stopped after failure.'};
  return states[job.status] || 'Run state not reported.';
}
function nextAction(attempt, job, hasNext) {
  if (hasNext) return 'Forge started the next attempt shown below.';
  if (job.status === 'retry_wait') return 'Forge scheduled another attempt.';
  if (attempt.status === 'leased') return 'Forge is waiting for the coding agent result.';
  if (job.status === 'delivering') return 'Forge moved the candidate to review and CI.';
  if (job.status === 'succeeded') return job.delivery?.phase === 'merged' ? 'Forge merged the delivery.' : 'Forge marked this run successful.';
  return 'Forge stopped this run; no further attempt is scheduled.';
}
function copyField(parent, label, value) {
  if (!value) return;
  const row = add(parent, 'div', '', 'technical-field');
  add(row, 'span', label, 'hint'); add(row, 'code', String(value));
  const copy = button(row, 'Copy', async () => {
    try { await navigator.clipboard.writeText(String(value)); copy.textContent = 'Copied'; }
    catch { copy.textContent = 'Select value to copy'; }
  });
  copy.setAttribute('aria-label', `Copy ${label}`);
}
function renderIssueCompletion(node, issue, activity) {
  node.replaceChildren();
  if (issue.state === 'closed') {
    const closed = activity?.data?.issue || issue;
    fields(node, [['Closed by', closed.closed_by || issue.closed_by || 'Not reported'], ['Closed', stamp(closed.closed_at || issue.closed_at)]]);
  }
  const prs = activity?.data?.merged_prs || [];
  for (const pr of prs) {
    const proof = pr.details?.delivery_evidence === true;
    const delivered = section(node, proof ? 'Delivered changes' : 'Related merged PR');
    add(delivered, 'p', proof ? 'GitHub delivery evidence' : 'Related GitHub evidence', 'eyebrow');
    if (pr.details) {
      add(delivered, 'h3', pr.details.title);
      add(delivered, 'p', pr.details.summary, 'instruction');
      add(delivered, 'p', `${pr.details.changed_files} files · +${pr.details.additions} / −${pr.details.deletions}`, 'hint');
      const files = add(delivered, 'ul', '');
      for (const file of pr.details.files) add(files, 'li', `${file.filename} · ${file.status} · +${file.additions} / −${file.deletions}`);
      if (pr.details.summary_truncated) add(delivered, 'p', 'Description shortened; read the full PR.', 'hint');
      if (pr.details.files_truncated) add(delivered, 'p', 'File list is partial (up to 20 safe entries).', 'hint');
    } else add(delivered, 'p', 'Related PR details unavailable.', 'hint');
    safeLink(delivered, pr.url, `${proof ? 'Closing PR' : 'Related merged PR'} #${pr.number} ↗`);
    add(delivered, 'p', `Merged ${stamp(pr.merged_at)}`, 'hint');
  }
  if (activity?.status === 'loading') add(node, 'p', 'Loading additional GitHub evidence…', 'hint');
  if (activity?.status === 'unavailable') add(node, 'p', 'Additional GitHub activity unavailable', 'hint');
  if (activity?.data?.truncated) add(node, 'p', 'GitHub activity is limited to 50 records.', 'hint');
}
async function loadIssueActivity(task, activity) {
  const currentSession = session;
  try {
    activity.data = await api(`/v1/control/projects/${encodeURIComponent(task.project)}/issues/${task.issue.number}/activity`);
    activity.status = activity.data.available ? 'available' : 'unavailable';
  } catch { activity.status = 'unavailable'; }
  if (token && session === currentSession && selectedActivity === activity && activity.node) renderIssueCompletion(activity.node, task.issue, activity);
}
function renderTaskDetail(task, runs, total = runs.length) {
  const root = byId('detail-content');
  const expanded = new Set([...root.querySelectorAll('details')].filter(node => node.open).map(node => node.dataset.disclosure));
  const focused = document.activeElement?.dataset?.disclosure;
  root.replaceChildren();
  const disclosure = (parent, label, key) => {
    const node = add(parent, 'details', ''); node.dataset.disclosure = key; node.open = expanded.has(key);
    const summary = add(node, 'summary', label); summary.dataset.disclosure = key;
    if (focused === key) summary.focus({preventScroll:true});
    return node;
  };
  const latest = runs[runs.length - 1], job = latest?.job || task?.latest_run;
  const title = task?.title || job?.title || 'Run details';
  byId('detail-title').textContent = task ? title : `Run/Job · ${title}`;
  const summary = add(root, 'section', '', 'task-summary');
  const identity = task?.issue ? `Issue #${task.issue.number}` : task ? 'Linked local task · issue state not loaded' : 'Run/Job · execution history';
  add(summary, 'p', identity, 'eyebrow');
  fields(summary, [['Lane / state', task ? taskLane({...task, latest_run:job}) : runState(job?.status)], ['Project', task?.project || job?.project], ['Worker / agent', job ? `${job.worker_id || 'Not assigned'} → ${job.agent || 'Not reported'}` : task?.issue?.state === 'closed' ? 'No linked Forge execution' : 'No run started']]);
  const lastAttempt = latest?.attempts?.at(-1);
  add(summary, 'p', lastAttempt ? attemptOutcome(lastAttempt, job) : job ? runAction(job) : task?.issue?.state === 'closed' ? 'Completed on GitHub. No Forge execution record is linked; this task predates tracking or was completed outside Forge.' : 'No Forge runs yet. Review the brief and start a run.', 'outcome');
  if (job?.delivery) {
    add(summary, 'p', `Delivery: ${deliveryPhase(job.delivery.phase)} · CI: ${ciState(job.delivery.ci_state)}`, 'hint');
    safeLink(summary, job.delivery.pr_url, 'Open pull request ↗');
  }
  safeLink(summary, task?.source_ref || job?.source_ref, 'Open task source ↗');
  if (task?.issue) {
    const node = add(summary, 'section', '', 'completion-evidence');
    const activity = selectedActivity?.key === taskKey(task) ? selectedActivity : null;
    if (activity) activity.node = node;
    renderIssueCompletion(node, task.issue, activity);
  }
  const brief = disclosure(root, 'Full brief', 'brief');
  const briefText = task?.issue?.body || latest?.instruction || 'No brief reported.';
  add(brief, 'p', briefText, 'instruction');
  if (task?.issue?.body_truncated) add(brief, 'p', 'This issue brief is truncated at 8,000 characters. Read the full issue before starting a run.', 'hint');
  if (task && !['pending','retry_wait','leased','delivering'].includes(job?.status) && task.issue?.state !== 'closed') button(summary, 'Start run', () => startIssueTask(task), 'primary');
  const group = section(root, task ? `Runs · ${total}` : 'Run history');
  if (!runs.length) add(group, 'p', 'No runs linked to this exact source URL.', 'hint');
  if (total > runs.length) add(group, 'p', `Showing the latest ${runs.length} of ${total} runs.`, 'hint');
  for (const data of runs) {
    const run = data.job, attempts = data.attempts || [];
    const runNode = add(group, 'section', '', 'run-group');
    add(runNode, 'h3', `Run ${run.ordinal || 1}`);
    add(runNode, 'p', runAction(run), 'outcome');
    add(runNode, 'p', `Started ${stamp(run.created_at)} · Updated ${stamp(run.updated_at)} · ${duration(run)} elapsed`, 'hint');
    if (data.error) { add(runNode, 'p', 'Run details unavailable. Refresh to try again.', 'error'); continue; }
    const reportSection = section(runNode, 'Agent report');
    const reportedAttempts = attempts.filter(attempt => attempt.status === 'succeeded' && attempt.candidate_sha && attempt.agent_report);
    if (!reportedAttempts.length) add(reportSection, 'p', 'Detailed agent report was not captured for this run', 'hint');
    for (const attempt of reportedAttempts) {
      add(reportSection, 'p', `Self-reported · Attempt ${attempt.ordinal}`, 'eyebrow');
      add(reportSection, 'p', attempt.agent_report.summary, 'instruction');
      const changes = add(reportSection, 'ul', '');
      for (const change of attempt.agent_report.changes) add(changes, 'li', change);
    }
    const verification = section(runNode, 'Forge verification');
    if (data.delivery) {
      fields(verification, [['Delivery', deliveryPhase(data.delivery.phase)], ['CI', ciState(data.delivery.ci_state)]]);
      safeLink(verification, data.delivery.pr_url, 'Open run pull request ↗');
      if (run.status === 'delivering' && data.delivery.phase === 'awaiting_review') resumeDeliveryButton(verification, run.id);
    }
    if (data.instruction && data.instruction !== briefText) add(disclosure(runNode, 'Run brief', `run-brief-${run.id}`), 'p', data.instruction, 'instruction');
    add(runNode, 'p', `Worker slot → configured plugin agent · ${run.agent || 'Not reported'}`, 'hint');
    add(runNode, 'p', 'No subagent telemetry reported for this run', 'hint');
    if (!attempts.length) add(verification, 'p', 'No attempts yet; waiting for a worker.', 'hint');
    attempts.forEach((attempt, index) => {
      const item = add(verification, 'section', '', 'attempt');
      add(item, 'h4', `Attempt ${attempt.ordinal}`);
      const outcome = attemptOutcome(attempt, run, index < attempts.length - 1);
      add(item, 'p', outcome, 'outcome');
      const completed = attempt.completed_at && !attempt.completed_at.startsWith('0001-') ? attempt.completed_at : null;
      const elapsed = completed ? new Date(completed) - new Date(attempt.leased_at) : attempt.status === 'leased' ? Date.now() - new Date(attempt.leased_at) : NaN;
      fields(item, [['Worker / agent', `${attempt.worker_id || 'Not reported'} → ${run.agent || 'Not reported'}`], ['Started', stamp(attempt.leased_at)], ['Completed', stamp(completed)], ['Duration', elapsedMS(elapsed)]]);
      if (attempt.candidate_sha && !outcome.includes('candidate')) add(item, 'p', 'Produced a candidate commit.');
      const next = nextAction(attempt, run, index < attempts.length - 1);
      if (!outcome.includes(next)) add(item, 'p', next);
      if (attempt.failure_code) add(item, 'p', attempt.failure_code, 'hint');
      const reported = new Set([outcome]);
      for (const evidence of attempt.evidence || []) {
        const text = evidenceSummary(evidence, run);
        if (!reported.has(text)) add(item, 'p', text);
        reported.add(text);
        add(item, 'p', evidence.reason, 'hint');
      }
    });
    if (data.attempts_truncated) add(runNode, 'p', 'Only the first 100 attempts are available here.', 'hint');
    const messages = {delivery_review:'Awaiting independent review.', delivery_resumed:'Owner resumed delivery.', submitted:'Run queued in Forge.', delivery_pending:'Candidate handed to delivery.', delivery_phase:'Delivery advanced', delivery_retry:'Forge scheduled another delivery attempt.', delivery_merged:'Pull request merged.', delivery_failed:'Delivery stopped after failure.'};
    const events = (data.timeline || []).filter(event => messages[event.type]);
    if (events.length) {
      const timeline = section(runNode, 'Run timeline'), list = add(timeline, 'ol', '', 'timeline');
      const seen = new Set();
      for (const event of events) {
        const text = `${messages[event.type]}${event.type === 'delivery_phase' ? `: ${{publishing:'publishing the candidate',ci:'waiting for CI',merging:'merging the pull request'}[event.phase] || 'delivery in progress'}.` : ''}`;
        if (!seen.has(text)) add(list, 'li', `${stamp(event.at)} · ${text}`);
        seen.add(text);
      }
      if (data.timeline_truncated) add(timeline, 'p', 'Timeline is limited to the first 100 stored events.', 'hint');
    }
    const diagnostics = disclosure(runNode, 'Diagnostics · technical fields', `diagnostics-${run.id}`);
    for (const [key, label] of [['id','Run/Job ID'],['attempt_id','Current attempt ID'],['base_sha','Pinned base commit'],['candidate_sha','Candidate commit']]) copyField(diagnostics,label,data.diagnostics?.[key]);
    copyField(diagnostics, 'Delivery branch', data.delivery?.branch); copyField(diagnostics, 'Merge commit', data.delivery?.merge_sha);
    for (const attempt of attempts) {
      copyField(diagnostics, `Attempt ${attempt.ordinal} ID`, attempt.id);
      copyField(diagnostics, `Attempt ${attempt.ordinal} candidate`, attempt.candidate_sha);
      for (const evidence of attempt.evidence || []) copyField(diagnostics, 'Evidence ID', evidence.evidence_id);
    }
  }
}
function resumeDeliveryButton(parent, id) {
  const epoch = detailEpoch, currentSession = session, currentToken = token;
  const current = () => token && currentToken === token && currentSession === session && epoch === detailEpoch && byId('detail-modal').open;
  const action = button(parent, 'Resume delivery', async () => {
    if (!current() || resumePending.has(id)) return;
    resumePending.add(id); action.disabled = true;
    byId('detail-notice').textContent = 'Resuming delivery…';
    try {
      await api(`/v1/control/jobs/${encodeURIComponent(id)}/resume-delivery`, {});
      if (!current()) return;
      if (selectedTask) await loadTaskDetail(selectedTask);
      else await loadDetail(id);
    } catch (error) { if (current()) byId('detail-notice').textContent = error.message; }
    finally {
      if (currentSession === session) resumePending.delete(id);
      if (current()) action.disabled = false;
    }
  }, 'primary');
  action.disabled = resumePending.has(id);
}
function renderDetail(data) { renderTaskDetail(null, [data]); }
async function loadTaskDetail(task) {
  const key = taskKey(task), activity = selectedActivity, currentSession = session;
  try {
    const group = await api(`/v1/control/projects/${encodeURIComponent(task.project || '_unassigned')}/runs?source_ref=${encodeURIComponent(task.source_ref)}`);
    const runs = await Promise.all(group.runs.map(async run => {
      try { const data = await api(`/v1/control/jobs/${encodeURIComponent(run.id)}`); data.job.ordinal = run.ordinal; return data; }
      catch { return {job:run,error:true}; }
    }));
    if (!token || session !== currentSession || selectedActivity !== activity || !selectedTask || taskKey(selectedTask) !== key) return;
    const current = boardTasks().find(item => taskKey(item) === key) || task;
    selectedTask = current;
    renderTaskDetail(current, runs, group.total); byId('detail-notice').textContent = '';
  } catch (error) { if (session === currentSession && selectedActivity === activity && selectedTask && taskKey(selectedTask) === key) byId('detail-notice').textContent = error.message; }
}
function openTaskDetail(task) {
  detailEpoch++;
  selectedJob = ''; selectedTask = task;
  selectedActivity = task.issue ? {key:taskKey(task), status:'loading'} : null;
  byId('detail-content').replaceChildren(); renderTaskDetail(task, []);
  byId('detail-notice').textContent = 'Loading linked runs…';
  if (!byId('detail-modal').open) byId('detail-modal').showModal();
  if (selectedActivity) loadIssueActivity(task, selectedActivity);
  loadTaskDetail(task);
}
async function loadDetail(id) {
  const request = ++detailRequest, epoch = detailEpoch, currentSession = session, currentToken = token, job = selectedJob;
  const current = () => request === detailRequest && epoch === detailEpoch && currentSession === session &&
    currentToken === token && token && id === job && job === selectedJob && !selectedTask && byId('detail-modal').open;
  if (!current()) return;
  try {
    const data = await api(`/v1/control/jobs/${encodeURIComponent(id)}`);
    if (!current()) return;
    renderDetail(data); byId('detail-notice').textContent = '';
  } catch (error) { if (current()) byId('detail-notice').textContent = error.message; }
}
function openDetail(id) {
  detailEpoch++;
  selectedJob = id; selectedTask = null; selectedActivity = null;
  byId('detail-title').textContent = 'Run/Job details'; byId('detail-content').replaceChildren();
  byId('detail-notice').textContent = 'Loading run…';
  if (!byId('detail-modal').open) byId('detail-modal').showModal();
  loadDetail(id);
}
function taskProjectInfo() {
  const project = overview.projects.find(p => p.id === byId('task-project').value);
  byId('task-project-info').textContent = project ? `${project.default_branch} · ${project.agent} · Pool ${project.worker_pool} · ${project.delivery ? 'PR delivery available' : 'Candidate only; delivery unavailable'}` : 'No public-source project available.';
}
function openTask() {
  if (!overview || submitPending) return;
  byId('task-form').reset(); byId('task-error').textContent = ''; byId('scoped-checks').hidden = true; byId('task-checks').required = false;
  const select = byId('task-project'); select.replaceChildren();
  for (const project of overview.projects.filter(p => p.public_source)) add(select, 'option', project.id).value = project.id;
  if (overview.projects.some(p => p.id === byId('project-filter').value && p.public_source)) select.value = byId('project-filter').value;
  taskProjectInfo(); byId('task-modal').showModal(); byId('task-title').focus();
}
async function startIssueTask(task) {
  const request = ++startRequest;
  if (submitPending) return;
  if (!overview?.projects.some(p => p.id === task.project && p.public_source)) {
    byId('detail-notice').textContent = 'Public source preparation is unavailable for this project.'; return;
  }
  let brief = task.issue?.body || '';
  if (!brief && task.latest_run?.id) {
    const epoch = detailEpoch, activity = selectedActivity, currentSession = session, currentToken = token, key = taskKey(task);
    const current = () => request === startRequest && epoch === detailEpoch && activity === selectedActivity &&
      currentSession === session && currentToken === token && token && byId('detail-modal').open &&
      selectedTask && taskKey(selectedTask) === key;
    if (!current()) return;
    try { brief = (await api(`/v1/control/jobs/${encodeURIComponent(task.latest_run.id)}`)).instruction; }
    catch (error) { if (current()) byId('detail-notice').textContent = error.message; return; }
    if (!current() || submitPending) return;
  }
  if (byId('detail-modal').open) byId('detail-modal').close();
  openTask();
  byId('task-project').value = task.project; byId('task-title').value = task.title;
  byId('task-instruction').value = brief || task.title;
  byId('task-source').value = task.source_ref;
  taskProjectInfo();
  if (task.issue?.body_truncated) byId('task-error').textContent = 'Issue brief is truncated. Review the complete issue and finish these instructions before starting.';
}
async function submitTask(event) {
  event.preventDefault();
  if (submitPending) return;
  const currentSession = session;
  submitPending = true; byId('submit-task').disabled = true; byId('task-close').disabled = true;
  byId('submit-task').textContent = 'Preparing repository…'; byId('task-error').textContent = '';
  try {
    const input = {delivery_policy: byId('task-delivery-policy').value, project: byId('task-project').value, title: byId('task-title').value, instruction: byId('task-instruction').value, source_ref: byId('task-source').value.trim(), check_preset: byId('task-preset').value, checks: byId('task-preset').value === 'go' ? '' : byId('task-checks').value};
    const job = await api('/v1/control/jobs', input);
    if (currentSession !== session) return;
    byId('task-modal').close();
    await refresh();
    if (currentSession === session && token) {
      if (input.source_ref) {
        const task = boardTasks().find(task => task.source_ref === input.source_ref && task.project === input.project);
        openTaskDetail(task || {project:input.project,source_ref:input.source_ref,title:input.title,latest_run:{...job,project:input.project},run_count:1});
      } else openDetail(job.id);
    }
  } catch (error) { if (currentSession === session) byId('task-error').textContent = error.message; }
  finally {
    submitPending = false; byId('submit-task').disabled = false; byId('task-close').disabled = false;
    byId('submit-task').textContent = 'Start run';
  }
}
function switchView(view) {
  for (const name of ['board', 'workers', 'runs']) {
    const active = view === name;
    byId(`${name}-view`).hidden = !active;
    byId(`${name}-tab`).className = active ? 'nav active' : 'nav';
    if (active) byId(`${name}-tab`).setAttribute('aria-current','page'); else byId(`${name}-tab`).removeAttribute('aria-current');
  }
  byId('view-title').textContent = {board:'Task board',workers:'Workers',runs:'Runs history'}[view];
}
byId('auth-form').addEventListener('submit', event => {
  event.preventDefault();
  if (refreshing) return;
  session++; token = byId('token').value; byId('token').value = ''; refresh();
});
byId('lock').addEventListener('click', () => { lockWorkspace(); byId('token').focus(); });
byId('refresh').addEventListener('click', refresh);
byId('board-tab').addEventListener('click', () => switchView('board'));
byId('workers-tab').addEventListener('click', () => switchView('workers'));
byId('project-filter').addEventListener('change', () => selectProject(byId('project-filter').value));
byId('backlog-refresh').addEventListener('click', () => loadBacklog(true));
byId('runs-tab').addEventListener('click', () => switchView('runs'));
byId('run-status').addEventListener('change', renderRuns);
byId('new-task').addEventListener('click', openTask);
byId('task-project').addEventListener('change', taskProjectInfo);
byId('task-preset').addEventListener('change', () => { byId('scoped-checks').hidden = byId('task-preset').value === 'go'; byId('task-checks').required = !byId('scoped-checks').hidden; });
byId('task-form').addEventListener('submit', submitTask);
byId('task-close').addEventListener('click', () => byId('task-modal').close());
byId('task-modal').addEventListener('cancel', event => { if (submitPending) event.preventDefault(); });
byId('detail-close').addEventListener('click', () => byId('detail-modal').close());
byId('detail-modal').addEventListener('close', () => { detailEpoch++; selectedJob = ''; selectedTask = null; selectedActivity = null; });
document.addEventListener('visibilitychange', () => { if (!document.hidden) refresh(); });
window.addEventListener('online', refresh);
window.addEventListener('offline', () => notice('Offline — displayed data may be stale.', true));
setInterval(() => { if (!document.hidden) refresh(); }, 5000);
renderBoard();
