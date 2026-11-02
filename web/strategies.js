const strategyList = document.querySelector("#strategyList");
const strategyCount = document.querySelector("#strategyCount");

async function loadStrategies() {
  try {
    const response = await fetch("/api/public/strategies", { headers: { Accept: "application/json" } });
    const data = await response.json();
    if (!response.ok) throw new Error(data.error || "Status strategi belum tersedia.");
    const items = data.strategies || [];
    strategyCount.textContent = `01 / ${String(items.length).padStart(2, "0")}`;
    strategyList.replaceChildren();
    for (const strategy of items) {
      const article = document.createElement("article");
      article.className = "strategy-card";
      const top = document.createElement("div");
      top.className = "strategy-card-top";
      const index = document.createElement("span");
      index.className = "strategy-index";
      index.textContent = String(items.indexOf(strategy) + 1).padStart(2, "0");
      const status = document.createElement("span");
      status.className = "status-pill";
      status.dataset.status = strategy.status;
      status.textContent = strategy.status === "active" ? "SUMBER AKTIF" : "MENUNGGU SUMBER";
      top.append(index, status);
      const category = document.createElement("p");
      category.className = "strategy-category";
      category.textContent = strategy.category;
      const title = document.createElement("h2");
      title.textContent = strategy.name;
      const description = document.createElement("p");
      description.className = "strategy-description";
      description.textContent = strategy.description;
      const facts = document.createElement("div");
      facts.className = "strategy-facts";
      for (const [label, value] of [["EKSEKUSI", strategy.execution], ["MODE AWAL", "Simulasi"], ["HISTORI KINERJA", strategy.performance_note]]) {
        const item = document.createElement("div");
        const small = document.createElement("small");
        small.textContent = label;
        const strong = document.createElement("strong");
        strong.textContent = value;
        item.append(small, strong);
        facts.append(item);
      }
      const bottom = document.createElement("div");
      bottom.className = "strategy-card-bottom";
      const risk = document.createElement("span");
      risk.textContent = strategy.risk_label;
      const access = document.createElement("a");
      access.className = "text-link";
      access.href = "/register";
      access.textContent = "Lihat akses client →";
      bottom.append(risk, access);
      article.append(top, category, title, description, facts, bottom);
      strategyList.append(article);
    }
  } catch (error) {
    strategyCount.textContent = "STATUS / OFFLINE";
    const note = document.createElement("p");
    note.className = "data-note";
    note.textContent = error.message;
    strategyList.replaceChildren(note);
  }
}

loadStrategies();
