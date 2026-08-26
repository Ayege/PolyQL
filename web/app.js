// PolyQL playground.
//
// The page renders a seeded example immediately and translates live once the
// WebAssembly module is ready. Both paths render through renderResult, so the
// pre-load view and the live view cannot drift into showing different things.
//
// Two things the pickers do beyond picking. The source select checks that its
// language actually parses what is in the editor, and says which ones do when it
// does not — changing it otherwise produces a parse error for a language nobody
// claimed the query was written in. The target select carries the cost of each
// choice, surveyed by running the real translation against every language, so
// the price of a target is visible before it is chosen rather than after.

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
    share: document.getElementById("share"),
    build: document.getElementById("build"),
    advisory: document.getElementById("advisory"),
    advisoryText: document.getElementById("advisory-text"),
    advisoryActions: document.getElementById("advisory-actions"),
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
      // A parse failure belongs to the source language, not the target: nothing
      // was attempted, so there is no fidelity verdict to show.
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

  // --- the pickers -------------------------------------------------------

  // costLabel turns one survey entry into the text beside a target's name. The
  // three outcomes a chooser actually distinguishes are: it survives exactly, it
  // survives approximately, and part of it does not survive at all.
  function costLabel(entry) {
    if (!entry.ok) { return "won't parse"; }
    if (entry.empty) { return "nothing written"; }
    if (entry.unsupported > 0) {
      return entry.unsupported + (entry.unsupported === 1 ? " dropped" : " dropped");
    }
    if (entry.partial > 0) { return "approx " + entry.score.toFixed(2); }
    return "exact";
  }

  // annotateTargets rewrites the target options with what each would cost. The
  // survey runs the same translation the page runs, so a target labelled here as
  // exact is exact when selected.
  function annotateTargets(source, query) {
    if (!engineReady || !query) {
      fillSelect(el.target, languages, el.target.value);
      return;
    }

    var survey = window.polyqlSurvey(source, query) || [];
    var costs = {};
    survey.forEach(function (entry) { costs[entry.target] = entry; });

    var selected = el.target.value;
    el.target.textContent = "";
    languages.forEach(function (dsl) {
      var option = document.createElement("option");
      option.value = dsl;
      option.textContent = costs[dsl] ? dsl + " · " + costLabel(costs[dsl]) : dsl;
      if (dsl === selected) { option.selected = true; }
      el.target.appendChild(option);
    });
  }

  // showAdvisory explains a source language that does not parse the query, and
  // offers the languages that do. Without this, changing the source picker
  // reports a parse error against a language the query was never written in,
  // which reads as the tool being broken rather than the picker being wrong.
  function showAdvisory(query) {
    el.advisoryActions.textContent = "";

    var accepted = window.polyqlDetect(query) || [];
    if (accepted.indexOf(el.source.value) !== -1) {
      el.advisory.hidden = true;
      return;
    }

    if (accepted.length === 0) {
      // Nothing parses it, so this is a query problem and the parse error in the
      // output pane already says where. Adding a banner would say it twice.
      el.advisory.hidden = true;
      return;
    }

    el.advisoryText.textContent =
      "No " + el.source.value + " parser accepts this query. It is accepted by:";

    accepted.forEach(function (dsl) {
      var button = document.createElement("button");
      button.type = "button";
      button.className = "advisory-button";
      button.textContent = "read it as " + dsl;
      button.addEventListener("click", function () {
        el.source.value = dsl;
        translateNow();
      });
      el.advisoryActions.appendChild(button);
    });

    el.advisory.hidden = false;
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
      el.advisory.hidden = true;
      renderVerdict(null);
      annotateTargets(el.source.value, "");
      updateLocation();
      return;
    }

    renderResult(window.polyqlTranslate(el.source.value, el.target.value, query));
    annotateTargets(el.source.value, query);
    showAdvisory(query);
    updateLocation();
  }

  function scheduleTranslate() {
    window.clearTimeout(debounceTimer);
    debounceTimer = window.setTimeout(translateNow, 150);
  }

  // --- sharing -----------------------------------------------------------

  // The page's whole state is three values, so it fits in a link. A demo is much
  // easier to hand to someone as a URL that opens on the query being discussed.
  function updateLocation() {
    var params = new URLSearchParams();
    params.set("from", el.source.value);
    params.set("to", el.target.value);
    var query = el.query.value.trim();
    if (query) { params.set("q", query); }
    window.history.replaceState(null, "", "?" + params.toString());
  }

  function readLocation() {
    var params = new URLSearchParams(window.location.search);
    var query = params.get("q");
    if (!query) { return null; }
    return {
      query: query,
      source: params.get("from") || "promql",
      target: params.get("to") || "logql",
    };
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
    el.advisory.hidden = true;
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
      chip.title = ex.source + " → " + ex.target;
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

  // Swapping carries the output back into the editor, because after a swap the
  // old query is written in what is now the target language. Translating the
  // result back is also the interesting thing to do with a swap: it shows what
  // survives a round trip.
  el.swap.addEventListener("click", function () {
    var from = el.source.value;
    var carried = el.output.getAttribute("data-empty") !== "true" &&
      el.output.getAttribute("data-error") !== "true" ? el.output.textContent : "";

    el.source.value = el.target.value;
    el.target.value = from;
    if (carried) {
      el.query.value = carried;
      currentExample = -1;
      markActiveChip();
      el.exampleNote.textContent = "";
    }
    translateNow();
  });

  el.copy.addEventListener("click", function () {
    var text = el.output.textContent;
    if (!text || el.output.getAttribute("data-empty") === "true") { return; }
    copyToClipboard(text, el.copy, "copy");
  });

  el.share.addEventListener("click", function () {
    updateLocation();
    copyToClipboard(window.location.href, el.share, "link");
  });

  function copyToClipboard(text, button, label) {
    navigator.clipboard.writeText(text).then(function () {
      button.textContent = "copied";
      window.setTimeout(function () { button.textContent = label; }, 1200);
    }).catch(function () {
      button.textContent = "failed";
      window.setTimeout(function () { button.textContent = label; }, 1200);
    });
  }

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

  // A link wins over the default example: someone followed it to see one
  // specific query.
  var shared = readLocation();
  if (shared) {
    currentExample = -1;
    el.query.value = shared.query;
    fillSelect(el.source, languages, shared.source);
    fillSelect(el.target, languages, shared.target);
    el.exampleNote.textContent = "";
    el.output.textContent = "translating once the engine loads…";
    el.output.setAttribute("data-empty", "true");
    markActiveChip();
  } else {
    loadExample(0);
  }

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
