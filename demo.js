/* ---- Demo: an agent driving termcp over MCP on one shell -------------
   Every line is a real tool or a real mark from the product:
     a = an MCP call the agent made (the point of the page)
     o = shell output
     i = the human's keystrokes
     q/A = a review decision (no bytes of its own)
   Nothing here calls a model or a server; the transcript is scripted. */
const termScreen = document.getElementById('term-screen');
const termForm = document.getElementById('term-form');
const termInput = document.getElementById('term-input');
const termHint = document.getElementById('term-hint');
const toast = document.getElementById('term-toast');
const toastBody = document.getElementById('term-toast-body');
const reviewLock = document.getElementById('review-lock');

let reviewOn = true;      // review mode: the agent's writes are held
let waiting = false;      // the agent is stopped, waiting on the human
let mode = 'ai';          // 'ai' = hand the task over; 'human' = type it yourself
let queue = [];           // the running task's remaining steps
let lastTask = 'ssh';     // what the replay key re-runs
let auto = false;         // true while playing the whole task without waiting

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function line(mark, text, cls) {
  const p = el('p', 'tl' + (cls ? ' ' + cls : ''));
  p.appendChild(el('span', 'mk mk-' + mark, mark));
  p.appendChild(document.createTextNode(text));
  termScreen.appendChild(p);
  termScreen.scrollTop = termScreen.scrollHeight;
  return p;
}

/* The tasks a visitor hands to the agent. `say` is the human's sentence in
   plain words; the steps are what the agent then decides to do with the
   terminal — that gap IS the product. Code lines stay untranslated. */
const TASKS = {
  ssh: {
    say: 'say_ssh',
    steps: [
      { mark: 'a', code: 'term(cmd="connect edge-web")' },
      { mark: 'o', key: 'd_creds' },
      { mark: 'o', code: '→ a1f3:1 · pty 120x32' },
      { mark: 'a', code: 'term(cmd="run a1f3:1 tail -n 3 /var/log/nginx/error.log")' },
      { mark: 'o', code: '[error] connect() failed (111: Connection refused) while connecting to upstream, upstream: "http://127.0.0.1:3000"' },
      { mark: 'a', code: 'term(cmd="run a1f3:1 ls -la /etc/nginx/conf.d")' },
      { mark: 'o', code: 'proxy.conf   482 B   -rw-r--r--' },
      { mark: 'a', code: 'term(cmd="open a1f3 forward --local-port 8080 --remote-host 127.0.0.1 --remote-port 3000")' },
      { mark: 'o', code: '127.0.0.1:8080 → web-01:3000 · tunnel up' }
    ]
  },
  port: {
    say: 'say_port',
    steps: [
      { mark: 'a', code: 'term(cmd="connect internal")' },
      { mark: 'o', code: '→ b7c2:1 · pty 80x24' },
      { mark: 'a', code: 'term(cmd="run b7c2:1 grep -n proxy_pass /etc/nginx/nginx.conf")' },
      { mark: 'o', code: '14:    proxy_pass http://127.0.0.1:3000;' },
      { mark: 'a', code: 'term(cmd="run b7c2:1 sed -i s/:3000/:8080/ /etc/nginx/conf.d/proxy.conf")' },
      { gate: true },
      { mark: 'o', key: 'd_written' },
      { mark: 'a', code: 'term(cmd="run b7c2:1 sudo nginx -s reload")' },
      { mark: 'o', code: '[sudo] password for ops:' },
      { mark: 'a', code: 'term(cmd="notify --level warn --sticky --on b7c2 --title \'Need the sudo password\' nginx -s reload is waiting")' },
      { toast: true },
      { human: true },
      { mark: 'o', code: 'nginx: configuration file test is successful · reloaded' },
      { mark: 'a', code: 'term(cmd="open b7c2 forward --local-port 8080 --remote-host 127.0.0.1 --remote-port 3000")' },
      { mark: 'o', code: '127.0.0.1:8080 → 127.0.0.1:3000 · tunnel up' }
    ]
  },
  disk: {
    say: 'say_disk',
    steps: [
      { mark: 'a', code: 'term(cmd="connect internal")' },
      { mark: 'a', code: 'term(cmd="run c9d1:1 df -h /")' },
      { mark: 'o', code: 'Filesystem      Size  Used Avail Use% Mounted on' },
      { mark: 'o', code: '/dev/vda1        40G   31G  7.2G  82% /' },
      { mark: 'a', code: 'term(cmd="ls")' },
      { mark: 'o', code: '→ c9d1:1 pty running · c9d1:2 pipe running' },
      { mark: 'a', code: 'term(cmd="notify --level warn --on c9d1 --title \'/ is 82% full\' /var/log is taking 3.4G")' },
      { toast: true },
      { mark: 'o', key: 'd_disk' }
    ]
  }
};

/* A canned shell for when the human takes the keyboard. Small on purpose:
   the point is that the line lands on the terminal the agent reads. */
const FAKE = {
  pwd: '/srv/app',
  whoami: 'ops',
  'uname -a': 'Linux edge-01 6.6.0 x86_64 GNU/Linux',
  'cat /etc/hostname': 'edge-01',
  'nginx -t': 'configuration file /etc/nginx/nginx.conf test is successful',
  'df -h /': '/dev/vda1  40G  31G  7.2G  82% /'
};

const GATE_CMD = 'sed -i s/:3000/:8080/ /etc/nginx/conf.d/proxy.conf';

function setHint(text) {
  termHint.textContent = text;
  termHint.classList.toggle('armed', waiting);
}

function defaultHint() {
  if (waiting) setHint(t('input_hint_wait'));
  else setHint(t(mode === 'ai' ? 'input_hint' : 'input_hint_human'));
}

function setMode(next) {
  mode = next;
  document.getElementById('mode-ai').classList.toggle('active', mode === 'ai');
  document.getElementById('mode-human').classList.toggle('active', mode === 'human');
  termInput.placeholder = t(mode === 'ai' ? 'input_placeholder_ai' : 'input_placeholder_human');
  document.querySelector('.term-send').textContent = t(mode === 'ai' ? 'input_send_ai' : 'input_send_human');
  defaultHint();
}

function showToast(msg) {
  toastBody.textContent = msg;
  toast.hidden = false;
}

function clearGate() {
  const g = document.getElementById('gate');
  if (g) g.remove();
}

/* The review gate: nothing here is editable — the AI wrote the command, the
   human accepts or refuses it. Either way this step resolves. */
function renderGate() {
  const box = el('div', 'gate');
  box.id = 'gate';
  box.appendChild(el('div', 'gate-h', t('gate_h')));
  box.appendChild(el('code', 'gate-cmd', GATE_CMD));
  const actions = el('div', 'gate-actions');
  const accept = el('button', 'gate-accept', t('gate_accept'));
  accept.type = 'button';
  const reject = el('button', 'gate-reject', t('gate_reject'));
  reject.type = 'button';
  accept.addEventListener('click', () => decide(true));
  reject.addEventListener('click', () => decide(false));
  actions.appendChild(accept);
  actions.appendChild(reject);
  box.appendChild(actions);
  termScreen.appendChild(box);
  termScreen.scrollTop = termScreen.scrollHeight;
}

function decide(accepted) {
  clearGate();
  line('q', 'A  ' + (accepted ? t('gate_accepted') : t('gate_rejected')), 'tl-review');
  /* Rejected: the write never happened, so drop the step that only exists
     because the file was changed. */
  if (!accepted) queue = queue.filter(s => s.key !== 'd_written');
  pump();
}

/* Drain the running task until it needs a human: a review decision, a secret,
   or the end. Everything the agent does on its own happens here.

   `auto` is the autoplay used on load and on replay: the page shows a
   finished run rather than a transcript parked at a gate. The human's two
   moments are still written down — the decision and the secret — so the
   transcript tells the same story, just already told. */
function pump() {
  for (;;) {
    const step = queue.shift();
    if (!step) { waiting = false; defaultHint(); return; }
    if (step.gate) {
      if (auto) {
        line('q', 'A  ' + t('gate_accepted'), 'tl-review');
        continue;
      }
      if (!reviewOn) continue;              // review off: the write just lands
      renderGate();
      return;
    }
    if (step.toast) { showToast(t('toast_body')); continue; }
    if (step.human) {
      if (auto) {
        line('i', '$ ********', 'tl-human');
        line('o', t('d_sudo_ok'), null);
        continue;
      }
      waiting = true;
      defaultHint();
      return;
    }
    const text = step.code !== undefined ? step.code : t(step.key);
    if (step.mark === 'a') {
      const p = line('a', '', 'tl-ai');
      p.appendChild(el('span', 'tool', text));
    } else {
      line(step.mark, text, null);
    }
  }
}

/* Hand a task over: the human says it in plain words, the agent works out
   the commands. This is the headline capability. */
function runTask(id, opts) {
  const task = TASKS[id];
  if (!task) return;
  lastTask = id;
  auto = !!(opts && opts.auto);
  termScreen.textContent = '';
  toast.hidden = true;
  clearGate();
  waiting = false;
  line('i', '$ ' + t(task.say), 'tl-human');
  queue = task.steps.slice();
  defaultHint();
  pump();
}

document.querySelectorAll('.task-chip').forEach(chip => {
  /* Clicked by a visitor: the gate stays visible, because watching the agent
     get held is the product. Only the initial render and the replay key run
     the whole thing through, so the page never sits parked at a gate. */
  chip.addEventListener('click', () => runTask(chip.getAttribute('data-task')));
});
document.getElementById('mode-ai').addEventListener('click', () => setMode('ai'));
document.getElementById('mode-human').addEventListener('click', () => setMode('human'));

reviewLock.addEventListener('click', () => {
  reviewOn = !reviewOn;
  reviewLock.setAttribute('aria-pressed', String(reviewOn));
  reviewLock.textContent = t(reviewOn ? 'review_on' : 'review_off');
});

termForm.addEventListener('submit', (e) => {
  e.preventDefault();
  const text = termInput.value.trim();
  if (!text) return;
  termInput.value = '';

  /* The agent is stopped at a prompt it must not answer: the secret is the
     human's, and the product keeps it out of the model's context. */
  if (waiting) {
    line('i', '$ ********', 'tl-human');
    waiting = false;
    toast.hidden = true;
    defaultHint();
    line('o', t('d_sudo_ok'), null);
    pump();
    return;
  }

  if (mode === 'ai') {
    /* Same thing the task chips do, but with the visitor's own words. */
    line('i', '$ ' + text, 'tl-human');
    queue = [
      { mark: 'a', code: 'term(cmd="connect internal")' },
      { mark: 'a', code: 'term(cmd="run c9d1:1 " + JSON.stringify(text))' },
      { mark: 'o', code: FAKE[text] !== undefined ? FAKE[text] : 'bash: ' + text + ': command not found' },
      { mark: 'a', code: 'term(cmd="read c9d1:1")' },
      { mark: 'o', key: 'd_read' }
    ];
    pump();
    return;
  }

  /* Human mode: your line goes to the shell yourself — and the agent, which
     is on the same shell, reads it back. That is the co-driving claim. */
  line('i', '$ ' + text, 'tl-human');
  line('o', FAKE[text] !== undefined ? FAKE[text] : 'bash: ' + text + ': command not found', null);
  const p = line('a', '', 'tl-ai');
  p.appendChild(el('span', 'tool', 'term(cmd="read c9d1:1")'));
  line('o', t('agent_sees'), null);
});

document.getElementById('term-replay').addEventListener('click', () => runTask(lastTask, { auto: true }));
