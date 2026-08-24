(() => {
  'use strict';

  let token = '';
  const byId = id => document.getElementById(id);
  const notice = byId('notice');

  async function get(path) {
    const response = await fetch(path, {headers: {Authorization: `Bearer ${token}`}});
    if (!response.ok) throw new Error(`request failed (${response.status})`);
    return response.json();
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function addText(parent, tag, value) {
    const node = document.createElement(tag);
    node.textContent = value;
    parent.appendChild(node);
    return node;
  }

  function showJob(job) {
    const details = byId('job');
    clear(details);
    for (const [label, value] of [
      ['ID', job.id], ['Kind', job.kind], ['Status', job.status],
      ['Attempt', job.attempt_id || '—'], ['Worker', job.worker_id || '—'],
      ['Base SHA', job.base_sha || '—'], ['Candidate SHA', job.candidate_sha || '—'],
      ['Created', job.created_at], ['Updated', job.updated_at]
    ]) {
      addText(details, 'dt', label);
      addText(details, 'dd', value);
    }
  }

  async function loadTimeline(id) {
    const data = await get(`/v1/debug/jobs/${encodeURIComponent(id)}?limit=100`);
    showJob(data.job);
    const list = byId('timeline');
    clear(list);
    for (const event of data.events) addText(list, 'li', `${event.at} — ${event.type}`);
  }

  async function loadAll() {
    notice.textContent = 'Loading…';
    try {
      const [jobs, workers] = await Promise.all([get('/v1/debug/jobs?limit=100'), get('/v1/debug/workers?limit=100')]);
      const jobList = byId('jobs');
      clear(jobList);
      for (const job of jobs.items) {
        const item = document.createElement('li');
        const button = addText(item, 'button', `${job.status} · ${job.kind} · ${job.id}`);
        button.type = 'button';
        button.addEventListener('click', () => loadTimeline(job.id).catch(showFailure));
        jobList.appendChild(item);
      }
      const workerList = byId('workers');
      clear(workerList);
      for (const worker of workers.items) addText(workerList, 'li', `${worker.connected ? 'connected' : 'disconnected'} · ${worker.id} · ${worker.last_seen}`);
      if (jobs.items.length) await loadTimeline(jobs.items[0].id);
      notice.textContent = 'Loaded';
    } catch (error) {
      showFailure(error);
    }
  }

  function showFailure(error) {
    notice.textContent = error.message;
  }

  byId('connect').addEventListener('click', () => {
    const field = byId('token');
    token = field.value;
    field.value = '';
    loadAll();
  });
})();
