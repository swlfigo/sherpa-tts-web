"use strict";

// The page talks to the JSON API of the server. Generated audio comes back
// once and then lives only here, as blob URLs, until the page is closed.

const $ = (id) => document.getElementById(id);
const els = {
  pill: $("model-pill"),
  notice: $("model-notice"),
  noticeText: $("model-notice-text"),
  noticeMeter: $("model-meter"),
  noticeFill: $("model-meter-fill"),
  noticeDetail: $("model-notice-detail"),
  form: $("composer"),
  text: $("text"),
  count: $("count"),
  voice: $("voice"),
  preview: $("preview"),
  speed: $("speed"),
  speedOut: $("speed-out"),
  generate: $("generate"),
  shortcut: $("shortcut"),
  job: $("job"),
  jobText: $("job-text"),
  jobFill: $("job-meter-fill"),
  cancel: $("cancel"),
  error: $("error"),
  results: $("results"),
  takes: $("takes"),
  template: $("take-template"),
  version: $("version"),
};

const state = {
  maxChars: 5000,
  modelReady: false,
  voices: new Map(), // name -> voice
  job: null, // {id, voice, timer}
  takes: [],
  previews: new Map(), // "voice@speed" -> blob URL
  previewAudio: null,
  previewKey: "",
  previewBusy: false,
};

const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

// ---- API ----

async function api(path, options = {}) {
  let res;
  try {
    res = await fetch(path, { credentials: "same-origin", ...options });
  } catch {
    throw new Error("连接不上服务器，请检查网络后重试。");
  }
  if (res.status === 401) {
    throw Object.assign(new Error("需要重新登录，请刷新页面后输入密码。"), { status: 401 });
  }
  if (!res.ok) {
    let message = `服务器返回错误 ${res.status}`;
    try {
      const body = await res.json();
      if (body.error) message = body.error;
    } catch {}
    throw Object.assign(new Error(message), { status: res.status });
  }
  return res;
}

function postJSON(path, body) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

// ---- Formatting ----

function clock(seconds) {
  const s = Math.max(0, Math.round(seconds || 0));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

function roughly(seconds) {
  if (seconds >= 90) return `约 ${Math.round(seconds / 60)} 分钟`;
  return `约 ${Math.max(1, Math.round(seconds))} 秒`;
}

const mb = (bytes) => (bytes / 1e6).toFixed(1);

function voiceLabel(name) {
  const v = state.voices.get(name);
  return v ? `${v.group} ${v.display}` : name;
}

// ---- Model status ----

async function refreshStatus() {
  let delay = 1000;
  try {
    const s = await (await api("/api/status")).json();
    state.maxChars = s.maxChars || state.maxChars;
    if (s.version) els.version.textContent = ` 版本 ${s.version}`;
    renderModel(s.model);
    if (s.model.state === "ready") delay = 0;
    if (s.model.state === "failed") delay = 15000;
  } catch (err) {
    renderModel({ state: "offline", error: err.message });
    delay = 3000;
  }
  updateCount();
  if (delay) setTimeout(refreshStatus, delay);
}

function renderModel(m) {
  state.modelReady = m.state === "ready";
  els.pill.dataset.state = m.state;
  const pill = {
    ready: "模型已就绪",
    downloading: "正在下载模型",
    loading: "正在加载模型",
    failed: "模型不可用",
    offline: "连接不上服务器",
  };
  els.pill.textContent = pill[m.state] || m.state;

  let text = "";
  let detail = "";
  let fraction = -1;
  switch (m.state) {
    case "downloading":
      text = "服务器第一次启动，正在下载语音模型，下载完成后就能生成。";
      fraction = m.total ? m.done / m.total : 0;
      detail = `${mb(m.done)} / ${mb(m.total)} MB`;
      if (m.bytesPerSec > 1e3) {
        detail += `，${mb(m.bytesPerSec)} MB/s，剩余${roughly((m.total - m.done) / m.bytesPerSec)}`;
      }
      if (m.error) detail += `。上次下载出错，稍后自动重试：${m.error}`;
      break;
    case "loading":
      text = "正在加载语音模型，通常只要几秒钟。";
      break;
    case "failed":
      text = "语音模型没能加载，暂时不能生成。请管理员查看服务器日志。";
      detail = m.error || "";
      break;
    case "offline":
      text = m.error;
      break;
  }
  els.notice.hidden = state.modelReady;
  els.noticeText.textContent = text;
  els.noticeDetail.textContent = detail;
  els.noticeMeter.hidden = fraction < 0;
  if (fraction >= 0) els.noticeFill.style.width = `${(fraction * 100).toFixed(1)}%`;
  updateButtons();
}

// ---- Input ----

function textLength() {
  return [...els.text.value.trim()].length;
}

function updateCount() {
  const n = textLength();
  els.count.textContent = `${n} / ${state.maxChars} 字`;
  els.count.classList.toggle("is-over", n > state.maxChars);
  updateButtons();
}

function updateButtons() {
  const n = textLength();
  els.generate.disabled = !state.modelReady || !!state.job || n === 0 || n > state.maxChars;
  els.voice.disabled = state.voices.size === 0;
  els.preview.disabled = !state.modelReady || state.previewBusy;
  if (state.previewBusy) els.preview.textContent = "生成中…";
  else if (state.previewAudio) els.preview.textContent = "停止";
  else els.preview.textContent = "试听";
}

async function loadVoices() {
  try {
    const data = await (await api("/api/voices")).json();
    const groups = new Map();
    for (const v of data.voices) {
      state.voices.set(v.name, v);
      if (!groups.has(v.group)) {
        const g = document.createElement("optgroup");
        g.label = v.group;
        groups.set(v.group, g);
        els.voice.append(g);
      }
      const o = document.createElement("option");
      o.value = v.name;
      o.textContent = `${v.display}（${v.group}）`;
      groups.get(v.group).append(o);
    }
    const saved = localStorage.getItem("voice");
    els.voice.value = state.voices.has(saved) ? saved : data.default;
  } catch (err) {
    showError(err.message);
  }
  updateButtons();
}

function showSpeed() {
  els.speedOut.textContent = `${Number(els.speed.value).toFixed(1)} 倍`;
}

// ---- Errors ----

function showError(message) {
  els.error.textContent = message;
  els.error.hidden = false;
}

function hideError() {
  els.error.hidden = true;
}

// ---- Generating ----

async function generate() {
  if (els.generate.disabled) return;
  hideError();
  stopPreview();
  const voice = els.voice.value;
  const job = { id: "", voice: voiceLabel(voice), timer: 0 };
  state.job = job;
  renderJob("正在提交…", 0);
  try {
    const res = await postJSON("/api/jobs", {
      text: els.text.value,
      voice,
      speed: Number(els.speed.value),
    });
    job.id = (await res.json()).id;
    pollJob(job);
  } catch (err) {
    endJob(job);
    showError(err.message);
  }
}

async function pollJob(job) {
  if (state.job !== job) return;
  let st;
  try {
    st = await (await api(`/api/jobs/${job.id}`)).json();
  } catch (err) {
    if (state.job !== job) return;
    if (err.status === 404 || err.status === 401) {
      endJob(job);
      showError(err.status === 404 ? "任务已过期，请重新生成。" : err.message);
      return;
    }
    renderJob("连接中断，正在重试…", null);
    job.timer = setTimeout(() => pollJob(job), 2000);
    return;
  }
  if (state.job !== job) return;
  switch (st.state) {
    case "queued":
      renderJob(st.ahead > 0 ? `排队中，前面还有 ${st.ahead} 个任务` : "马上开始…", 0);
      break;
    case "running":
      renderJob(`正在生成… ${Math.round(st.progress * 100)}%`, st.progress);
      break;
    case "done":
      await receive(job, st);
      return;
    case "failed":
      endJob(job);
      showError(`生成失败：${st.error || "原因不明"}`);
      return;
    case "canceled":
      endJob(job);
      return;
  }
  job.timer = setTimeout(() => pollJob(job), 500);
}

async function receive(job, st) {
  renderJob("正在取回音频…", 1);
  try {
    const blob = await (await api(`/api/jobs/${job.id}/audio`)).blob();
    addTake({
      name: st.filename,
      voice: job.voice,
      duration: st.duration,
      blob,
      samples: wavSamples(await blob.arrayBuffer()),
    });
  } catch (err) {
    showError(`取回音频失败：${err.message}`);
  }
  endJob(job);
}

function renderJob(text, fraction) {
  els.job.hidden = false;
  els.jobText.textContent = text;
  if (fraction !== null) els.jobFill.style.width = `${Math.round(fraction * 100)}%`;
  els.generate.classList.add("is-recording");
  els.cancel.disabled = false;
  updateButtons();
}

function endJob(job) {
  if (state.job !== job) return;
  clearTimeout(job.timer);
  state.job = null;
  els.job.hidden = true;
  els.jobFill.style.width = "0";
  els.generate.classList.remove("is-recording");
  updateButtons();
}

async function cancelJob() {
  const job = state.job;
  if (!job) return;
  els.cancel.disabled = true;
  if (job.id) {
    try {
      await api(`/api/jobs/${job.id}/cancel`, { method: "POST" });
    } catch {}
  }
  endJob(job);
}

// ---- Previews ----

async function preview() {
  const voice = els.voice.value;
  const speed = Number(els.speed.value).toFixed(1);
  const key = `${voice}@${speed}`;
  const wasPlaying = state.previewKey;
  stopPreview();
  if (wasPlaying === key) return; // the button said 停止
  hideError();
  pauseTakes();

  let url = state.previews.get(key);
  if (!url) {
    state.previewBusy = true;
    updateButtons();
    try {
      const res = await postJSON("/api/preview", { voice, speed: Number(speed) });
      url = URL.createObjectURL(await res.blob());
      state.previews.set(key, url);
    } catch (err) {
      showError(`试听失败：${err.message}`);
      return;
    } finally {
      state.previewBusy = false;
      updateButtons();
    }
  }
  const audio = new Audio(url);
  state.previewAudio = audio;
  state.previewKey = key;
  audio.addEventListener("ended", () => {
    if (state.previewAudio === audio) stopPreview();
  });
  audio.play().catch(() => stopPreview());
  updateButtons();
}

function stopPreview() {
  if (state.previewAudio) state.previewAudio.pause();
  state.previewAudio = null;
  state.previewKey = "";
  updateButtons();
}

// ---- Takes: the generated audio, with its waveform ----

// wavSamples reads the 16-bit PCM samples of a WAV file.
function wavSamples(buf) {
  const view = new DataView(buf);
  let at = 12;
  while (at + 8 <= view.byteLength) {
    const id = String.fromCharCode(...new Uint8Array(buf, at, 4));
    const size = view.getUint32(at + 4, true);
    if (id === "data") {
      const end = Math.min(at + 8 + size, view.byteLength);
      return new Int16Array(buf.slice(at + 8, end - ((end - at - 8) % 2)));
    }
    at += 8 + size + (size % 2);
  }
  return new Int16Array(0);
}

// peaks reduces the samples to n loudness values in [0, 1].
function peaks(samples, n = 1200) {
  const out = new Float32Array(n);
  const step = samples.length / n;
  let top = 0;
  for (let i = 0; i < n; i++) {
    const a = Math.floor(i * step);
    const b = Math.max(a + 1, Math.floor((i + 1) * step));
    let m = 0;
    for (let j = a; j < b && j < samples.length; j++) {
      const x = Math.abs(samples[j]);
      if (x > m) m = x;
    }
    out[i] = m;
    if (m > top) top = m;
  }
  if (top > 0) for (let i = 0; i < n; i++) out[i] /= top;
  return out;
}

function addTake({ name, voice, duration, blob, samples }) {
  const node = els.template.content.firstElementChild.cloneNode(true);
  const take = {
    name,
    duration,
    url: URL.createObjectURL(blob),
    downloaded: false,
    peaks: peaks(samples),
    node,
    canvas: node.querySelector(".wave"),
    playBtn: node.querySelector('[data-act="play"]'),
    downloadBtn: node.querySelector('[data-act="download"]'),
    time: node.querySelector(".take-time"),
    raf: 0,
  };
  take.audio = new Audio(take.url);
  node.querySelector(".take-name").textContent = name;
  node.querySelector(".take-name").title = name;
  node.querySelector(".take-info").textContent = voice;
  take.canvas.setAttribute("aria-valuemax", String(Math.round(duration)));

  const a = take.audio;
  a.addEventListener("play", () => {
    take.playBtn.textContent = "暂停";
    const tick = () => {
      paint(take);
      if (!a.paused) take.raf = requestAnimationFrame(tick);
    };
    tick();
  });
  a.addEventListener("pause", () => {
    take.playBtn.textContent = "播放";
    cancelAnimationFrame(take.raf);
    paint(take);
  });
  a.addEventListener("ended", () => {
    a.currentTime = 0;
    paint(take);
  });

  take.playBtn.addEventListener("click", () => {
    if (a.paused) {
      stopPreview();
      pauseTakes(take);
      a.play();
    } else {
      a.pause();
    }
  });
  take.downloadBtn.addEventListener("click", () => download(take));
  node.querySelector('[data-act="remove"]').addEventListener("click", () => removeTake(take));

  const seekTo = (event) => {
    const r = take.canvas.getBoundingClientRect();
    const f = Math.min(1, Math.max(0, (event.clientX - r.left) / r.width));
    a.currentTime = f * totalTime(take);
    paint(take);
  };
  take.canvas.addEventListener("pointerdown", (event) => {
    take.canvas.setPointerCapture(event.pointerId);
    seekTo(event);
  });
  take.canvas.addEventListener("pointermove", (event) => {
    if (take.canvas.hasPointerCapture(event.pointerId)) seekTo(event);
  });
  take.canvas.addEventListener("keydown", (event) => {
    const steps = { ArrowLeft: -2, ArrowRight: 2, PageDown: -10, PageUp: 10 };
    if (event.key in steps) a.currentTime = Math.max(0, Math.min(totalTime(take), a.currentTime + steps[event.key]));
    else if (event.key === "Home") a.currentTime = 0;
    else if (event.key === "End") a.currentTime = totalTime(take);
    else if (event.key === " " || event.key === "Enter") take.playBtn.click();
    else return;
    event.preventDefault();
    paint(take);
  });

  state.takes.unshift(take);
  els.takes.prepend(node);
  els.results.hidden = false;
  resizeObserver.observe(take.canvas);
  paint(take);
}

function totalTime(take) {
  return Number.isFinite(take.audio.duration) ? take.audio.duration : take.duration;
}

function paint(take) {
  const c = take.canvas;
  const total = totalTime(take);
  const now = take.audio.currentTime;
  take.time.textContent = `${clock(now)} / ${clock(total)}`;
  c.setAttribute("aria-valuenow", String(Math.round(now)));
  c.setAttribute("aria-valuetext", `${clock(now)}，共 ${clock(total)}`);

  const w = c.clientWidth;
  const h = c.clientHeight;
  if (!w || !h) return;
  const dpr = window.devicePixelRatio || 1;
  if (c.width !== Math.round(w * dpr) || c.height !== Math.round(h * dpr)) {
    c.width = Math.round(w * dpr);
    c.height = Math.round(h * dpr);
  }
  const ctx = c.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  const css = getComputedStyle(document.documentElement);
  const played = css.getPropertyValue("--rec").trim();
  const rest = css.getPropertyValue("--wave").trim();
  const bar = 2;
  const pitch = 3;
  const n = Math.max(1, Math.floor(w / pitch));
  const at = total ? now / total : 0;
  for (let i = 0; i < n; i++) {
    const p = take.peaks[Math.floor((i * take.peaks.length) / n)];
    const bh = Math.max(2, Math.sqrt(p) * (h - 4)); // sqrt lifts quiet syllables
    ctx.fillStyle = (i + 0.5) / n <= at ? played : rest;
    ctx.fillRect(i * pitch, (h - bh) / 2, bar, bh);
  }
}

const resizeObserver = new ResizeObserver(() => state.takes.forEach(paint));
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => state.takes.forEach(paint));

function pauseTakes(except) {
  for (const t of state.takes) if (t !== except) t.audio.pause();
}

function download(take) {
  const a = document.createElement("a");
  a.href = take.url;
  a.download = take.name;
  document.body.append(a);
  a.click();
  a.remove();
  take.downloaded = true;
  take.downloadBtn.textContent = "已下载";
}

function removeTake(take) {
  if (!take.downloaded && !confirm("这段音频还没下载，移除后就找不回来了。确定移除吗？")) return;
  take.audio.pause();
  cancelAnimationFrame(take.raf);
  resizeObserver.unobserve(take.canvas);
  URL.revokeObjectURL(take.url);
  take.node.remove();
  state.takes = state.takes.filter((t) => t !== take);
  els.results.hidden = state.takes.length === 0;
}

// ---- Wiring ----

els.shortcut.textContent = isMac ? "⌘ Enter" : "Ctrl + Enter";
els.text.addEventListener("input", updateCount);
els.text.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) {
    event.preventDefault();
    generate();
  }
});
els.form.addEventListener("submit", (event) => {
  event.preventDefault();
  generate();
});
els.cancel.addEventListener("click", cancelJob);
els.preview.addEventListener("click", preview);
els.voice.addEventListener("change", () => {
  localStorage.setItem("voice", els.voice.value);
  stopPreview();
});
els.speed.addEventListener("input", () => {
  showSpeed();
  localStorage.setItem("speed", els.speed.value);
});

const savedSpeed = Number(localStorage.getItem("speed"));
if (savedSpeed >= 0.5 && savedSpeed <= 2) els.speed.value = String(savedSpeed);
showSpeed();

// Closing the page cancels its job, and warns about audio not downloaded.
window.addEventListener("pagehide", () => {
  if (state.job && state.job.id) navigator.sendBeacon(`/api/jobs/${state.job.id}/cancel`);
});
window.addEventListener("beforeunload", (event) => {
  if (state.job || state.takes.some((t) => !t.downloaded)) {
    event.preventDefault();
    event.returnValue = "";
  }
});

updateCount();
loadVoices();
refreshStatus();
