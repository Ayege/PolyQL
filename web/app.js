// PolyQL playground.
//
// The page renders a seeded example immediately and translates live once the
// WebAssembly module is ready. Both paths render through renderResult, so the
// pre-load view and the live view cannot drift into showing different things.

(function () {
  "use strict";

  var examples = window.POLYQL_EXAMPLES || [];
  var engineReady = false;
  var currentExample = 0;
  var debounceTimer = null;

  var el = {
    status: document.getElementById("status"),
    statusText: document.getElementById("status-text"),
    query: document.getElementById("query"),
    source: document.getElementById("source-dsl"),
    target: document.getElementById("target-dsl"),
    output: document.getElementById("output"),
    notes: document.getElementById("notes"),
    findings: document.getElementById("findings"),
    score: document.getElementById("score"),
    meter: document.getElementById("meter"),
    summary: document.getElementById("summary"),
    exampleNote: document.getElementById("example-note"),
    chips: document.getElementById("examples"),
    swap: document.getElementById("swap"),
    copy: document.getElementById("copy"),
    build: document.getElementById("build"),
  };

  // The pickers are seeded from the examples so the page is usable before the
  // module loads, then replaced with what the binary actually registered.
  var languages = uniqueLanguages(examples);

  function uniqueLanguages(list) {
    var seen = {};
    var out = [];
    list.forEach(function (ex) {
      [ex.source, ex.target].forEach(function (dsl) {
        if (!seen[dsl]) { seen[dsl] = true; out.push(dsl); }
      });
    });
    return out.sort();
  }

  function fillSelect(select, values, selected) {
    select.textContent = "";
    values.forEach(function (value) {
      var option = document.createElement("option");
      option.value = value;
      option.textContent = value;
      if (value === selected) { option.selected = true; }
      select.appendChild(option);
    });
  }

  function setStatus(state, text) {
    el.status.setAttribute("data-state", state);
    el.statusText.textContent = text;
  }

  // --- rendering ---------------------------------------------------------

  function renderResult(result) {
    if (!result) { return; }

    if (!result.ok) {
      el.output.textContent = result.error;
      el.output.setAttribute("data-error", "true");
      el.output.removeAttribute("data-empty");
      el.notes.textContent = "";
      renderVerdict(null);
      return;
    }

    el.output.removeAttribute("data-error");
    if (result.output) {
      el.output.textContent = result.output;
      el.output.removeAttribute("data-empty");
    } else {
      el.output.textContent = "nothing could be written for this query";
      el.output.setAttribute("data-empty", "true");
    }

    el.notes.textContent = "";
    (result.notes || []).forEach(function (note) {
      var li = document.createElement("li");
      li.textContent = note;
      el.notes.appendChild(li);
    });

    renderVerdict(result.report);
  }

  function renderVerdict(report) {
    el.findings.textContent = "";
    el.meter.textContent = "";

    if (!report) {
      el.score.textContent = "—";
      el.summary.textContent = "";
      return;
    }

    el.score.textContent = report.score.toFixed(2);
    el.summary.textContent = report.summary;

    // Segment widths come from the node counts rather than the score, so the
    // bar shows the shape of the loss and the number beside it shows its size.
    var total = report.total || 1;
    [["full", report.full], ["partial", report.partial], ["unsupported", report.unsupported]]
      .forEach(function (pair) {
        if (!pair[1]) { return; }
        var seg = document.createElement("span");
        seg.className = "seg-" + pair[0];
        seg.style.width = (pair[1] / total * 100) + "%";
        seg.title = pair[1] + " " + pair[0];
        el.meter.appendChild(seg);
      });

    if (report.signalMismatch) {
      var li = document.createElement("li");
      li.className = "mismatch";
      li.textContent = report.signalMismatch;
      el.findings.appendChild(li);
    }

    (report.nodes || []).forEach(function (node) {
      el.findings.appendChild(findingElement(node));
    });
  }

  function findingElement(node) {
    var li = document.createElement("li");
    li.className = "finding";

    var flag = document.createElement("span");
    flag.className = "flag";
    flag.setAttribute("data-flag", node.flag);
    flag.textContent = node.flag.toLowerCase();

    var body = document.createElement("div");
    body.className = "finding-body";

    var reason = document.createElement("div");
    reason.className = "finding-reason";
    reason.textContent = node.reason;

    var path = document.createElement("div");
    path.className = "finding-path";
    path.textContent = node.path;

    body.appendChild(reason);
    body.appendChild(path);
    li.appendChild(flag);
    li.appendChild(body);
    return li;
  }

  // --- translating -------------------------------------------------------

  function translateNow() {
    if (!engineReady) { return; }
    var query = el.query.value.trim();
    if (!query) {
      el.output.textContent = "type a query";
      el.output.setAttribute("data-empty", "true");
      el.output.removeAttribute("data-error");
      el.notes.textContent = "";
      renderVerdict(null);
      return;
    }
    renderResult(window.polyqlTranslate(el.source.value, el.target.value, query));
  }

  function scheduleTranslate() {
    window.clearTimeout(debounceTimer);
    debounceTimer = window.setTimeout(translateNow, 150);
  }

  // --- examples ----------------------------------------------------------

  function loadExample(index) {
    var ex = examples[index];
    if (!ex) { return; }
    currentExample = index;

    el.query.value = ex.query;
    fillSelect(el.source, languages, ex.source);
    fillSelect(el.target, languages, ex.target);
    el.exampleNote.textContent = ex.note;
    markActiveChip();

    // Before the module is ready the seeded result is all there is; after, the
    // live one replaces it with an identical value, which is what the
    // generator's test guarantees.
    if (engineReady) { translateNow(); } else { renderResult(ex.result); }
  }

  function markActiveChip() {
    Array.prototype.forEach.call(el.chips.children, function (chip, i) {
      chip.setAttribute("aria-pressed", i === currentExample ? "true" : "false");
    });
  }

  function buildChips() {
    examples.forEach(function (ex, i) {
      var chip = document.createElement("button");
      chip.type = "button";
      chip.className = "chip";
      chip.textContent = ex.title;
      chip.setAttribute("aria-pressed", "false");
      chip.addEventListener("click", function () { loadExample(i); });
      el.chips.appendChild(chip);
    });
  }

  // --- wiring ------------------------------------------------------------

  el.query.addEventListener("input", function () {
    currentExample = -1;
    markActiveChip();
    el.exampleNote.textContent = "";
    scheduleTranslate();
  });

  el.source.addEventListener("change", translateNow);
  el.target.addEventListener("change", translateNow);

  el.swap.addEventListener("click", function () {
    var from = el.source.value;
    el.source.value = el.target.value;
    el.target.value = from;
    translateNow();
  });

  el.copy.addEventListener("click", function () {
    var text = el.output.textContent;
    if (!text || el.output.getAttribute("data-empty") === "true") { return; }
    navigator.clipboard.writeText(text).then(function () {
      el.copy.textContent = "copied";
      window.setTimeout(function () { el.copy.textContent = "copy"; }, 1200);
    });
  });

  document.addEventListener("keydown", function (event) {
    if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
      event.preventDefault();
      translateNow();
    }
  });

  // Streaming instantiation needs the server to send application/wasm. GitHub
  // Pages does; not every static server a contributor runs locally will, so fall
  // back to buffering rather than failing with a MIME-type error.
  function load(go) {
    return WebAssembly.instantiateStreaming(fetch("polyql.wasm"), go.importObject)
      .catch(function () {
        return fetch("polyql.wasm")
          .then(function (response) { return response.arrayBuffer(); })
          .then(function (bytes) { return WebAssembly.instantiate(bytes, go.importObject); });
      });
  }

  // The Go program calls this once its exports are installed.
  window.polyqlReady = function () {
    if (window.polyqlLoadError) {
      setStatus("error", window.polyqlLoadError);
      return;
    }
    engineReady = true;

    var registered = window.polyqlLanguages();
    if (registered && registered.length) {
      languages = registered.slice().sort();
      var source = el.source.value;
      var target = el.target.value;
      fillSelect(el.source, languages, source);
      fillSelect(el.target, languages, target);
    }

    var build = window.polyqlVersion || {};
    el.build.textContent = "polyql " + (build.version || "dev") +
      " (" + (build.commit || "unknown") + ") · " + languages.join(" · ");

    setStatus("ready", "ready · runs in this tab");
    translateNow();
  };

  buildChips();
  loadExample(0);
  setStatus("loading", "loading engine…");

  // Load the module last, so the seeded view is already painted when the
  // download starts.
  if (typeof Go === "undefined") {
    setStatus("error", "wasm_exec.js missing — run `make playground`");
  } else {
    var go = new Go();
    load(go)
      .then(function (result) { go.run(result.instance); })
      .catch(function (err) { setStatus("error", "engine failed to load: " + err.message); });
  }
})();
