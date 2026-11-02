let csrfToken = "";
let serverSearchTimer = 0;
let serverSearchSequence = 0;

async function clientJSON(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: {
      Accept: "application/json",
      ...(options.body ? { "Content-Type": "application/json" } : {}),
      ...(options.method && options.method !== "GET" ? { "X-CSRF-Token": csrfToken } : {}),
      ...options.headers,
    },
  });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "Permintaan tidak dapat diproses.");
  return data;
}

function showClientMessage(message, success = false) {
  const box = document.querySelector("#clientMessage");
  box.textContent = message;
  box.classList.toggle("success", success);
  box.hidden = false;
}

function statusText(account) {
  if (!account) return "BELUM TERHUBUNG";
  return String(account.status || "standby").replaceAll("_", " ").toUpperCase();
}

function renderClientDashboard(data) {
  document.querySelector("#clientName").textContent = data.user.name || "client";
  document.querySelector("#clientNavName").textContent = data.user.email || "CLIENT PORTAL";
  const strategyStatus = document.querySelector("#strategyStatus");
  strategyStatus.dataset.status = data.strategy.status;
  strategyStatus.textContent = data.strategy.status === "active" ? "SUMBER STRATEGI AKTIF" : "STRATEGI MENUNGGU";

  const account = data.account;
  const accountStatus = document.querySelector("#accountStatus");
  const accountSummary = document.querySelector("#accountSummary");
  const accountStats = document.querySelector("#accountStats");
  const connectForm = document.querySelector("#mt5Form");
  const showFormButton = document.querySelector("#showConnectForm");
  const modeActions = document.querySelector("#modeActions");
  const copyStatus = document.querySelector("#copyModeStatus");
  const copyMessage = document.querySelector("#copyModeMessage");

  accountStatus.dataset.status = account?.status || "offline";
  accountStatus.textContent = statusText(account);
  document.querySelector("#accountMessage").textContent = account?.message || (account ? "Status koneksi MT5 Anda." : "Hubungkan akun trading broker untuk memulai dalam mode standby.");
  accountSummary.replaceChildren();
  accountStats.replaceChildren();
  accountSummary.hidden = !account;
  accountStats.hidden = !account;
  if (account) {
    const accountLogin = document.createElement("strong");
    accountLogin.textContent = account.login;
    const accountServer = document.createElement("span");
    accountServer.textContent = account.server;
    accountSummary.append(accountLogin, accountServer);
    const stats = [
      ["MODE LOT", account.lot_mode === "tetap" ? `Tetap ${Number(account.fixed_lot).toFixed(2)}` : `Rasio ${Number(account.ratio_lot).toFixed(2)}×`],
      ["BATAS / ENTRY", `${Number(account.max_lot_per_order).toFixed(2)} lot`],
      ["BATAS TOTAL", `${Number(account.max_lot_total).toFixed(2)} lot · ${account.max_open_positions} posisi`],
    ];
    if (account.balance !== null && account.balance !== undefined && account.currency) stats[0] = ["SALDO TERVERIFIKASI", `${new Intl.NumberFormat("id-ID", { maximumFractionDigits: 2 }).format(account.balance)} ${account.currency}`];
    for (const [label, value] of stats) {
      const item = document.createElement("div");
      const small = document.createElement("small");
      small.textContent = label;
      const strong = document.createElement("strong");
      strong.textContent = value;
      item.append(small, strong);
      accountStats.append(item);
    }
  }
  connectForm.hidden = Boolean(account) && !connectForm.dataset.editing;
  showFormButton.hidden = !account;
  modeActions.hidden = !account;
  showFormButton.textContent = connectForm.dataset.editing ? "Tutup pengaturan" : "Perbarui koneksi / batas risiko";

  const enabled = Boolean(account?.copy_enabled);
  const dryRun = Boolean(account?.dry_run);
  const mode = !account ? "standby" : enabled ? (dryRun ? "simulasi" : "copying") : "standby";
  copyStatus.dataset.status = mode;
  copyStatus.textContent = mode === "copying" ? "LIVE AKTIF" : mode.toUpperCase();
  copyMessage.textContent = mode === "copying"
    ? "Order live mengikuti posisi sumber dan batas risiko yang ditampilkan."
    : mode === "simulasi"
      ? "Simulasi berjalan tanpa mengirim order ke broker."
      : account ? "Akun tersambung, tetapi belum mengirim order baru." : "Akun baru selalu dimulai dalam mode standby.";

  const insights = document.querySelector("#insightList");
  insights.replaceChildren();
  for (const message of data.insights || []) {
    const item = document.createElement("li");
    item.textContent = message;
    insights.append(item);
  }
}

async function refreshClientDashboard() {
  const data = await clientJSON("/api/client/dashboard");
  renderClientDashboard(data);
}

function attachMT5ServerSearch() {
  const input = document.querySelector("#mt5Server");
  const suggestions = document.querySelector("#mt5ServerSuggestions");
  input.addEventListener("input", () => {
    const query = input.value.trim();
    input.dataset.company = "";
    window.clearTimeout(serverSearchTimer);
    const sequence = ++serverSearchSequence;
    if (query.length < 3) {
      suggestions.hidden = true;
      suggestions.replaceChildren();
      input.setAttribute("aria-expanded", "false");
      return;
    }
    serverSearchTimer = window.setTimeout(async () => {
      try {
        const response = await fetch(`/api/mt5/servers?q=${encodeURIComponent(query)}`, { headers: { Accept: "application/json" } });
        const data = await response.json();
        if (!response.ok) throw new Error(data.error || "Pencarian server gagal.");
        if (sequence !== serverSearchSequence || input.value.trim() !== query) return;
        suggestions.replaceChildren();
        for (const item of data.servers || []) {
          const option = document.createElement("button");
          option.type = "button";
          option.className = "server-option";
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
            input.dataset.company = item.company;
            suggestions.hidden = true;
            input.setAttribute("aria-expanded", "false");
          });
          suggestions.append(option);
        }
        if (!suggestions.childElementCount) {
          const empty = document.createElement("div");
          empty.className = "server-empty";
          empty.textContent = "Server tidak ditemukan. Masukkan nama persis dari broker.";
          suggestions.append(empty);
        }
        suggestions.hidden = false;
        input.setAttribute("aria-expanded", "true");
      } catch {
        if (sequence !== serverSearchSequence) return;
        suggestions.replaceChildren();
        const empty = document.createElement("div");
        empty.className = "server-empty";
        empty.textContent = "Direktori broker tidak tersedia. Masukkan nama server persis dari broker.";
        suggestions.append(empty);
        suggestions.hidden = false;
        input.setAttribute("aria-expanded", "true");
      }
    }, 280);
  });
  document.addEventListener("click", (event) => {
    if (event.target.closest(".server-search")) return;
    suggestions.hidden = true;
    input.setAttribute("aria-expanded", "false");
  });
}

async function updateMode(enabled, dryRun, acknowledge = false) {
  await clientJSON("/api/client/mode", {
    method: "POST",
    body: JSON.stringify({ enabled, dry_run: dryRun, acknowledge_live_risk: acknowledge }),
  });
  await refreshClientDashboard();
}

document.querySelector("#mt5Form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const button = document.querySelector("#mt5Button");
  const original = button.querySelector("span").textContent;
  button.disabled = true;
  button.querySelector("span").textContent = "Menghubungkan terminal…";
  document.querySelector("#clientMessage").hidden = true;
  try {
    const response = await clientJSON("/api/client/mt5", {
      method: "POST",
      body: JSON.stringify({
        login: document.querySelector("#mt5Login").value.trim(),
        server: document.querySelector("#mt5Server").value.trim(),
        company: document.querySelector("#mt5Server").dataset.company || "",
        password: document.querySelector("#mt5Password").value,
        lot_mode: document.querySelector("#lotMode").value,
        ratio_lot: Number(document.querySelector("#ratioLot").value),
        fixed_lot: Number(document.querySelector("#fixedLot").value),
        max_lot_per_order: Number(document.querySelector("#maxLotOrder").value),
        max_lot_total: Number(document.querySelector("#maxLotTotal").value),
        max_open_positions: Number(document.querySelector("#maxPositions").value),
      }),
    });
    document.querySelector("#mt5Password").value = "";
    if (!response.valid) throw new Error(response.message || "Login MT5 belum berhasil. Periksa server dan password broker.");
    form.dataset.editing = "";
    showClientMessage("Akun MT5 tersambung dan berada dalam mode standby.", true);
    await refreshClientDashboard();
  } catch (error) {
    showClientMessage(error.message);
  } finally {
    button.disabled = false;
    button.querySelector("span").textContent = original;
  }
});

document.querySelector("#showConnectForm").addEventListener("click", () => {
  const form = document.querySelector("#mt5Form");
  const editing = form.dataset.editing === "true";
  form.dataset.editing = editing ? "" : "true";
  form.hidden = editing;
  document.querySelector("#showConnectForm").textContent = editing ? "Perbarui koneksi / batas risiko" : "Tutup pengaturan";
});

document.querySelector("#simulationButton").addEventListener("click", async () => {
  try { await updateMode(true, true); } catch (error) { showClientMessage(error.message); }
});
document.querySelector("#liveButton").addEventListener("click", async () => {
  if (!window.confirm("Aktifkan order live untuk akun MT5 Anda? Order dapat mengalami kerugian dan mengikuti batas risiko yang ditetapkan.")) return;
  try { await updateMode(true, false, true); } catch (error) { showClientMessage(error.message); }
});
document.querySelector("#pauseButton").addEventListener("click", async () => {
  if (!window.confirm("Jeda pengiriman order baru? Posisi yang sudah terbuka tidak akan ditutup otomatis.")) return;
  try {
    await clientJSON("/api/client/pause", { method: "POST", body: "{}" });
    await refreshClientDashboard();
  } catch (error) { showClientMessage(error.message); }
});

document.querySelector("#logoutButton").addEventListener("click", async () => {
  try {
    await clientJSON("/api/auth/logout", { method: "POST", body: "{}" });
    window.location.replace("/");
  } catch (error) { showClientMessage(error.message); }
});

async function initializeClientPortal() {
  try {
    const session = await clientJSON("/api/auth/session");
    if (!session.authenticated || session.role !== "client") {
      window.location.replace(session.role === "admin" ? "/admin" : "/login");
      return;
    }
    csrfToken = session.csrf_token;
    attachMT5ServerSearch();
    await refreshClientDashboard();
    window.setInterval(() => refreshClientDashboard().catch(() => {}), 5000);
  } catch (error) {
    showClientMessage(error.message);
  }
}

initializeClientPortal();
