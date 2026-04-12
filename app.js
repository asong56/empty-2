// app.js — TardiTalk frontend (vanilla JS, no deps).
'use strict';

const CONFIG = {
  api: 'http://localhost:8888',
  ws:  'ws://localhost:8888/ws',
  cacheDays: 3,
  pageSize: 40,
  // WS backoff: starts at 1s, doubles each retry, caps at 30s, ±1s jitter
  wsBackoffBase: 1000,
  wsBackoffMax:  30000,
};

const State = {
  threads:        [],
  activeThreadId: null,
  activeThread:   null,
  messages:       [],
  contacts:       [],
  filter:         'all',
  searchQuery:    '',
  settings: {
    pinnedOnly:       true,
    suppressStickers: true,
    stripTrackers:    true,
    htmlToMarkdown:   true,
  },
  ws:          null,
  wsRetryTimer: null,
  wsRetryCount: 0,
  isMobile:    window.innerWidth <= 640,
};

// ── DOM refs
const $ = id => document.getElementById(id);
const DOM = {
  sidebar:          $('sidebar'),
  threadList:       $('thread-list'),
  searchInput:      $('search-input'),
  connDot:          $('conn-status-dot'),
  connLabel:        $('conn-status-label'),
  platformHealth:   $('platform-health'),
  chatEmpty:        $('chat-empty'),
  chatActive:       $('chat-active'),
  chatAvatar:       $('chat-avatar'),
  chatName:         $('chat-name'),
  chatMeta:         $('chat-meta'),
  messageFeed:      $('message-feed'),
  composeInput:     $('compose-input'),
  btnSend:          $('btn-send'),
  composeProto:     $('compose-proto-label'),
  contactsList:     $('contact-list'),
  contactsPanel:    $('contacts-panel'),
  contactsSearch:   $('contacts-search'),
  settingsPanel:    $('settings-panel'),
  overlay:          $('overlay'),
  btnCompose:       $('btn-compose'),
  btnContacts:      $('btn-contacts'),
  btnSettings:      $('btn-settings'),
  btnContactsClose: $('btn-contacts-close'),
  btnSettingsClose: $('btn-settings-close'),
  btnChatBack:      $('btn-chat-back'),
  btnExport:        $('btn-export'),
};

// ── Utilities
function esc(str) {
  return String(str)
    .replace(/&/g,'&amp;').replace(/</g,'&lt;')
    .replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

function relTime(iso) {
  const d = new Date(iso); if (isNaN(d)) return '';
  const diff = Date.now() - d.getTime();
  const m = Math.floor(diff/60000), h = Math.floor(diff/3600000), dy = Math.floor(diff/86400000);
  if (m < 1) return 'now'; if (m < 60) return m+'m'; if (h < 24) return h+'h';
  if (dy < 7) return dy+'d';
  return d.toLocaleDateString(undefined,{month:'short',day:'numeric'});
}

function fullTime(iso) {
  const d = new Date(iso); if (isNaN(d)) return '';
  return d.toLocaleTimeString(undefined,{hour:'2-digit',minute:'2-digit'});
}

function initials(name) {
  return name.split(/\s+/).map(w=>w[0]||'').join('').toUpperCase().slice(0,2)||'?';
}

const PROTO_LABELS = {wechat:'WeChat',whatsapp:'WhatsApp',signal:'Signal',email:'Mail',sms:'SMS',internal:'Ghost'};

const STATUS_GLYPH = {pending:'○',sent:'◦',delivered:'◎',read:'●',failed:'✕'};

const PROTO_RECEIPT = {wechat:'unsupported',whatsapp:'full',signal:'full',email:'unsupported',sms:'sent_only',internal:'full'};

function receiptGlyph(protocol, status) {
  const s = PROTO_RECEIPT[protocol]||'full';
  if (s==='unsupported') return '◦';
  if (s==='sent_only' && status==='read') return '◎';
  return STATUS_GLYPH[status]||'○';
}

function renderMarkdown(text) {
  let h = esc(text);
  h = h.replace(/```([\s\S]*?)```/g,'<pre><code>$1</code></pre>');
  h = h.replace(/`([^`]+)`/g,'<code>$1</code>');
  h = h.replace(/\*\*(.+?)\*\*/g,'<strong>$1</strong>');
  h = h.replace(/_(.+?)_/g,'<em>$1</em>');
  h = h.replace(/~~(.+?)~~/g,'<del>$1</del>');
  h = h.replace(/\[([^\]]+)\]\((https?:\/\/[^)]+)\)/g,'<a href="$2" rel="noopener noreferrer" target="_blank">$1</a>');
  h = h.replace(/^&gt;\s(.+)/gm,'<blockquote>$1</blockquote>');
  h = h.replace(/\n/g,'<br>');
  return h;
}

// ── WebSocket (exponential backoff)
function connectWS() {
  if (State.ws && State.ws.readyState <= 1) return;
  setConnStatus('connecting');
  const ws = new WebSocket(CONFIG.ws);
  State.ws = ws;

  ws.onopen = () => {
    State.wsRetryCount = 0;
    setConnStatus('connected');
  };
  ws.onmessage = e => {
    try { handlePushEvent(JSON.parse(e.data)); } catch(_) {}
  };
  ws.onclose = ws.onerror = () => {
    State.ws = null;
    setConnStatus('disconnected');
    scheduleReconnect();
  };
}

function scheduleReconnect() {
  if (State.wsRetryTimer) return;
  const delay = Math.min(
    CONFIG.wsBackoffBase * Math.pow(2, State.wsRetryCount),
    CONFIG.wsBackoffMax
  ) + Math.random() * 1000;
  State.wsRetryCount++;
  State.wsRetryTimer = setTimeout(() => {
    State.wsRetryTimer = null;
    connectWS();
  }, delay);
}

function setConnStatus(status) {
  const dot = DOM.connDot, label = DOM.connLabel;
  dot.className = 'conn-dot ' + status;
  label.textContent = status === 'connected' ? 'Connected'
    : status === 'connecting' ? 'Connecting…' : 'Reconnecting…';
}

function handlePushEvent(evt) {
  switch (evt.type) {
    case 'new_message': {
      const msg = evt.payload;
      if (!msg) return;
      updateThreadPreview(msg);
      if (msg.thread_id === State.activeThreadId) {
        State.messages.push(msg);
        appendMessageBubble(msg);
        scrollFeedBottom();
      }
      break;
    }
    case 'message_retract': {
      const p = evt.payload;
      if (!p) return;
      const el = document.querySelector(`[data-msg-id="${esc(p.message_id)}"]`);
      if (el) {
        const bubble = el.querySelector('.bubble');
        if (bubble) {
          bubble.classList.add('retracted');
          bubble.textContent = p.is_self ? `[Deleted: ${p.retracted_body||''}]` : '[Message deleted]';
        }
      }
      break;
    }
    case 'platform_health': {
      const p = evt.payload;
      if (!p) return;
      updatePlatformBadge(p);
      break;
    }
  }
}

// ── Thread List
async function loadThreads() {
  try { const d = await (await fetch(`${CONFIG.api}/api/threads?proto=${State.filter}`)).json(); State.threads = d.threads||[]; renderThreadList(); }
  catch(e) { console.error('loadThreads',e); }
}

function renderThreadList() {
  const q = State.searchQuery.toLowerCase();
  const threads = State.threads.filter(t =>
    !q || t.display_name.toLowerCase().includes(q) ||
    (t.last_message?.snippet||'').toLowerCase().includes(q)
  );

  DOM.threadList.innerHTML = threads.map(t => {
    const lm = t.last_message;
    return `<div class="thread-item${t.id===State.activeThreadId?' active':''}" role="listitem"
      data-thread-id="${esc(t.id)}" tabindex="0">
      <div class="avatar">${esc(initials(t.display_name))}</div>
      <div class="thread-info">
        <div class="thread-top">
          <span class="thread-name">${esc(t.display_name)}</span>
          <span class="thread-time">${lm ? relTime(lm.timestamp) : ''}</span>
        </div>
        <div class="thread-snippet">
          <span class="proto-dot ${esc(t.protocol)}"></span>
          ${lm ? esc(lm.snippet||'') : ''}
        </div>
      </div>
      ${t.unread_count > 0 ? `<div class="thread-badge">${t.unread_count}</div>` : ''}
    </div>`;
  }).join('');

  DOM.threadList.querySelectorAll('.thread-item').forEach(el => {
    el.addEventListener('click', () => openThread(el.dataset.threadId));
    el.addEventListener('keydown', e => { if (e.key==='Enter'||e.key===' ') openThread(el.dataset.threadId); });
  });
}

function updateThreadPreview(msg) {
  const t = State.threads.find(x=>x.id===msg.thread_id);
  if (!t) return;
  t.last_message = {sender_name: msg.sender_id==='self'?'You':msg.sender_id, snippet: msg.body||'[media]', timestamp: msg.sent_at};
  if (msg.thread_id !== State.activeThreadId) t.unread_count = (t.unread_count||0)+1;
  renderThreadList();
}

// ── Chat View
async function openThread(id) {
  State.activeThreadId = id;
  State.activeThread = State.threads.find(t=>t.id===id)||{id,display_name:'Chat',protocol:'internal'};
  const t = State.activeThread;
  t.unread_count = 0;

  DOM.chatEmpty.style.display = 'none';
  DOM.chatActive.style.display = 'flex';
  DOM.chatAvatar.textContent = initials(t.display_name);
  DOM.chatName.textContent = t.display_name;
  DOM.chatMeta.textContent = PROTO_LABELS[t.protocol]||t.protocol;
  DOM.composeProto.textContent = PROTO_LABELS[t.protocol]||t.protocol;

  if (State.isMobile) {
    DOM.sidebar.classList.remove('open');
  }
  renderThreadList();

  try {
    const r = await fetch(`${CONFIG.api}/api/threads/${encodeURIComponent(id)}/messages`);
    const data = await r.json();
    State.messages = data.messages||[];
    renderMessages();
  } catch(e) { console.error('openThread', e); }
}

function renderMessages() {
  DOM.messageFeed.innerHTML = '';
  let lastDate = '';
  State.messages.forEach(msg => {
    const d = new Date(msg.sent_at);
    const ds = isNaN(d) ? '' : d.toLocaleDateString(undefined,{weekday:'short',month:'short',day:'numeric'});
    if (ds !== lastDate) {
      lastDate = ds;
      const div = Object.assign(document.createElement('div'),{className:'day-divider',innerHTML:`<span>${esc(ds)}</span>`});
      DOM.messageFeed.appendChild(div);
    }
    DOM.messageFeed.appendChild(buildMessageEl(msg));
  });
  scrollFeedBottom();
}

function buildMessageEl(msg) {
  const row = document.createElement('div');
  row.className = 'msg-row ' + (msg.is_self ? 'self' : 'other');
  row.dataset.msgId = msg.id;
  let bodyHTML = '';
  if (msg.is_retracted) {
    bodyHTML = `<div class="bubble retracted">[Message deleted]</div>`;
  } else if (msg.content_type === 'redirected' && msg.redirected) {
    const r = msg.redirected;
    bodyHTML = `<div class="bubble redirected">${esc(r.display_label||r.original_type)}${r.deep_link?` <a href="${esc(r.deep_link)}" target="_blank" rel="noopener">Open</a>`:''}</div>`;
  } else {
    let md = renderMarkdown(msg.reply_snippet && msg.reply_to_id
      ? `<div class="quote-block">${esc(msg.reply_snippet)}</div>` + (msg.body||'')
      : (msg.body||''));
    bodyHTML = msg.is_self ? `<div class="bubble">${md}</div>`
      : `<div class="sender-name">${esc(msg.sender_id)}</div><div class="bubble">${md}</div>`;
  }
  const g = receiptGlyph(msg.protocol, msg.status);
  const glyph = msg.is_self ? `<span class="receipt-glyph" title="${esc(g)}">${g}</span>` : '';
  row.innerHTML = bodyHTML + `<div class="msg-footer"><span>${esc(fullTime(msg.sent_at))}</span>${glyph}</div>`;
  return row;
}

function appendMessageBubble(msg) {
  DOM.messageFeed.appendChild(buildMessageEl(msg));
}

function scrollFeedBottom() {
  DOM.messageFeed.scrollTop = DOM.messageFeed.scrollHeight;
}

// ── Compose
async function sendMessage() {
  if (!State.activeThreadId) return;
  const body = DOM.composeInput.value.trim();
  if (!body) return;
  DOM.composeInput.value = ''; DOM.btnSend.disabled = true; DOM.composeInput.style.height = '';
  try {
    const r = await fetch(`${CONFIG.api}/api/messages/send`,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({thread_id:State.activeThreadId,protocol:State.activeThread?.protocol||'wechat',body,content_type:'text'})});
    if (!r.ok) throw new Error(await r.text());
  } catch(e) { console.error('sendMessage',e); }
}

// ── Contacts
async function loadContacts() {
  try { const d = await (await fetch(`${CONFIG.api}/api/contacts`)).json(); State.contacts = d.contacts||[]; renderContacts(); }
  catch(e) {}
}

function renderContacts(q = '') {
  const contacts = q ? State.contacts.filter(c => c.display_name.toLowerCase().includes(q.toLowerCase())) : State.contacts;
  DOM.contactsList.innerHTML = contacts.map(c => {
    const handles = Object.entries(c.handles||{}).map(([p,h]) => `<span class="handle-tag">${esc(PROTO_LABELS[p]||p)}: ${esc(h)}</span>`).join('');
    return `<div class="contact-item" role="listitem"><div class="avatar">${esc(initials(c.display_name))}</div><div><div style="font-size:.9rem;font-weight:600">${esc(c.display_name)}</div><div class="contact-handles">${handles}</div></div></div>`;
  }).join('');
}

// ── Platform health badges
function updatePlatformBadge(p) {
  const label = PROTO_LABELS[p.protocol]||p.protocol;
  const existing = DOM.platformHealth.querySelector(`[data-proto="${p.protocol}"]`);
  if (existing) {
    existing.className = `ph-badge ${p.status}`;
    existing.title = label; existing.textContent = label;
  } else {
    const span = document.createElement('span');
    Object.assign(span, {className:`ph-badge ${p.status}`,title:label,textContent:label});
    span.dataset.proto = p.protocol;
    DOM.platformHealth.appendChild(span);
  }
}

// ── Settings toggles
function initToggles() {
  [
    ['toggle-pinned-only','pinnedOnly'],['toggle-suppress-stickers','suppressStickers'],
    ['toggle-strip-trackers','stripTrackers'],['toggle-html-md','htmlToMarkdown'],
  ].forEach(([id,key]) => {
    const el = $(id); if (!el) return;
    el.classList.toggle('on', State.settings[key]);
    el.setAttribute('aria-pressed', State.settings[key]);
    el.addEventListener('click', () => {
      const v = State.settings[key] = !State.settings[key];
      el.classList.toggle('on', v); el.setAttribute('aria-pressed', v);
    });
  });
}

// ── Panel helpers
function openPanel(panel) {
  [panel, DOM.overlay].forEach(el => { el.classList.add('open'); el.setAttribute('aria-hidden','false'); });
}
function closeAllPanels() {
  [DOM.contactsPanel, DOM.settingsPanel, DOM.overlay].forEach(el => {
    el.classList.remove('open'); el.setAttribute('aria-hidden','true');
  });
}

// ── Search
let searchTimer = null;
DOM.searchInput.addEventListener('input', () => {
  clearTimeout(searchTimer);
  State.searchQuery = DOM.searchInput.value.trim();
  searchTimer = setTimeout(() => {
    if (State.searchQuery) {
      fetch(`${CONFIG.api}/api/search?q=${encodeURIComponent(State.searchQuery)}`)
        .then(r=>r.json()).then(d => {
          // Highlight matched threads
          renderThreadList();
        }).catch(()=>{});
    } else {
      renderThreadList();
    }
  }, 250);
});

// ── Event bindings
DOM.btnContacts.addEventListener('click', () => { loadContacts(); openPanel(DOM.contactsPanel); });
DOM.btnSettings.addEventListener('click', () => openPanel(DOM.settingsPanel));
DOM.btnContactsClose.addEventListener('click', closeAllPanels);
DOM.btnSettingsClose.addEventListener('click', closeAllPanels);
DOM.overlay.addEventListener('click', closeAllPanels);

DOM.btnChatBack.addEventListener('click', () => {
  DOM.sidebar.classList.toggle('open', true);
  DOM.chatActive.style.display = 'none';
  DOM.chatEmpty.style.display = 'flex';
});

DOM.composeInput.addEventListener('input', () => {
  DOM.btnSend.disabled = !DOM.composeInput.value.trim();
  // Auto-resize textarea
  DOM.composeInput.style.height = '';
  DOM.composeInput.style.height = Math.min(DOM.composeInput.scrollHeight, 120) + 'px';
});

DOM.composeInput.addEventListener('keydown', e => {
  if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); sendMessage(); }
});
DOM.btnSend.addEventListener('click', sendMessage);

// Filter buttons
document.querySelectorAll('.filter-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.filter-btn').forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
    State.filter = btn.dataset.filter;
    loadThreads();
  });
});

DOM.btnExport?.addEventListener('click', () => {
  if (!State.activeThread) return;
  const data = JSON.stringify({thread: State.activeThread, messages: State.messages}, null, 2);
  const a = document.createElement('a');
  a.href = 'data:application/json,' + encodeURIComponent(data);
  a.download = `tarditalk-${State.activeThreadId}-export.json`;
  a.click();
});

DOM.contactsSearch.addEventListener('input', e => renderContacts(e.target.value));

// Mobile sidebar
DOM.btnCompose?.addEventListener('click', () => {
  DOM.sidebar.classList.toggle('open');
});

// ── Boot
window.addEventListener('resize', () => {
  State.isMobile = window.innerWidth <= 640;
});

initToggles();
loadThreads();
connectW
