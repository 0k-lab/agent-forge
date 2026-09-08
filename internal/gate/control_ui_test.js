// Run with: node --test internal/gate/control_ui_test.js
// Minimal DOM double: exercise the embedded script without a browser dependency.
const assert = require('node:assert/strict');
const {test} = require('node:test');
const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');

function workspace() {
  let activeElement;
  class Element {
    constructor(tag = 'div') {
      this.tagName = tag; this.children = []; this.listeners = {}; this.attributes = {};
      this.dataset = {}; this.value = ''; this.hidden = false; this.disabled = false; this.open = false; this.required = false;
    }
    set textContent(text) { this.text = String(text); this.children = []; }
    get textContent() { return (this.text || '') + this.children.map(c => c.textContent).join(''); }
    appendChild(node) { this.children.push(node); return node; }
    replaceChildren() { this.text = ''; this.children = []; }
    setAttribute(name, value) { this.attributes[name] = value; }
    removeAttribute(name) { delete this.attributes[name]; }
    addEventListener(name, action) { this.listeners[name] = action; }
    focus() { this.focused = true; activeElement = this; }
    querySelectorAll(tag) { return this.children.flatMap(node => [...(node.tagName === tag ? [node] : []), ...node.querySelectorAll(tag)]); }
    querySelector(tag) { for (const node of this.children) { if (node.tagName === tag) return node; const found = node.querySelector(tag); if (found) return found; } return null; }
    showModal() { this.open = true; }
    close() { this.open = false; this.listeners.close?.(); }
    reset() { nodes['task-preset'].value = 'go'; }
  }
  const html = readFileSync(path.join(__dirname, 'app/index.html'), 'utf8');
  const nodes = Object.fromEntries([...html.matchAll(/id="([^"]+)"/g)].map(m => [m[1], new Element()]));
  const calls = [];
  const context = vm.createContext({
    document: {get activeElement() { return activeElement; }, getElementById: id => nodes[id], createElement: tag => new Element(tag), addEventListener() {}, hidden: false},
    window: {addEventListener() {}}, URL, Date, console,
    setInterval: (fn, ms) => { assert.equal(ms, 5000); },
    fetch: async (url, options) => { calls.push({url, options}); return {ok: true, status: 200, json: async () => ({projects: [], workers: [], jobs: []})}; }
  });
  vm.runInContext(readFileSync(path.join(__dirname, 'app/app.js'), 'utf8'), context);
  return {nodes, context, calls, run: code => vm.runInContext(code, context)};
}
const project = {id: 'parser', default_branch: 'main', worker_pool: 'coding', agent: 'codex', public_source: true, delivery: true};

test('authoritative board states, text-only titles, filtering and truthful slot capacity', () => {
  const w = workspace();
  const statuses = ['pending', 'retry_wait', 'leased', 'delivering', 'succeeded', 'failed'];
  const jobs = statuses.map((status, i) => ({id: String(i), title: `<img src=x> ${status}`, source_ref: `https://github.com/org/repo/issues/${i + 1}`, status, project: 'parser', created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-01T00:01:00Z'}));
  w.context.data = {projects: [project], jobs, tasks: jobs.map(j => ({source_ref:j.source_ref,project:j.project,title:j.title,latest_run:j,run_count:1})), workers: [
    {id: 'worker', base_id: 'worker', slot: 0, pool: 'coding', connected: true, occupied: true, active_job_id: '2', agent: 'codex'},
    {id: 'worker#1', base_id: 'worker', slot: 1, pool: 'coding', connected: true, occupied: false},
    {id: 'worker#2', base_id: 'worker', slot: 2, pool: 'coding', connected: false, occupied: false}
  ]};
  w.run('renderOverview(data)');
  assert.equal(w.nodes.board.children.length, 6);
  assert.match(w.nodes.board.children[1].textContent, /pending.*retry_wait/);
  for (const [column, status] of [[2,'leased'],[3,'delivering'],[4,'succeeded'],[5,'failed']]) assert.match(w.nodes.board.children[column].textContent, new RegExp(status));
  assert.match(w.nodes.board.textContent, /<img src=x>/);
  assert.match(w.nodes['worker-summary'].textContent, /3 slots · 2 connected · 1 occupied · 1 connected and free/);
  w.nodes['project-filter'].value = 'different'; w.run('renderBoard()');
  assert.doesNotMatch(w.nodes.board.textContent, /<img/);
  w.run(`safeLink(byId('board'), 'javascript:alert(1)', 'unsafe')`);
  assert.doesNotMatch(w.nodes.board.textContent, /unsafe/);
});

test('reopening New task with Go preset clears the hidden scoped-check requirement', () => {
  const w = workspace(); w.context.data = {projects: [project], jobs: [], workers: []};
  w.run('renderOverview(data); openTask()');
  w.nodes['task-preset'].value = '';
  w.nodes['task-preset'].listeners.change();
  assert.equal(w.nodes['task-checks'].required, true);
  w.run('openTask()');
  assert.equal(w.nodes['scoped-checks'].hidden, true);
  assert.equal(w.nodes['task-checks'].required, false);
  assert.equal(w.nodes['task-title'].focused, true);
});

test('submission uses memory auth, no SHA, and allows only one pending request', async () => {
  const w = workspace(); let resolve;
  w.context.fetch = (url, options) => {w.calls.push({url, options}); return new Promise(done => {resolve = done;});};
  w.run(`token = 'owner-token'`);
  for (const [id, value] of Object.entries({'task-project':'parser','task-title':'Fix parser','task-instruction':'Handle Unicode','task-source':'','task-preset':'go'})) w.nodes[id].value = value;
  const first = w.run('submitTask({preventDefault(){}})');
  await w.run('submitTask({preventDefault(){}})');
  assert.equal(w.calls.length, 1);
  assert.equal(w.nodes['submit-task'].disabled, true);
  assert.equal(w.calls[0].options.headers.Authorization, 'Bearer owner-token');
  assert.equal(w.calls[0].options.credentials, 'omit');
  assert.equal(JSON.parse(w.calls[0].options.body).check_preset, 'go');
  assert.equal('base_sha' in JSON.parse(w.calls[0].options.body), false);
  resolve({ok:false,status:422}); await first;
  assert.equal(w.nodes['submit-task'].disabled, false);
  assert.match(w.nodes['task-error'].textContent, /unavailable/);
});

test('unauthorized locks and clears data; a late overview cannot repopulate a locked workspace', async () => {
  const w = workspace();
  w.context.fetch = async () => ({ok:false,status:401});
  w.run(`token = 'wrong'`);
  await w.run('refresh()');
  assert.equal(w.run('token'), '');
  assert.match(w.nodes.notice.textContent, /Unauthorized/);
  assert.equal(w.nodes['new-task'].disabled, true);
  let resolve;
  w.context.fetch = () => new Promise(done => {resolve = done;});
  w.run(`token = 'owner'`);
  const pending = w.run('refresh()');
  w.run('lockWorkspace()');
  resolve({ok:true,status:200,json:async()=>({projects:[project],workers:[],jobs:[]})});
  await pending;
  assert.equal(w.run('overview'), null);
  assert.equal(w.nodes['auth-panel'].hidden, false);
});

test('auto-refresh preserves a focused task card and expanded diagnostics', () => {
  const w = workspace();
  const job = {id: 'task', source_ref: 'https://github.com/org/repo/issues/1', title: 'Fix parser', project: 'parser', status: 'pending', created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-01T00:01:00Z'};
  w.context.data = {projects:[project],jobs:[job],tasks:[{source_ref:job.source_ref,project:job.project,title:job.title,latest_run:job}],workers:[]};
  w.run('renderOverview(data)');
  w.nodes.board.querySelector('button').focus();
  w.run('renderOverview(data)');
  assert.equal(w.context.document.activeElement, w.nodes.board.querySelector('button'));
  w.context.detail = {job, instruction:'Fix the parser', check_count:1, attempts:[],timeline:[],diagnostics:{id:'task'}};
  w.run('renderDetail(detail)');
  w.nodes['detail-content'].querySelector('details').open = true;
  w.run('renderDetail(detail)');
  assert.equal(w.nodes['detail-content'].querySelector('details').open, true);
});

test('six task lanes exclude historical jobs; Runs history filters project and status', () => {
 const w=workspace();
 w.context.data={projects:[project],tasks:[],workers:[],jobs:[{id:'old',title:'Historical Forge job',status:'succeeded',project:'parser'}]};
 w.run('renderOverview(data)');
 assert.equal(w.nodes.board.children.length,6);
 assert.doesNotMatch(w.nodes.board.textContent,/Historical Forge job/);
 assert.match(w.nodes.runs.textContent,/Run\/Job.*Historical Forge job/);
 w.nodes['run-status'].value='failed';w.run('renderRuns()');assert.doesNotMatch(w.nodes.runs.textContent,/Historical Forge job/);
 w.nodes['run-status'].value='';w.nodes['project-filter'].value='other';w.run('renderRuns()');assert.doesNotMatch(w.nodes.runs.textContent,/Historical Forge job/);
 w.context.data.tasks=[{project:'parser',source_ref:'https://github.com/org/repo/issues/1',title:'Product backlog',issue:{number:1,state:'open',labels:[]}}, {project:'parser',source_ref:'https://github.com/org/repo/issues/2',title:'Scheduled issue',issue:{number:2,state:'open',labels:['ready-for-agent']}}];
 w.run('renderOverview(data)');
 assert.match(w.nodes.board.children[0].textContent,/Product backlog/);
 assert.match(w.nodes.board.children[1].textContent,/Scheduled issue/);
});

test('unavailable GitHub backlog keeps linked local tasks and runs truthful', async () => {
 const w=workspace();
 w.context.data={projects:[project],tasks:[{project:'parser',source_ref:'https://github.com/org/repo/issues/1',title:'Linked work',latest_run:{id:'run',status:'leased'}}],workers:[],jobs:[]};
 w.run("token='owner';renderOverview(data)");w.nodes['project-filter'].value='parser';
 w.context.fetch=async()=>({ok:false,status:502});
 await w.run('loadBacklog(true)');
 assert.match(w.nodes['backlog-notice'].textContent,/unavailable/i);
 assert.match(w.nodes.board.children[2].textContent,/Linked work/);
});

test('task drawer groups runs and attempts with collapsed brief and labeled diagnostics', () => {
 const w=workspace();
 const task={project:'parser',source_ref:'https://github.com/org/repo/issues/12',title:'Fix parser',issue:{number:12,body:'A long public issue brief',state:'open',labels:[]},latest_run:{status:'retry_wait'}};
 const run={job:{id:'job',title:'Fix parser',ordinal:1,project:'parser',status:'retry_wait',agent:'codex',worker_id:'slot-1',plugin_timeout_ms:900000},instruction:'Full execution instructions',check_count:1,diagnostics:{id:'job',base_sha:'abcdef'},timeline:[{type:'submitted',at:'2026-09-01T00:00:00Z'},{type:'leased'},{type:'retry_scheduled'}],attempts:[{id:'attempt',ordinal:1,status:'failed',failure_code:'execution_failed',failure_disposition:'retryable',worker_id:'slot-1',leased_at:'2026-09-01T00:00:00Z',completed_at:'2026-09-01T00:15:00Z',evidence:[{reason:'plugin_protocol_failed',duration_ms:900000}]}]};
 w.context.task=task;w.context.details=[run];w.run('renderTaskDetail(task,details)');
 const root=w.nodes['detail-content'];
 assert.match(root.textContent,/Issue #12/);assert.match(root.textContent,/Ready/);
 assert.match(root.textContent,/Run 1/);assert.match(root.textContent,/Attempt 1/);
 assert.match(root.textContent,/Coding agent timed out after 15 min/);
 assert.match(root.textContent,/Forge scheduled another attempt/);
 function walk(node){return [node,...node.children.flatMap(walk)];}
 const disclosures=walk(root).filter(n=>n.tagName==='details');
 assert.ok(disclosures.some(n=>n.children[0].textContent==='Full brief'&&!n.open));
 assert.ok(disclosures.some(n=>n.children[0].textContent.startsWith('Diagnostics')&&!n.open));
 assert.ok(walk(root).some(n=>n.tagName==='button'&&n.textContent==='Copy'));
 assert.equal(walk(root).filter(n=>n.tagName==='pre').length,0);
 assert.doesNotMatch(root.textContent,/"base_sha"|retry_scheduled|· leased/);
});

test('attempt outcomes are deterministic, bounded by reported timing, and do not invent retries', () => {
 const w=workspace();
 assert.equal(w.run("attemptOutcome({status:'failed',failure_code:'execution_failed',failure_disposition:'retryable',evidence:[]},{status:'retry_wait'})"),'Coding agent did not finish; Forge scheduled another attempt.');
 assert.equal(w.run("evidenceSummary({reason:'scoped_check_passed',check_index:0,duration_ms:21000},{})"),'Check 1 passed in 21s.');
 assert.equal(w.run("evidenceSummary({reason:'plugin_protocol_failed',duration_ms:900000},{plugin_timeout_ms:900000})"),'Coding agent timed out after 15 min.');
 assert.doesNotMatch(w.run("evidenceSummary({reason:'plugin_protocol_failed',duration_ms:900000},{plugin_timeout_ms:3600000})"),/timed out/);
 assert.equal(w.run("attemptOutcome({status:'succeeded',evidence:[{reason:'cleanup_failed'}]},{status:'succeeded'})"),'Work completed, but temporary workspace cleanup needs attention.');
 assert.doesNotMatch(w.run("attemptOutcome({status:'failed',failure_code:'execution_failed',failure_disposition:'retryable',evidence:[]},{status:'failed'})"),/scheduled another/);
});

test('starting a backlog issue prefills its exact URL, title and brief without a SHA', async () => {
 const w=workspace();w.context.data={projects:[project],tasks:[],workers:[],jobs:[]};
 w.run('renderOverview(data)');
 w.context.task={project:'parser',title:'Fix Unicode parser',source_ref:'https://github.com/org/repo/issues/12',issue:{number:12,body:'Keep Unicode intact.\nAdd a focused check.',state:'open',labels:[]}};
 await w.run('startIssueTask(task)');
 assert.equal(w.nodes['task-project'].value,'parser');
 assert.equal(w.nodes['task-title'].value,'Fix Unicode parser');
 assert.equal(w.nodes['task-instruction'].value,'Keep Unicode intact.\nAdd a focused check.');
 assert.equal(w.nodes['task-source'].value,'https://github.com/org/repo/issues/12');
 assert.equal(w.nodes['task-modal'].open,true);
 assert.equal(w.calls.length,0);
});

test('drawer humanizes run and delivery states, and marks a truncated issue brief', () => {
 const w=workspace();
 w.context.detail={job:{id:'run',title:'Fix',status:'retry_wait',delivery:{phase:'retry_wait',ci_state:'pending'}},instruction:'brief',attempts:[],timeline:[],diagnostics:{}};
 w.run('renderDetail(detail)');
 assert.doesNotMatch(w.nodes['detail-content'].textContent,/retry_wait/);
 w.context.task={title:'Fix',project:'parser',issue:{number:1,state:'open',body:'excerpt',body_truncated:true}};
 w.run('renderTaskDetail(task,[])');
 assert.match(w.nodes['detail-content'].textContent,/brief is truncated/i);
});

test('successful submission opens the submitted task even if fields change while pending', async () => {
 const w=workspace();let resolve;
 w.run("token='owner'; refresh=async()=>{}; openTaskDetail=task=>{globalThis.opened=task}; openDetail=id=>{globalThis.opened={id}}");
 w.context.fetch=()=>new Promise(done=>{resolve=done});
 for(const [id,value] of Object.entries({'task-project':'parser','task-title':'Original task','task-instruction':'Original brief','task-source':'https://github.com/org/repo/issues/12','task-preset':'go'}))w.nodes[id].value=value;
 const pending=w.run('submitTask({preventDefault(){}})');
 w.nodes['task-source'].value='https://github.com/org/repo/issues/99';w.nodes['task-project'].value='changed';
 resolve({ok:true,status:201,json:async()=>({id:'new-run',status:'pending'})});await pending;
 assert.equal(w.context.opened.source_ref,'https://github.com/org/repo/issues/12');
 assert.equal(w.context.opened.project,'parser');assert.equal(w.context.opened.title,'Original task');
});

test('All projects fetches public feeds with at most three requests, dedupes and shares navigation state', async () => {
 const w=workspace();const projects=['alpha','beta','gamma','delta'].map(id=>({...project,id}));
 const task=id=>({project:id,source_ref:`https://github.com/org/${id}/issues/1`,title:`Issue ${id}`,issue:{number:1,state:'open',labels:[]}});
 w.context.data={projects:[...projects,{...project,id:'private',public_source:false}],tasks:[task('alpha')],jobs:[{id:'run',project:'alpha',title:'Alpha run',status:'pending'}],workers:[{id:'worker',pool:'coding',connected:true,occupied:false}]};
 w.run("token='owner';renderOverview(data)");
 let active=0,peak=0;const calls=[];
 w.context.fetch=async url=>{const id=url.split('/')[4];calls.push(id);active++;peak=Math.max(peak,active);await new Promise(resolve=>setImmediate(resolve));active--;return {ok:true,status:200,json:async()=>({tasks:[task(id),task(id)],available:true})};};
 await w.run('loadBacklog()');
 assert.deepEqual(calls.sort(),['alpha','beta','delta','gamma']);assert.ok(peak<=3);assert.equal(peak,3);
 assert.equal(w.run('boardTasks().length'),4);
 for(const id of calls)assert.match(w.nodes.board.textContent,new RegExp(`Issue ${id}`));
 const alpha=w.nodes['project-nav'].children.find(node=>node.dataset.project==='alpha');
 await alpha.listeners.click();
 assert.equal(w.nodes['project-filter'].value,'alpha');assert.match(w.nodes.board.textContent,/Issue alpha/);assert.doesNotMatch(w.nodes.board.textContent,/Issue beta/);
 assert.match(w.nodes['project-info'].textContent,/Branch/);assert.match(w.nodes['backlog-notice'].textContent,/alpha/);
 assert.equal(calls.length,4);
 w.nodes['project-filter'].value='';await w.nodes['project-filter'].listeners.change();
 assert.match(w.nodes.board.textContent,/Issue beta/);
 assert.equal(w.nodes['project-nav'].children[0].attributes['aria-current'],'true');
 await w.run('loadBacklog(true)');assert.equal(calls.length,8);
});

test('All projects keeps successful feeds on partial failure and fences old selections and locked sessions', async () => {
 const w=workspace();w.context.data={projects:[{...project,id:'alpha'},{...project,id:'beta'}],tasks:[],jobs:[],workers:[]};
 w.run("token='owner';renderOverview(data)");
 w.context.fetch=async url=>url.includes('/beta/')?{ok:false,status:502}:{ok:true,status:200,json:async()=>({tasks:[{project:'alpha',source_ref:'https://github.com/org/repo/issues/1',title:'Alpha issue',issue:{state:'open',labels:[]}}]})};
 await w.run('loadBacklog()');
 assert.match(w.nodes.board.textContent,/Alpha issue/);assert.match(w.nodes['backlog-notice'].textContent,/unavailable.*beta/i);assert.ok(w.nodes['backlog-notice'].textContent.length<600);
 const pendingResponses=[];w.context.fetch=()=>new Promise(done=>{pendingResponses.push(done)});
 const pending=w.run('loadBacklog(true)');await new Promise(done=>setImmediate(done));
 w.run('lockWorkspace()');for(const resolve of pendingResponses)resolve({ok:true,status:200,json:async()=>({tasks:[{title:'Stale issue'}]})});await pending;
 assert.doesNotMatch(w.nodes.board.textContent,/Stale|Alpha issue/);assert.equal(w.run('backlogs.size'),0);
});

test('closed issue without a run shows GitHub completion, closure and lazy merged PR evidence without Start', async () => {
 const w=workspace();
 w.context.task={project:'parser',source_ref:'https://github.com/org/repo/issues/53',title:'Completed task',issue:{number:53,state:'closed',labels:[],closed_at:'2026-08-29T20:44:18Z',closed_by:'kricha-lab-dev-worker[bot]',body:'Full brief'}};
 w.context.data={projects:[project],tasks:[w.context.task],jobs:[],workers:[]};
 const calls=[];
 w.context.fetch=async(url,options)=>{calls.push(url);assert.equal(options.headers.Authorization,'Bearer owner');return {ok:true,status:200,json:async()=>url.endsWith('/activity')?{available:true,merged_prs:[{number:55,url:'https://github.com/org/repo/pull/55',merged_at:'2026-08-29T20:44:17Z'}]}:{runs:[],total:0}}};
 w.run("token='owner';renderOverview(data)");assert.equal(calls.length,0);
 w.run('openTaskDetail(task)');await new Promise(done=>setImmediate(done));
 const root=w.nodes['detail-content'];
 assert.match(root.textContent,/Completed on GitHub\. No Forge execution record is linked; this task predates tracking or was completed outside Forge\./);
 assert.match(root.textContent,/kricha-lab-dev-worker\[bot\]/);assert.match(root.textContent,/2026/);
 assert.doesNotMatch(root.textContent,/Review the brief and start|No run started/);
 assert.ok(root.querySelectorAll('a').some(a=>a.href==='https://github.com/org/repo/pull/55'&&a.textContent.includes('#55')));
 assert.ok(!root.querySelectorAll('button').some(b=>b.textContent.includes('Start')));
 assert.equal(root.querySelector('details').open,false);
 await w.run('loadTaskDetail(task)');assert.equal(calls.filter(url=>url.endsWith('/activity')).length,1);
});

test('activity failure retains closed metadata and readable notice; open issues retain Start', async () => {
 const w=workspace();w.context.task={project:'parser',source_ref:'https://github.com/org/repo/issues/53',title:'Completed',issue:{number:53,state:'closed',closed_by:'worker[bot]',closed_at:'2026-08-29T20:44:18Z'}};
 w.run("token='owner'");
 w.context.fetch=async url=>url.endsWith('/activity')?{ok:false,status:502}:{ok:true,status:200,json:async()=>({runs:[],total:0})};
 w.run('openTaskDetail(task)');await new Promise(done=>setImmediate(done));
 assert.match(w.nodes['detail-content'].textContent,/Additional GitHub activity unavailable/);
 assert.match(w.nodes['detail-content'].textContent,/Completed on GitHub.*worker\[bot\]/);
 w.run("task.issue.state='open';renderTaskDetail(task,[])");
 assert.ok(w.nodes['detail-content'].querySelectorAll('button').some(b=>b.textContent==='Start run'));
});

test('backlog cache expires after five minutes and late All projects responses cannot change a selected project', async () => {
 const w=workspace();w.context.data={projects:[{...project,id:'alpha'},{...project,id:'beta'}],tasks:[],jobs:[],workers:[]};
 w.run("token='owner';renderOverview(data)");
 const task=id=>({project:id,source_ref:`https://github.com/org/${id}/issues/1`,title:`Issue ${id}`,issue:{number:1,state:'open',labels:[]}});
 const response=id=>({ok:true,status:200,json:async()=>({tasks:[task(id)]})});
 const pending=[];w.context.fetch=url=>new Promise(resolve=>pending.push({id:url.split('/')[4],resolve}));
 const all=w.run('loadBacklog()');await new Promise(done=>setImmediate(done));
 const single=w.run("selectProject('beta')");
 for(const item of pending)item.resolve(response(item.id));
 w.context.fetch=async()=>response('beta');await Promise.all([all,single]);
 assert.equal(w.nodes['project-filter'].value,'beta');assert.doesNotMatch(w.nodes.board.textContent,/Issue alpha/);assert.match(w.nodes.board.textContent,/Issue beta/);
 assert.equal(w.run("backlogs.has('alpha')"),false);
 w.nodes.board.querySelector('button').focus();
 let calls=0;w.context.fetch=async()=>{calls++;return response('beta')};
 await w.run('loadBacklog()');assert.equal(calls,0);
 w.run("backlogs.get('beta').checkedAt=Date.now()-300001");
 await w.run('loadBacklog()');assert.equal(calls,1);
 assert.equal(w.context.document.activeElement,w.nodes.board.querySelector('button'));
});

test('late issue activity cannot repaint another drawer or a locked workspace', async () => {
 const w=workspace();w.context.task={project:'parser',source_ref:'https://github.com/org/repo/issues/53',title:'Completed',issue:{number:53,state:'closed'}};
 w.run("token='owner'");const pending=[];
 w.context.fetch=url=>url.endsWith('/activity')?new Promise(resolve=>pending.push(resolve)):Promise.resolve({ok:true,status:200,json:async()=>({runs:[],total:0})});
 w.run('openTaskDetail(task)');await new Promise(done=>setImmediate(done));
 w.nodes['detail-modal'].close();
 w.run('openTaskDetail(task)');await new Promise(done=>setImmediate(done));
 pending[0]({ok:true,status:200,json:async()=>({available:true,merged_prs:[{number:99,url:'https://github.com/org/repo/pull/99'}]})});
 await new Promise(done=>setImmediate(done));assert.doesNotMatch(w.nodes['detail-content'].textContent,/#99/);
 w.run('lockWorkspace()');pending[1]({ok:true,status:200,json:async()=>({available:true,merged_prs:[{number:55,url:'https://github.com/org/repo/pull/55'}]})});
 await new Promise(done=>setImmediate(done));assert.equal(w.nodes['detail-content'].textContent,'');
});

test('Delivered changes explains fixture files before attempts and is GitHub evidence, not agent activity', async () => {
 const w=workspace();w.context.task={project:'parser',source_ref:'https://github.com/org/repo/issues/53',title:'Completed',issue:{number:53,state:'closed'}};
 w.run("token='owner'");
 w.context.fetch=async url=>({ok:true,status:200,json:async()=>url.endsWith('/activity')?{available:true,merged_prs:[{number:55,url:'https://github.com/org/repo/pull/55',merged_at:'2026-08-29T20:44:17Z',details:{delivery_evidence:true,title:'Preserve Unicode',summary:'Normalize input so equivalent names match. <img src=x>',changed_files:2,additions:18,deletions:3,files:[{filename:'parser/normalize.go',status:'modified',additions:12,deletions:3},{filename:'parser/normalize_test.go',status:'added',additions:6,deletions:0}]}}]}:{runs:[],total:0}});
 w.run('openTaskDetail(task)');await new Promise(done=>setImmediate(done));
 const root=w.nodes['detail-content'],text=root.textContent;
 assert.match(text,/Delivered changes.*GitHub delivery evidence.*Preserve Unicode.*equivalent names match/);
 assert.match(text,/2 files.*18.*3/);assert.match(text,/parser\/normalize.go.*parser\/normalize_test.go/);
 assert.ok(text.indexOf('Delivered changes')<text.indexOf('Runs ·'));
 assert.doesNotMatch(text,/Agent activity|Detailed agent report/);assert.equal(root.querySelectorAll('img').length,0);
});

test('Agent report is self-reported, text-only, attempt-bound and before Forge verification', () => {
 const w=workspace();
 const report={summary:'Normalize equivalent names <img src=x>',changes:['Compare normalized input <script>bad()</script>']};
 const attempt={id:'attempt2',ordinal:2,status:'succeeded',candidate_sha:'abc',agent_report:report,evidence:[{reason:'scoped_check_passed',check_index:0,duration_ms:21000}]};
 w.context.detail={job:{id:'run',ordinal:1,status:'succeeded',agent:'codex'},agent_report:report,instruction:'Brief',attempts:[{id:'attempt1',ordinal:1,status:'failed',failure_code:'execution_failed',evidence:[]},attempt],timeline:[],diagnostics:{}};
 w.run('renderDetail(detail)');
 const root=w.nodes['detail-content'],text=root.textContent;
 assert.match(text,/Agent report.*Self-reported.*Attempt 2.*Normalize equivalent names/);
 assert.ok(text.indexOf('Agent report')<text.indexOf('Forge verification'));
 assert.ok(text.indexOf('Forge verification')<text.indexOf('Attempt 1'));
 assert.match(text,/Check 1 passed in 21s/);
 assert.equal(root.querySelectorAll('img').length,0);assert.equal(root.querySelectorAll('script').length,0);
 assert.equal((text.match(/Normalize equivalent names/g)||[]).length,1);
 w.context.detail.attempts=[];delete w.context.detail.agent_report;
 w.run('renderDetail(detail)');assert.match(root.textContent,/Detailed agent report was not captured for this run/);
 w.run("renderTaskDetail({title:'Closed',issue:{state:'closed'}},[])");assert.doesNotMatch(root.textContent,/Detailed agent report|Agent report/);
});

 test('related cross-references never imply delivery, even with closing-looking summary', () => {
 const w=workspace();
 for (const proof of [undefined,false,'true']) {
 w.context.activity={data:{merged_prs:[{number:55,url:'https://github.com/org/repo/pull/55',merged_at:'2026-08-29T20:44:17Z',details:{delivery_evidence:proof,title:'Related work',summary:'Closes #53',files:[],changed_files:0,additions:0,deletions:0}}]}};
 w.run("renderIssueCompletion(byId('detail-content'),{state:'closed'},activity)");
 assert.match(w.nodes['detail-content'].textContent,/Related merged PR.*Related GitHub evidence.*Related work/);
 assert.doesNotMatch(w.nodes['detail-content'].textContent,/Delivered changes|GitHub delivery evidence/);
 }
 });

for (const change of ['selection','close','lock','overlap','reopen','token','normal','error']) {
 test(`latest-run prefill fences ${change}`, async () => {
 const w=workspace();
 w.context.task={project:'parser',source_ref:'https://github.com/org/repo/issues/53',title:'Old issue',issue:{number:53,state:'open',body:''},latest_run:{id:'old',status:'failed'}};
 w.context.other={...w.context.task,title:'Current issue',source_ref:'https://github.com/org/repo/issues/54',issue:{number:54,state:'open',body:'New brief'},latest_run:null};
 w.context.data={projects:[project],tasks:[w.context.task,w.context.other],jobs:[],workers:[]};
 w.run("token='owner';renderOverview(data)");
 w.context.fetch=async url=>({ok:true,status:200,json:async()=>url.endsWith('/activity')?{available:true,merged_prs:[]}:{runs:[],total:0}});
 w.run('openTaskDetail(task)');await new Promise(done=>setImmediate(done));
 const pending=[];w.context.fetch=()=>new Promise(resolve=>pending.push(resolve));
 const first=w.run('startIssueTask(task)');
 if(change==='selection'||change==='error') w.run('openTaskDetail(other)');
 if(change==='close') w.nodes['detail-modal'].close();
 if(change==='reopen') {w.nodes['detail-modal'].close();w.run('openTaskDetail(task)');}
 if(change==='lock') w.run('lockWorkspace()');
 if(change==='token') w.run("token='replacement'");
 let second;
 if(change==='overlap') second=w.run('startIssueTask(task)');
 const before={drawer:w.nodes['detail-modal'].open,modal:w.nodes['task-modal'].open,title:w.nodes['task-title'].value,brief:w.nodes['task-instruction'].value,notice:w.nodes['detail-notice'].textContent};
 pending[0]({ok:change!=='error',status:change==='error'?502:200,json:async()=>({instruction:'Old fetched brief'})});
 await first;
 if(change==='normal') {
 assert.equal(w.nodes['task-modal'].open,true);assert.equal(w.nodes['detail-modal'].open,false);
 assert.equal(w.nodes['task-instruction'].value,'Old fetched brief');assert.equal(w.nodes['task-source'].value,w.context.task.source_ref);
 } else {
 assert.deepEqual({drawer:w.nodes['detail-modal'].open,modal:w.nodes['task-modal'].open,title:w.nodes['task-title'].value,brief:w.nodes['task-instruction'].value,notice:w.nodes['detail-notice'].textContent},before);
 }
 if(second) {pending[1]({ok:true,status:200,json:async()=>({instruction:'Newest brief'})});await second;assert.equal(w.nodes['task-instruction'].value,'Newest brief');}
 });
}

function deferred() {
  let resolve, reject;
  const promise = new Promise((done, fail) => { resolve = done; reject = fail; });
  return {promise, resolve, reject};
}

for (const phase of ['fetch', 'json']) {
  for (const outcome of ['success', 'failure']) {
    for (const change of ['session', 'lock', 'close', 'different job', 'reopen', 'overlap', 'token', 'normal']) {
      test(`loadDetail fences ${phase} ${outcome} after ${change}`, async () => {
        const w = workspace(), waiting = deferred(), decoding = deferred();
        const detail = title => ({job:{id:'job',title,status:'pending'},instruction:title,attempts:[],timeline:[]});
        const response = title => ({ok:true,status:200,json:async()=>detail(title)});
        // Observe completion while exercising the real openDetail/loadDetail DOM path.
        w.run("token='owner'; const realLoadDetail=loadDetail; loadDetail=id=>(globalThis.loading=realLoadDetail(id))");
        w.context.fetch = () => phase === 'fetch' ? waiting.promise : Promise.resolve({
          ok:true,status:200,json:()=>{decoding.resolve();return waiting.promise;}
        });
        w.run("openDetail('job')");
        const old = w.context.loading;
        if (phase === 'json') await decoding.promise;
        w.context.fetch = async () => response('Current drawer');
        if (change === 'session') {
          w.run("lockWorkspace(); session++; token='new-owner'; openDetail('job')");
        }
        if (change === 'lock') w.run('lockWorkspace()');
        if (change === 'close') w.nodes['detail-modal'].close();
        if (change === 'different job') w.run("openDetail('other')");
        if (change === 'reopen') { w.nodes['detail-modal'].close(); w.run("openDetail('job')"); }
        if (change === 'overlap') w.run("loadDetail('job')");
        if (change === 'token') w.run("token='replacement'");
        if (w.context.loading !== old) await w.context.loading;
        w.nodes['detail-notice'].textContent = 'Current notice';
        const snapshot = () => ({content:w.nodes['detail-content'].textContent,
          children:[...w.nodes['detail-content'].children],notice:w.nodes['detail-notice'].textContent,
          open:w.nodes['detail-modal'].open,selection:w.run('selectedJob'),title:w.nodes['detail-title'].textContent});
        const before = snapshot();
        if (phase === 'fetch') waiting.resolve(outcome === 'success' ? response('Old drawer') : {ok:false,status:404});
        else if (outcome === 'success') waiting.resolve(detail('Old drawer'));
        else waiting.reject(new Error('Body decoding failed'));
        await old;
        if (change !== 'normal') assert.deepEqual(snapshot(), before);
        else if (outcome === 'success') {
          assert.match(w.nodes['detail-content'].textContent, /Old drawer/);
          assert.equal(w.nodes['detail-notice'].textContent, '');
        } else {
          assert.equal(w.nodes['detail-content'].textContent, before.content);
          assert.equal(w.nodes['detail-notice'].textContent, phase === 'fetch' ? 'Task not found.' : 'Body decoding failed');
        }
      });
    }
  }
}

test('api rejects prior-session data after delayed JSON decoding', async () => {
  const w = workspace(), body = deferred(), decoding = deferred();
  w.run("token='owner'");
  w.context.fetch = async () => ({ok:true,status:200,json:()=>{decoding.resolve();return body.promise;}});
  const pending = w.run("api('/v1/control/jobs/job')");
  const rejected = assert.rejects(pending, /Workspace session changed\./);
  await decoding.promise;
  w.run("lockWorkspace(); session++; token='new-owner'");
  body.resolve({secret:'old session data'});
  await rejected;
});

for (const phase of ['fetch', 'json']) {
  test(`submission does not render a prior-session sentinel after delayed ${phase}`, async () => {
    const w = workspace(), waiting = deferred(), decoding = deferred();
    w.run("token='owner'");
    w.context.fetch = () => phase === 'fetch' ? waiting.promise : Promise.resolve({
      ok:true,status:201,json:()=>{decoding.resolve();return waiting.promise;}
    });
    const pending = w.run('submitTask({preventDefault(){}})');
    if (phase === 'json') await decoding.promise;
    w.run("lockWorkspace(); session++; token='new-owner'");
    w.nodes['task-error'].textContent = 'Current task notice';
    w.context.fetch = async () => ({ok:true,status:200,json:async()=>({projects:[],workers:[],jobs:[]})});
    waiting.resolve(phase === 'fetch' ? {ok:true,status:201,json:async()=>({id:'old'})} : {id:'old'});
    await pending;
    assert.equal(w.nodes['task-error'].textContent, 'Current task notice');
    assert.equal(w.nodes['detail-modal'].open, false);
  });
}

test('task submission explicitly offers automatic and review delivery', async () => {
  const w = workspace();
  const html = readFileSync(path.join(__dirname, 'app/index.html'), 'utf8');
  assert.match(html, /<label for="task-delivery-policy">Delivery policy<\/label>/);
  assert.match(html, /<option value="automatic"[^>]*>Automatic/);
  assert.match(html, /<option value="review">Review/);
  w.run(`token = 'owner'`);
  w.nodes['task-delivery-policy'].value = 'review';
  w.context.fetch = async (url, options) => { w.calls.push({url, options}); return {ok:false,status:422}; };
  await w.run('submitTask({preventDefault(){}})');
  assert.equal(JSON.parse(w.calls[0].options.body).delivery_policy, 'review');
});


test('held run stays in Review & CI and resumes once with owner auth', async () => {
 const w=workspace();
 const delivery={phase:'awaiting_review'};
 const job={id:'held',title:'Review candidate',project:'parser',status:'delivering',delivery,source_ref:'https://github.com/org/repo/issues/89'};
 w.context.data={projects:[project],workers:[],jobs:[job],tasks:[{project:'parser',title:job.title,source_ref:job.source_ref,latest_run:job}]};
 w.context.detail={job,delivery,attempts:[],timeline:[],diagnostics:{id:job.id}};
 w.run(`token='owner'; selectedJob='held'; renderOverview(data); byId('detail-modal').showModal(); renderDetail(detail)`);
 assert.match(w.nodes.board.children[3].textContent,/Awaiting independent review/);
 const action=()=>w.nodes['detail-content'].querySelectorAll('button').find(b=>b.textContent==='Resume delivery');
 assert.ok(action());
 let resolve;
 w.context.fetch=(url,options)=>{w.calls.push({url,options});return new Promise(done=>{resolve=done;});};
 const first=action().listeners.click();
 assert.equal(action().disabled,true);
 w.run('renderDetail(detail)');
 assert.equal(action().disabled,true);
 await action().listeners.click();
 assert.equal(w.calls.length,1);
 assert.equal(w.calls[0].url,'/v1/control/jobs/held/resume-delivery');
 assert.equal(w.calls[0].options.headers.Authorization,'Bearer owner');
 assert.equal(w.calls[0].options.method,'POST');
 w.context.fetch=async()=>({ok:true,status:200,json:async()=>({...w.context.detail,job:{...job,delivery:{phase:'pending'}},delivery:{phase:'pending'}})});
 resolve({ok:true,status:200,json:async()=>({phase:'pending'})});
 await first;
 assert.equal(action(),undefined);
 assert.match(w.nodes['detail-content'].textContent,/Waiting to publish/);
});

test('resume callbacks and late responses cannot cross drawer or owner sessions', async () => {
 for (const mode of ['navigation','lock','reauth','late-401']) {
  const w=workspace();
  w.context.detail={job:{id:'held',title:'Candidate',status:'delivering',delivery:{phase:'awaiting_review'}},delivery:{phase:'awaiting_review'},attempts:[],timeline:[],diagnostics:{}};
  w.run(`token='owner'; selectedJob='held'; byId('detail-modal').showModal(); renderDetail(detail)`);
  const action=w.nodes['detail-content'].querySelectorAll('button').find(b=>b.textContent==='Resume delivery');
  assert.ok(action);
  let resolve;
  w.context.fetch=(url,options)=>{w.calls.push({url,options});return new Promise(done=>{resolve=done;});};
  const pending=action.listeners.click();
  if(mode==='navigation') w.run(`detailEpoch++; selectedJob='other'; byId('detail-content').textContent='Other run';`);
  else w.run(`lockWorkspace(); session++; token='new-owner'; byId('detail-content').textContent='New session';`);
  w.nodes['detail-notice'].textContent='Keep this notice';
  resolve({ok:mode!=='late-401',status:mode==='late-401'?401:200,json:async()=>({phase:'pending'})});
  await pending;
  assert.equal(w.calls.length,1);
  assert.equal(w.nodes['detail-notice'].textContent,'Keep this notice');
  assert.equal(w.nodes['detail-content'].textContent,mode==='navigation'?'Other run':'New session');
  assert.equal(w.run('token'),mode==='navigation'?'owner':'new-owner');
  await action.listeners.click();
  assert.equal(w.calls.length,1,'stale button must not send using new credentials');
 }
});


test('resume button is bound to the owner token that rendered it', async () => {
 const w=workspace();
 w.context.detail={job:{id:'held',status:'delivering'},delivery:{phase:'awaiting_review'},attempts:[],timeline:[],diagnostics:{}};
 w.run(`token='owner'; selectedJob='held'; byId('detail-modal').showModal(); renderDetail(detail)`);
 const action=w.nodes['detail-content'].querySelectorAll('button').find(b=>b.textContent==='Resume delivery');
 w.run(`token='replacement-owner'`);
 w.context.fetch=async(url,options)=>{ w.calls.push({url,options}); return {ok:false,status:409}; };
 await action.listeners.click();
 assert.equal(w.calls.length,0);
});
