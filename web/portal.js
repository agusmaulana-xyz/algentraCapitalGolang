const menuButton = document.querySelector(".menu-toggle");
const siteNav = document.querySelector(".site-nav");

if (menuButton && siteNav) {
  menuButton.addEventListener("click", () => {
    const open = siteNav.classList.toggle("menu-open");
    menuButton.setAttribute("aria-expanded", String(open));
  });
}

for (const link of document.querySelectorAll("[data-auth-link], [data-register-link]")) {
  fetch("/api/auth/session", { headers: { Accept: "application/json" } })
    .then((response) => response.json())
    .then((session) => {
      if (!session.authenticated || session.role !== "client") return;
      const loginLink = document.querySelector("[data-auth-link]");
      const registerLink = document.querySelector("[data-register-link]");
      if (loginLink) {
        loginLink.href = "/dashboard";
        loginLink.textContent = "Dashboard";
      }
      if (registerLink) registerLink.hidden = true;
    })
    .catch(() => {});
  break;
}
