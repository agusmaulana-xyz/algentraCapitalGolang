const form = document.querySelector("#connectForm");
const loginInput = document.querySelector("#login");
const serverInput = document.querySelector("#server");
const passwordInput = document.querySelector("#password");
const connectButton = document.querySelector("#connectButton");
const resultBox = document.querySelector("#resultBox");
const accountsList = document.querySelector("#accountsList");
const accountCount = document.querySelector("#accountCount");
const modeBadge = document.querySelector("#modeBadge");
const terminalNotice = document.querySelector("#terminalNotice");
const terminalNoticeTitle = document.querySelector("#terminalNoticeTitle");
const terminalNoticeText = document.querySelector("#terminalNoticeText");
const connectionHint = document.querySelector("#connectionHint");
const serverSelections = new WeakMap();
const serverPickerStates = new WeakMap();

const masterForm = document.querySelector("#masterForm");
const masterResult = document.querySelector("#masterResult");
const masterStatusCard = document.querySelector("#masterStatusCard");
const pauseMasterButton = document.querySelector("#pauseMasterButton");
const deleteMasterButton = document.querySelector("#deleteMasterButton");
const clientForm = document.querySelector("#clientForm");
const clientResult = document.querySelector("#clientResult");
const copyClientsList = document.querySelector("#copyClientsList");
const copyClientCount = document.querySelector("#copyClientCount");
let csrfToken = "";

async function getJSON(path) {
  const response = await fetch(path, { headers: { Accept: "application/json" } });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "Server lokal tidak dapat dihubungi.");
  return data;
}

async function postJSON(path, body) {
  const response = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", "X-CSRF-Token": csrfToken },
    body: JSON.stringify(body),
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "Permintaan tidak dapat diproses.");
  return data;
}

function renderAccounts(items) {
  accountCount.textContent = String(items.length).padStart(2, "0");
  accountsList.replaceChildren();
  if (!items.length) {
    const empty = document.createElement("div");
    empty.className = "empty-state";
    const title = document.createElement("strong");
    title.textContent = "Belum ada pemeriksaan";
    const message = document.createElement("p");
    message.textContent = "Akun yang diuji akan muncul di sini.";
    empty.append(title, message);
    accountsList.append(empty);
    return;
  }
  for (const item of items) {
    const card = document.createElement("article");
    card.className = "account-card";
    const symbol = document.createElement("div");
    symbol.className = "account-symbol";
    symbol.textContent = "MT5";
    const main = document.createElement("div");
    main.className = "account-main";
    const login = document.createElement("div");
    login.className = "account-login";
    login.textContent = item.login;
    const server = document.createElement("div");
    server.className = "account-server";
    server.textContent = item.server;
    const details = [login, server];
    if (item.symbol) {
      const instrument = document.createElement("div");
      instrument.className = "account-instrument";
      instrument.textContent = `Pair XAUUSD: ${item.symbol}`;
      details.push(instrument);
    }
    if (item.balance !== null && item.balance !== undefined && item.currency) {
      const balance = document.createElement("div");
      balance.className = "account-balance";
      const label = document.createElement("span");
      label.textContent = "Saldo";
      const amount = document.createElement("strong");
      amount.textContent = formatBalance(item.balance, item.currency);
      balance.append(label, amount);
      details.push(balance);
    }
    const date = document.createElement("div");
    date.className = "account-date";
    date.textContent = new Date(item.updated_at).toLocaleString("id-ID");
    details.push(date);
    main.append(...details);
    const status = document.createElement("span");
    status.className = `account-state ${item.status === "connected" ? "connected" : "failed"}`;
    status.textContent = item.status === "connected" ? "TERHUBUNG" : "GAGAL";
    const actions = document.createElement("div");
    actions.className = "account-actions";
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = "account-delete-button";
    remove.textContent = "Hapus riwayat";
    remove.setAttribute("aria-label", `Hapus riwayat akun ${item.login} di ${item.server}`);
    remove.addEventListener("click", async () => {
      if (!window.confirm(`Hapus catatan akun ${item.login} (${item.server}) dan folder terminal verifikasi lokalnya? Akun broker dan posisi trading tidak akan diubah.`)) return;
      remove.disabled = true;
      try {
        await postJSON(`/api/accounts/${item.id}/delete`, {});
        await refreshAccounts();
        showResult(resultBox, true, "Akun dihapus", "Catatan dan folder terminal verifikasi lokal sudah dihapus.");
      } catch (error) {
        showResult(resultBox, false, "Akun belum terhapus", error.message);
      } finally { remove.disabled = false; }
    });
    actions.append(remove);
    card.append(symbol, main, status, actions);
    accountsList.append(card);
  }
}

function formatBalance(balance, currency) {
  const value = Number(balance);
  if (!Number.isFinite(value)) return "—";
  const formatted = new Intl.NumberFormat("id-ID", {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(value);
  return `${formatted} ${currency}`;
}

function showResult(box, valid, title, message) {
  box.replaceChildren();
  box.className = `result-box ${valid ? "success" : "error"}`;
  const strong = document.createElement("strong");
  strong.textContent = title;
  const text = document.createElement("span");
  text.textContent = message || "Tidak ada detail tambahan.";
  box.append(strong, text);
  box.hidden = false;
}

async function refreshAccounts() {
  const data = await getJSON("/api/accounts");
  renderAccounts(data.accounts || []);
}

function hideServerSuggestions(input, suggestions) {
  suggestions.hidden = true;
  suggestions.replaceChildren();
  input.setAttribute("aria-expanded", "false");
}

function showServerSuggestions(input, suggestions, items, query) {
  suggestions.replaceChildren();
  if (!items.length) {
    const empty = document.createElement("div");
    empty.className = "server-empty";
    empty.textContent = `Server untuk “${query}” tidak ditemukan. Periksa nama server dari broker.`;
    suggestions.append(empty);
    suggestions.hidden = false;
    input.setAttribute("aria-expanded", "true");
    return;
  }
  for (const item of items) {
    const option = document.createElement("button");
    option.className = "server-option";
    option.type = "button";
    option.setAttribute("role", "option");
    const name = document.createElement("span");
    name.className = "server-option-name";
    name.textContent = item.name;
    const company = document.createElement("span");
    company.className = "server-option-company";
    company.textContent = item.company;
    option.append(name, company);
    option.addEventListener("click", () => {
      input.value = item.name;
      serverSelections.set(input, { name: item.name, company: item.company });
      hideServerSuggestions(input, suggestions);
    });
    suggestions.append(option);
  }
  suggestions.hidden = false;
  input.setAttribute("aria-expanded", "true");
}

function attachServerSearch(input, suggestions) {
  const state = { timer: 0, sequence: 0 };
  serverPickerStates.set(input, state);
  input.addEventListener("input", () => {
    const query = input.value.trim();
    const selected = serverSelections.get(input);
    if (!selected || query !== selected.name) serverSelections.delete(input);
    window.clearTimeout(state.timer);
    const sequence = ++state.sequence;
    if (query.length < 3) {
      hideServerSuggestions(input, suggestions);
      return;
    }
    suggestions.replaceChildren();
    const loading = document.createElement("div");
    loading.className = "server-empty";
    loading.textContent = "Mencari server broker…";
    suggestions.append(loading);
    suggestions.hidden = false;
    input.setAttribute("aria-expanded", "true");
    state.timer = window.setTimeout(async () => {
      try {
        const response = await fetch(`/api/mt5/servers?q=${encodeURIComponent(query)}`, {
          headers: { Accept: "application/json" },
        });
        const data = await response.json();
        if (!response.ok) throw new Error(data.error || "Pencarian server gagal.");
        if (sequence !== state.sequence || input.value.trim() !== query) return;
        showServerSuggestions(input, suggestions, data.servers || [], query);
      } catch {
        if (sequence !== state.sequence) return;
        suggestions.replaceChildren();
        const error = document.createElement("div");
        error.className = "server-empty";
        error.textContent = "Direktori broker tidak tersedia. Masukkan nama server persis dari broker.";
        suggestions.append(error);
      }
    }, 300);
  });
  input.addEventListener("keydown", (event) => {
    if (event.key === "Escape") hideServerSuggestions(input, suggestions);
  });
}

function setButtonLoading(button, loading, label) {
  button.disabled = loading;
  button.classList.toggle("loading", loading);
  button.querySelector(".button-label").textContent = loading ? label : button.dataset.label;
}

function renderMasterStatus(master) {
  masterStatusCard.replaceChildren();
  pauseMasterButton.hidden = !master;
  deleteMasterButton.hidden = !master;
  if (!master) {
    masterStatusCard.dataset.status = "offline";
    const title = document.createElement("strong");
    title.textContent = "Akun utama belum dihubungkan";
    const message = document.createElement("span");
    message.textContent = "Hubungkan akun MT5 yang akan menjadi sumber entry.";
    masterStatusCard.append(title, message);
    pauseMasterButton.hidden = true;
    deleteMasterButton.hidden = true;
    return;
  }
  masterStatusCard.dataset.status = master.connected ? "connected" : master.status;
  const title = document.createElement("strong");
  title.textContent = `${master.connected ? "TERHUBUNG" : String(master.status || "OFFLINE").toUpperCase()} · ${master.login}`;
  const detail = document.createElement("span");
  detail.textContent = `${master.server} · ${master.connected ? `${master.positions} posisi diterbitkan` : (master.message || "Sambungkan ulang akun utama.")}`;
  masterStatusCard.append(title, detail);
}

function makeActionButton(label, className, action) {
  const button = document.createElement("button");
  button.type = "button";
  button.textContent = label;
  if (className) button.className = className;
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      await action();
      await refreshCopyStatus();
    } catch (error) {
      showResult(clientResult, false, "Pengaturan copy gagal", error.message);
    } finally {
      button.disabled = false;
    }
  });
  return button;
}

function renderCopyClients(items) {
  copyClientCount.textContent = String(items.length).padStart(2, "0");
  copyClientsList.replaceChildren();
  if (!items.length) {
    const empty = document.createElement("div");
    empty.className = "empty-state";
    const title = document.createElement("strong");
    title.textContent = "Belum ada akun client";
    const message = document.createElement("p");
    message.textContent = "Akun client yang terhubung akan muncul di sini.";
    empty.append(title, message);
    copyClientsList.append(empty);
    return;
  }
  for (const client of items) {
    const card = document.createElement("article");
    card.className = "copy-client-card";
    const main = document.createElement("div");
    main.className = "copy-client-main";
    const title = document.createElement("strong");
    title.textContent = client.login;
    const server = document.createElement("span");
    server.textContent = client.server;
    const status = document.createElement("span");
    status.className = "copy-client-state";
    status.dataset.status = client.status;
    status.textContent = String(client.status || "offline").toUpperCase();
    const details = document.createElement("div");
    details.className = "copy-client-meta";
    const lotText = client.lot_mode === "tetap"
      ? `Lot tetap ${Number(client.fixed_lot).toFixed(2)}`
      : `Rasio ${Number(client.ratio_lot).toFixed(2)}× lot utama`;
    details.textContent = `${lotText} · batas ${Number(client.max_lot_per_order).toFixed(2)} lot/entry, ${Number(client.max_lot_total).toFixed(2)} total, ${client.max_open_positions} posisi`;
    if (client.balance !== null && client.balance !== undefined && client.currency) {
      details.textContent += ` · saldo ${formatBalance(client.balance, client.currency)}`;
    }
    if (client.message) details.textContent += ` · ${client.message}`;
    main.append(title, server, status, details);
    const actions = document.createElement("div");
    actions.className = "copy-client-actions";
    if (client.copy_enabled) {
      if (client.dry_run) {
        actions.append(makeActionButton("Aktifkan order live", "live-action", async () => {
          if (!window.confirm(`Aktifkan pengiriman order live ke akun ${client.login} (${client.server})? Order akan mengikuti entry dan perubahan posisi akun utama.`)) return;
          await postJSON(`/api/copy/clients/${client.id}/mode`, { enabled: true, dry_run: false });
        }));
      } else {
        actions.append(makeActionButton("Kembali ke simulasi", "", async () => {
          await postJSON(`/api/copy/clients/${client.id}/mode`, { enabled: true, dry_run: true });
        }));
      }
      actions.append(makeActionButton("Jeda", "pause-action", async () => {
        await postJSON(`/api/copy/clients/${client.id}/pause`, {});
      }));
    } else {
      actions.append(makeActionButton("Mulai simulasi", "", async () => {
        await postJSON(`/api/copy/clients/${client.id}/mode`, { enabled: true, dry_run: true });
      }));
      actions.append(makeActionButton("Aktifkan order live", "live-action", async () => {
        if (!window.confirm(`Aktifkan pengiriman order live ke akun ${client.login} (${client.server})? Batas lot yang tampil akan diterapkan.`)) return;
        await postJSON(`/api/copy/clients/${client.id}/mode`, { enabled: true, dry_run: false });
      }));
    }
    actions.append(makeActionButton("Hapus akun", "delete-action", async () => {
      if (!window.confirm(`Hapus akun client ${client.login} (${client.server}) beserta folder terminal lokalnya? Posisi dan pending order yang sudah berada di broker tidak ditutup atau dibatalkan.`)) return;
      await postJSON(`/api/copy/clients/${client.id}/delete`, {});
      await refreshAccounts();
    }));
    card.append(main, actions);
    copyClientsList.append(card);
  }
}

async function refreshCopyStatus() {
  const data = await getJSON("/api/copy/status");
  renderMasterStatus(data.master || null);
  renderCopyClients(data.clients || []);
}

async function initialize() {
  try {
    const session = await getJSON("/api/auth/session");
    if (!session.authenticated || session.role !== "admin") {
      window.location.replace("/admin/login");
      return;
    }
    csrfToken = session.csrf_token;
    const status = await getJSON("/api/status");
    modeBadge.dataset.mode = "terminal";
    modeBadge.querySelector("span").textContent = "REAL · MT5";
    const ready = status.terminal_configured && status.terminal_exists && status.probe_ready;
    terminalNotice.hidden = ready;
    if (!status.terminal_configured) {
      terminalNoticeTitle.textContent = "Path terminal belum diatur";
      terminalNoticeText.textContent = "Jalankan run.bat dan masukkan path terminal64.exe MetaTrader 5.";
    } else if (!status.terminal_exists) {
      terminalNoticeTitle.textContent = "terminal64.exe tidak ditemukan";
      terminalNoticeText.textContent = "Periksa nilai MT5_TERMINAL_PATH, lalu jalankan ulang run.bat.";
    } else if (!status.probe_ready) {
      terminalNoticeTitle.textContent = "MT5LoginProbe belum tersedia";
      terminalNoticeText.textContent = "Aplikasi tidak memiliki binary Expert yang dapat dijalankan.";
    }
    connectionHint.textContent = ready ? "Siap · login akan diuji langsung ke server broker." : "Lengkapi konfigurasi terminal sebelum verifikasi.";
    await Promise.all([refreshAccounts(), refreshCopyStatus()]);
  } catch (error) {
    connectionHint.textContent = error.message;
    showResult(resultBox, false, "Server lokal belum siap", "Jalankan run.bat lalu buka alamat localhost yang ditampilkan.");
  }
}

document.querySelector("#logoutButton")?.addEventListener("click", async () => {
  try {
    await postJSON("/api/auth/logout", {});
    window.location.replace("/");
  } catch (error) {
    showResult(masterResult, false, "Logout gagal", error.message);
  }
});

for (const input of document.querySelectorAll(".server-input")) {
  const id = input.getAttribute("aria-controls");
  const suggestions = document.getElementById(id);
  if (suggestions) attachServerSearch(input, suggestions);
}
if (serverInput && serverSuggestionsFallback()) attachServerSearch(serverInput, serverSuggestionsFallback());

function serverSuggestionsFallback() {
  return document.querySelector("#serverSuggestions");
}

document.addEventListener("click", (event) => {
  if (event.target.closest(".server-search")) return;
  for (const input of document.querySelectorAll(".server-input, #server")) {
    const suggestions = document.getElementById(input.getAttribute("aria-controls"));
    if (suggestions) hideServerSuggestions(input, suggestions);
  }
});

pauseMasterButton.addEventListener("click", async () => {
  pauseMasterButton.disabled = true;
  try {
    await postJSON("/api/copy/master/pause", {});
    await refreshCopyStatus();
  } catch (error) {
    showResult(masterResult, false, "Akun utama gagal dijeda", error.message);
  } finally {
    pauseMasterButton.disabled = false;
  }
});

deleteMasterButton.addEventListener("click", async () => {
  deleteMasterButton.disabled = true;
  try {
    const master = await getJSON("/api/copy/status").then((data) => data.master);
    if (!master || !window.confirm(`Hapus akun utama ${master.login} (${master.server}) dan folder terminal lokalnya? Semua client akan dijeda. Posisi dan pending order di broker tetap ada dan tidak ditutup.`)) return;
    await postJSON("/api/copy/master/delete", {});
    await Promise.all([refreshCopyStatus(), refreshAccounts()]);
    showResult(masterResult, true, "Akun utama dihapus", "Folder terminal dibersihkan dan semua client dijeda.");
  } catch (error) {
    showResult(masterResult, false, "Akun utama belum terhapus", error.message);
  } finally { deleteMasterButton.disabled = false; }
});

masterForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = document.querySelector("#masterButton");
  button.dataset.label = "Hubungkan akun utama";
  setButtonLoading(button, true, "Menghubungkan akun utama");
  try {
    const selected = serverSelections.get(document.querySelector("#masterServer")) || {};
    const data = await postJSON("/api/copy/master", {
      login: document.querySelector("#masterLogin").value.trim(),
      server: document.querySelector("#masterServer").value.trim(),
      company: selected.company || "",
      password: document.querySelector("#masterPassword").value,
    });
    showResult(masterResult, data.valid, data.valid ? "Akun utama siap" : "Login Belum Berhasil", data.message || data.error);
    await refreshCopyStatus();
    await refreshAccounts();
  } catch (error) {
    showResult(masterResult, false, "Akun utama gagal dihubungkan", error.message);
  } finally {
    document.querySelector("#masterPassword").value = "";
    setButtonLoading(button, false, "Hubungkan akun utama");
  }
});

clientForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const button = document.querySelector("#clientButton");
  button.dataset.label = "Hubungkan akun client";
  setButtonLoading(button, true, "Memeriksa akun client");
  try {
    const selected = serverSelections.get(document.querySelector("#clientServer")) || {};
    const data = await postJSON("/api/copy/clients", {
      login: document.querySelector("#clientLogin").value.trim(),
      server: document.querySelector("#clientServer").value.trim(),
      company: selected.company || "",
      password: document.querySelector("#clientPassword").value,
      lot_mode: document.querySelector("#clientLotMode").value,
      ratio_lot: Number(document.querySelector("#clientRatio").value),
      fixed_lot: Number(document.querySelector("#clientFixedLot").value),
      max_lot_per_order: Number(document.querySelector("#clientMaxOrder").value),
      max_lot_total: Number(document.querySelector("#clientMaxTotal").value),
      max_open_positions: Number(document.querySelector("#clientMaxPositions").value),
    });
    showResult(clientResult, data.valid, data.valid ? "Akun client standby" : "Login Belum Berhasil", data.message || data.error);
    await refreshCopyStatus();
    await refreshAccounts();
  } catch (error) {
    showResult(clientResult, false, "Akun client gagal dihubungkan", error.message);
  } finally {
    document.querySelector("#clientPassword").value = "";
    setButtonLoading(button, false, "Hubungkan akun client");
  }
});

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  resultBox.hidden = true;
  connectButton.disabled = true;
  connectButton.classList.add("loading");
  connectButton.querySelector(".button-label").textContent = "Memeriksa koneksi";
  try {
    const selected = serverSelections.get(serverInput) || {};
    const response = await fetch("/api/connect", {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json", "X-CSRF-Token": csrfToken },
      body: JSON.stringify({
        login: loginInput.value.trim(),
        server: serverInput.value.trim(),
        company: selected.company || "",
        password: passwordInput.value,
      }),
    });
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || "Data login tidak valid.");
    showResult(resultBox, data.valid, data.valid ? "Pemeriksaan selesai" : "Login Belum Berhasil", data.message);
    passwordInput.value = "";
    await refreshAccounts();
  } catch (error) {
    showResult(resultBox, false, "Pemeriksaan gagal", error.message || "Server lokal tidak merespons.");
  } finally {
    connectButton.disabled = false;
    connectButton.classList.remove("loading");
    connectButton.querySelector(".button-label").textContent = "Verifikasi login broker";
  }
});

initialize();
window.setInterval(() => {
  refreshCopyStatus().catch(() => {});
}, 3000);
