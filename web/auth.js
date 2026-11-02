const authForm = document.querySelector("#authForm");
const authMessage = document.querySelector("#authMessage");
const authMode = document.body.dataset.authMode;

function showAuthMessage(message, success = false) {
  authMessage.textContent = message;
  authMessage.classList.toggle("success", success);
  authMessage.hidden = false;
}

async function readSession() {
  const response = await fetch("/api/auth/session", { headers: { Accept: "application/json" } });
  const data = await response.json();
  if (!response.ok) throw new Error(data.error || "Sesi tidak dapat diperiksa.");
  return data;
}

if (authMode === "admin-login") {
  fetch("/api/auth/admin-status", { headers: { Accept: "application/json" } })
    .then((response) => response.json())
    .then((data) => {
      if (data.configured) return;
      const notice = document.querySelector("#adminSetupNotice");
      notice.textContent = data.message || "Atur username dan password admin di file .env untuk mengaktifkan akses.";
      notice.hidden = false;
      authForm.querySelector("button").disabled = true;
    })
    .catch(() => {});
}

readSession().then((session) => {
  if (!session.authenticated) return;
  if (session.role === "admin") window.location.replace("/admin");
  else if (session.role === "client") window.location.replace("/dashboard");
}).catch(() => {});

authForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  authMessage.hidden = true;
  const button = authForm.querySelector("button");
  const original = button.querySelector("span").textContent;
  button.disabled = true;
  button.querySelector("span").textContent = "Memproses…";
  const payload = { password: document.querySelector("#password").value };
  if (authMode === "admin-login") {
    payload.username = document.querySelector("#username").value.trim();
  } else {
    payload.name = document.querySelector("#name")?.value.trim() || "";
    payload.email = document.querySelector("#email").value.trim();
  }
  const endpoint = authMode === "register" ? "/api/auth/register" : authMode === "admin-login" ? "/api/auth/admin/login" : "/api/auth/client/login";
  try {
    const response = await fetch(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify(payload),
    });
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || "Permintaan tidak dapat diproses.");
    document.querySelector("#password").value = "";
    window.location.replace(data.role === "admin" ? "/admin" : "/dashboard");
  } catch (error) {
    showAuthMessage(error.message);
  } finally {
    button.disabled = false;
    button.querySelector("span").textContent = original;
  }
});
