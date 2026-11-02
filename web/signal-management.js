(() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  let csrf = "";
  let liveValue = false;
  let groupsLoaded = false;
  let groupsDirty = false;
  let toastTimer = 0;

  function notify(message, error = false) {
    const toast = $("toast");
    toast.textContent = message;
    toast.classList.toggle("error", error);
    toast.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { toast.hidden = true; }, 4200);
  }
  function feedback(id, message, type = "") {
    const node = $(id);
    node.textContent = message;
    node.className = "feedback" + (type ? " " + type : "");
    node.hidden = !message;
  }
  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (options.method && options.method !== "GET") {
      headers.set("Content-Type", "application/json");
      headers.set("X-CSRF-Token", csrf);
    }
    const response = await fetch(path, { credentials: "same-origin", cache: "no-store", ...options, headers });
    let body = {};
    try { body = await response.json(); } catch (_) {}
    if (!response.ok) throw new Error(body.error || "Permintaan tidak berhasil.");
    return body;
  }
  function setStatus(dotId, stateId, detailId, ready, title, detail) {
    $(dotId).className = "dot" + (ready ? " good" : " bad");
    $(stateId).textContent = title;
    $(detailId).textContent = detail;
  }
  function dateLabel(value) {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("id-ID", { dateStyle: "short", timeStyle: "short" });
  }
  function fmt(value) {
    return value == null || !Number.isFinite(Number(value)) ? "—" : Number(value).toLocaleString("id-ID", { maximumFractionDigits: 2 });
  }
  function renderSignals(signals) {
    const body = $("signalRows");
    body.replaceChildren();
    if (!signals.length) {
      const row = document.createElement("tr");
      const cell = document.createElement("td");
      cell.colSpan = 7;
      cell.className = "empty";
      cell.textContent = "Belum ada signal.";
      row.append(cell); body.append(row); return;
    }
    for (const signal of signals) {
      const row = document.createElement("tr");
      const values = [
        dateLabel(signal.created_at),
        signal.group || "Grup Telegram",
        signal.action ? signal.action + " / " + signal.symbol : "—",
        signal.entry_low != null ? fmt(signal.entry_low) + " – " + fmt(signal.entry_high) : "—",
        signal.sl != null ? "SL " + fmt(signal.sl) + " · TP " + (signal.tp || []).map(fmt).join(" / ") : "—",
        "W " + ((signal.results || {}).WIN || 0) + " / L " + ((signal.results || {}).LOSS || 0) + " / BE " + ((signal.results || {}).BE || 0) + " / EXP " + ((signal.results || {}).EXPIRED || 0) + " · " + fmt(signal.net_profit),
      ];
      values.forEach((value, index) => {
        const cell = document.createElement("td");
        cell.textContent = value;
        if (index === 2 && signal.action) {
          const span = document.createElement("span");
          span.className = "signal-action" + (signal.action === "SELL" ? " sell" : "");
          span.textContent = value;
          cell.replaceChildren(span);
        }
        row.append(cell);
      });
      const status = document.createElement("td");
      const pill = document.createElement("span");
      pill.className = "status-pill " + (signal.status || "");
      pill.textContent = (signal.status || "UNKNOWN") + (signal.reason ? " · " + signal.reason : "");
      status.append(pill); row.append(status); body.append(row);
    }
  }
  function renderGroups(groups, saved) {
    const list = $("groupsList");
    list.replaceChildren();
    const enabled = new Set(saved.filter((item) => item.enabled).map((item) => item.chat_id));
    if (!groups.length) {
      const empty = document.createElement("div");
      empty.className = "empty";
      empty.textContent = "Tidak ada grup atau channel Telegram yang dapat dipilih.";
      list.append(empty); $("saveGroups").disabled = true; return;
    }
    for (const group of groups) {
      const label = document.createElement("label");
      label.className = "group-row";
      const checkbox = document.createElement("input");
      checkbox.type = "checkbox";
      checkbox.dataset.chatId = group.chat_id;
      checkbox.checked = enabled.has(group.chat_id);
      const name = document.createElement("span");
      const strong = document.createElement("strong");
      strong.textContent = group.name || "Grup Telegram";
      const small = document.createElement("small");
      small.textContent = group.username ? "@" + group.username : group.chat_id;
      name.append(strong, small); label.append(checkbox, name); list.append(label);
    }
    $("saveGroups").disabled = false;
  }
  async function loadGroups() {
    if (groupsDirty) return;
    $("groupsList").textContent = "Memuat grup Telegram...";
    const data = await api("/api/admin/signals/telegram/groups");
    renderGroups(data.groups || [], data.saved || []);
    groupsLoaded = true;
  }
  async function loadDashboard() {
    const data = await api("/api/admin/signals/dashboard");
    const telegram = data.telegram || {};
    const isTelegram = telegram.connected === true;
    setStatus("telegramDot", "telegramState", "telegramDetail", isTelegram, isTelegram ? "Terhubung" : "Belum terhubung", isTelegram ? telegram.account_name || "Akun Telegram aktif" : telegram.configured ? "Login akun Telegram personal diperlukan" : "Isi TELEGRAM_API_ID dan TELEGRAM_API_HASH");
    $("telegramConnected").hidden = !isTelegram;
    $("phoneForm").hidden = isTelegram;
    if (!isTelegram) {
      groupsLoaded = false;
      groupsDirty = false;
      $("groupsList").textContent = "Login Telegram untuk memuat grup.";
    }
    $("codeForm").hidden = isTelegram || telegram.state !== "awaiting_code";
    $("twoFactorForm").hidden = isTelegram || telegram.state !== "awaiting_2fa";
    $("telegramAccount").textContent = telegram.account_name || "Akun Telegram terhubung";

    const gemini = data.gemini || {};
    setStatus("geminiDot", "geminiState", "geminiDetail", gemini.configured === true, gemini.configured ? "Siap · " + gemini.model : "Belum dikonfigurasi", gemini.configured ? "Gemini structured output aktif" : "Isi GEMINI_API_KEY lalu mulai ulang aplikasi");
    const master = data.master || {};
    setStatus("masterDot", "masterState", "masterDetail", master.connected === true, master.connected ? "Terhubung" : "Belum siap", master.connected ? master.login + " · " + master.server : master.message || "Hubungkan akun utama di halaman copy trading");
    const ea = data.ea || {};
    setStatus("eaDot", "eaState", "eaDetail", ea.connected === true, ea.connected ? "Heartbeat aktif" : "Menunggu heartbeat", ea.connected ? (ea.terminal || "MT5") + " · " + dateLabel(ea.heartbeat_at) : "Pastikan EA dan WebRequest tersedia");

    liveValue = data.live_enabled === true;
    $("liveSwitch").checked = liveValue;
    $("liveBadge").textContent = liveValue ? "LIVE AKTIF" : "NONAKTIF";
    $("liveBadge").classList.toggle("on", liveValue);
    $("liveDescription").textContent = liveValue ? "Signal baru dapat dikirim ke terminal master." : "Order tidak dibuat; parsing disimpan sebagai dry run.";
    $("webrequestURL").textContent = data.webrequest_url || "http://127.0.0.1:8080";
    $("winrate").textContent = fmt(data.winrate || 0) + "%";
    const stats = data.stats || {};
    $("winLoss").textContent = (stats.wins || 0) + " / " + (stats.losses || 0);
    $("netProfit").textContent = fmt(stats.net_profit || 0);
    $("signalTotal").textContent = stats.total || 0;
    renderSignals(data.signals || []);
    if (isTelegram && !groupsLoaded) {
      try { await loadGroups(); } catch (error) { feedback("groupsMessage", error.message, "error"); }
    }
  }
  async function initialize() {
    const session = await api("/api/auth/session");
    if (!session.authenticated || session.role !== "admin") {
      window.location.replace("/admin/login");
      return;
    }
    csrf = session.csrf_token || "";
    $("logoutButton").addEventListener("click", async () => {
      try { await api("/api/auth/logout", { method: "POST", body: "{}" }); window.location.assign("/admin/login"); }
      catch (error) { notify(error.message, true); }
    });
    $("phoneForm").addEventListener("submit", async (event) => {
      event.preventDefault(); feedback("telegramMessage", "Mengirim kode...");
      try {
        await api("/api/admin/signals/telegram/send-code", { method: "POST", body: JSON.stringify({ phone: $("phone").value }) });
        feedback("telegramMessage", "Kode login telah dikirim oleh Telegram.", "good");
        $("codeForm").hidden = false;
      } catch (error) { feedback("telegramMessage", error.message, "error"); }
    });
    $("codeForm").addEventListener("submit", async (event) => {
      event.preventDefault();
      try {
        const result = await api("/api/admin/signals/telegram/verify-code", { method: "POST", body: JSON.stringify({ code: $("loginCode").value }) });
        if (result.requires_2fa) { $("twoFactorForm").hidden = false; $("codeForm").hidden = true; feedback("telegramMessage", "Masukkan password verifikasi dua langkah."); }
        else { feedback("telegramMessage", "Akun Telegram berhasil terhubung.", "good"); await loadDashboard(); }
      } catch (error) { feedback("telegramMessage", error.message, "error"); }
    });
    $("twoFactorForm").addEventListener("submit", async (event) => {
      event.preventDefault();
      try {
        await api("/api/admin/signals/telegram/verify-2fa", { method: "POST", body: JSON.stringify({ password: $("twoFactorPassword").value }) });
        $("twoFactorPassword").value = ""; feedback("telegramMessage", "Akun Telegram berhasil terhubung.", "good"); await loadDashboard();
      } catch (error) { feedback("telegramMessage", error.message, "error"); }
    });
    $("telegramLogout").addEventListener("click", async () => {
      if (!window.confirm("Keluar dari sesi akun Telegram ini?")) return;
      try { await api("/api/admin/signals/telegram/logout", { method: "POST", body: "{}" }); await loadDashboard(); }
      catch (error) { feedback("telegramMessage", error.message, "error"); }
    });
    $("groupsList").addEventListener("change", (event) => {
      if (event.target instanceof HTMLInputElement && event.target.type === "checkbox") groupsDirty = true;
    });
    $("refreshGroups").addEventListener("click", async () => {
      if (groupsDirty && !window.confirm("Muat ulang grup dan buang perubahan pilihan yang belum disimpan?")) return;
      groupsDirty = false;
      groupsLoaded = false;
      try { await loadGroups(); feedback("groupsMessage", "Daftar grup diperbarui.", "good"); }
      catch (error) { feedback("groupsMessage", error.message, "error"); }
    });
    $("saveGroups").addEventListener("click", async () => {
      const groups = [...$("groupsList").querySelectorAll("input[type=checkbox]")].map((input) => ({ chat_id: input.dataset.chatId, enabled: input.checked }));
      try { await api("/api/admin/signals/telegram/groups", { method: "POST", body: JSON.stringify({ groups }) }); groupsDirty = false; groupsLoaded = false; feedback("groupsMessage", "Pilihan grup sumber berhasil disimpan.", "good"); await loadDashboard(); }
      catch (error) { feedback("groupsMessage", error.message, "error"); }
    });
    $("liveSwitch").addEventListener("change", async () => {
      const wanted = $("liveSwitch").checked;
      if (wanted && !window.confirm("Aktifkan eksekusi live? Signal baru dapat membuat order di akun utama MT5.")) { $("liveSwitch").checked = liveValue; return; }
      try {
        await api("/api/admin/signals/live", { method: "POST", body: JSON.stringify({ enabled: wanted }) });
        feedback("liveMessage", wanted ? "Mode live aktif." : "Mode live dinonaktifkan; signal baru tidak akan membuat order.", "good");
        await loadDashboard();
      } catch (error) { $("liveSwitch").checked = liveValue; feedback("liveMessage", error.message, "error"); }
    });
    $("testParser").addEventListener("click", async () => {
      $("parseResult").hidden = true; feedback("parseMessage", "Mengirim contoh ke Gemini...");
      try {
        const result = await api("/api/admin/signals/parse-test", { method: "POST", body: JSON.stringify({ message: $("sampleMessage").value }) });
        $("parseResult").textContent = JSON.stringify(result.signal, null, 2);
        $("parseResult").hidden = false; feedback("parseMessage", "Parsing selesai. Tidak ada order yang dikirim.", "good");
      } catch (error) { feedback("parseMessage", error.message, "error"); }
    });
    await loadDashboard();
    setInterval(() => loadDashboard().catch(() => {}), 15000);
  }
  initialize().catch((error) => { notify(error.message || "Halaman admin tidak dapat dimuat.", true); });
})();
