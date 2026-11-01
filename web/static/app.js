const $ = (id) => document.getElementById(id);
const roomId = location.pathname.split('/').pop();
const api = (path, body) => fetch(`/api/rooms/${roomId}/${path}`, {
  method: 'POST',
  headers: body ? { 'Content-Type': 'application/json' } : {},
  body: body ? JSON.stringify(body) : undefined,
});

const RANKS = ['', 'A', '2', '3', '4', '5', '6', '7', '8', '9', '10', 'J', 'Q', 'K'];
const SUITS = ['♠', '♥', '♦', '♣'];
const PER_REGION = 13;
const REGIONS = 4;

let me = -1;
let snap = null;
let wasMyTurn = false;

function show(name) {
  for (const v of ['message', 'wait', 'game']) $(`view-${v}`).hidden = v !== name;
}

function message(text) {
  $('message').textContent = text;
  show('message');
}

// ---------------------------------------------------------------- board

const slots = [];
(function buildBoard() {
  const board = $('board');
  for (let i = 0; i < 14; i++) {
    const slot = document.createElement('div');
    slot.className = 'slot';
    if (i < PER_REGION) {
      slot.classList.add('card');
      const inner = document.createElement('div');
      inner.className = 'inner';
      const back = document.createElement('div');
      back.className = 'back';
      const face = document.createElement('div');
      face.className = 'face';
      inner.append(back, face);
      slot.append(inner);
      slot.addEventListener('click', () => onCard(i));
    }
    board.append(slot);
    slots.push(slot);
  }
})();

function render() {
  if (!snap) return;
  const myTurn = snap.phase === 'playing' && snap.turn === me;
  const base = snap.region * PER_REGION;
  for (let i = 0; i < PER_REGION; i++) {
    const c = snap.cards[base + i];
    const slot = slots[i];
    slot.classList.toggle('up', c.state === 'up');
    slot.classList.toggle('gone', c.state === 'gone');
    const face = slot.querySelector('.face');
    if (c.state === 'up') {
      face.textContent = `${RANKS[c.rank]}${SUITS[c.suit]}`;
      face.classList.toggle('red', c.suit === 1 || c.suit === 2);
    }
  }
  $('s-me').textContent = snap.scores[me];
  $('s-op').textContent = snap.scores[1 - me];
  $('p-me').classList.toggle('active', myTurn);
  $('p-op').classList.toggle('active', snap.phase === 'playing' && snap.turn !== me);
  document.body.classList.toggle('my-turn', myTurn);
  $('prev').disabled = !myTurn || snap.region === 0;
  $('next').disabled = !myTurn || snap.region === REGIONS - 1;
  const dots = $('dots');
  dots.replaceChildren();
  for (let r = 0; r < REGIONS; r++) {
    const d = document.createElement('span');
    d.className = r === snap.region ? 'dot on' : 'dot';
    dots.append(d);
  }
  if (myTurn && !wasMyTurn && navigator.vibrate) navigator.vibrate(60);
  wasMyTurn = myTurn;

  if (snap.phase === 'finished') {
    const a = snap.scores[me], b = snap.scores[1 - me];
    $('result').textContent = a > b ? '勝ち' : a < b ? '負け' : '引き分け';
    $('result-score').textContent = `${a} – ${b}`;
    $('view-result').hidden = false;
  } else {
    $('view-result').hidden = true;
  }
}

async function onCard(i) {
  if (!snap || snap.phase !== 'playing' || snap.turn !== me || snap.pending) return;
  const idx = snap.region * PER_REGION + i;
  if (snap.cards[idx].state !== 'down') return;
  await api('flip', { index: idx });
}

async function move(delta) {
  if (!snap || snap.phase !== 'playing' || snap.turn !== me) return;
  const region = snap.region + delta;
  if (region < 0 || region >= REGIONS) return;
  await api('region', { region });
}

$('prev').addEventListener('click', () => move(-1));
$('next').addEventListener('click', () => move(1));
$('rematch').addEventListener('click', () => api('rematch'));

// Swipe left/right on the board moves region (active player only).
let touchX = null;
const stage = document.querySelector('.stage');
stage.addEventListener('touchstart', (e) => { touchX = e.touches[0].clientX; }, { passive: true });
stage.addEventListener('touchend', (e) => {
  if (touchX === null) return;
  const dx = e.changedTouches[0].clientX - touchX;
  touchX = null;
  if (Math.abs(dx) > 60) move(dx < 0 ? 1 : -1);
}, { passive: true });

// ---------------------------------------------------------------- wait

const url = location.origin + location.pathname;
$('room-url').textContent = url;
$('copy').addEventListener('click', async () => {
  try { await navigator.clipboard.writeText(url); $('copy').textContent = 'コピーした'; } catch { /* ignore */ }
});
if (navigator.share) {
  $('share').hidden = false;
  $('share').addEventListener('click', () => navigator.share({ url }).catch(() => {}));
}

// ---------------------------------------------------------------- stream

function connect() {
  const es = new EventSource(`/api/rooms/${roomId}/events`);
  es.onmessage = (ev) => {
    snap = JSON.parse(ev.data);
    document.body.classList.remove('offline');
    if (snap.phase === 'waiting') { show('wait'); return; }
    show('game');
    render();
  };
  es.onerror = () => { document.body.classList.add('offline'); };
}

(async function join() {
  try {
    const res = await api('join');
    if (res.status === 404) return message('部屋がありません');
    if (res.status === 409) return message('満席');
    if (!res.ok) return message(`エラー ${res.status}`);
    me = (await res.json()).seat;
    connect();
  } catch (e) {
    message('接続できません');
  }
})();
