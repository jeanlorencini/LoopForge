// pt-BR overlay for the chat workbench, adapted from web/i18n.js. The chat
// renders Chinese copy directly; this rewrites text nodes and a few attributes
// as they appear, using the zh→pt dictionary in i18n-pt-dict.json.
// aria-label is left alone: App.jsx and useArtifactPanelLayout.js query it.
// Set localStorage "loopforge.chat.lang" to "zh" to turn it off.
import dict from "./i18n-pt-dict.json";

export function enabled() {
  try {
    return localStorage.getItem("loopforge.chat.lang") !== "zh";
  } catch {
    return true;
  }
}

const HAN = /[㐀-鿿「」，。：（）、；！？]/;
const SKIP = new Set(["SCRIPT", "STYLE", "TEXTAREA", "CODE", "PRE", "NOSCRIPT"]);
// Model output, user-authored text, diffs and terminals are never rewritten.
const SKIP_SEL =
  '[data-i18n-skip],.markdown,.user-bubble,.queued-message-text,.diff-body,.xterm,.monaco-editor,[contenteditable="true"]';
const ATTRS = ["placeholder", "title", "data-tip", "data-tooltip"];
const escape = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const fragments = Object.keys(dict)
  .filter((k) => k.length > 1)
  .sort((a, b) => b.length - a.length);
const fragmentRE = fragments.length
  ? new RegExp("(?<![\\u3400-\\u9fff])(?:" + fragments.map(escape).join("|") + ")(?![\\u3400-\\u9fff])", "g")
  : null;
const sentences = fragments.filter((k) => k.length >= 6 && HAN.test(k));
const sentenceRE = sentences.length ? new RegExp(sentences.map(escape).join("|"), "g") : null;
const punct = [
  [/，/g, ", "],
  [/。/g, ". "],
  [/：/g, ": "],
  [/；/g, "; "],
  [/、/g, ", "],
  [/！/g, "! "],
  [/？/g, "? "],
  [/（/g, " ("],
  [/）/g, ") "],
  [/「|」|“|”/g, '"'],
  [/『|』/g, '"'],
];
const UNITS = { 次: ["vez", "vezes"], 步: ["etapa", "etapas"], 个: ["", ""], 台: ["", ""], 条: ["", ""], 项: ["", ""] };
const patterns = [
  [/第\s*(\d+)\s*阶段/g, "Etapa $1"],
  [/第\s*(\d+)\s*轮/g, "Rodada $1"],
  [/(\d{4})年(\d{1,2})月(\d{1,2})日/g, "$3/$2/$1"],
  [/(\d{1,2})月(\d{1,2})日/g, "$2/$1"],
  [/(\d+)\s*秒前/g, "há $1 s"],
  [/(\d+)\s*分钟前/g, "há $1 min"],
  [/(\d+)\s*小时前/g, "há $1 h"],
  [/(\d+)\s*天前/g, "há $1 dias"],
  [/(\d+(?:\.\d+)?)\s*(毫秒|秒|分钟|小时)/g, (_, n, u) => `${n} ${{ 毫秒: "ms", 秒: "s", 分钟: "min", 小时: "h" }[u]}`],
  [/(\d+)\s*行(?![㐀-鿿])/g, "$1 linhas"],
  [/(?<=[\x21-\x7e]) 与 (?=[\x21-\x7e])/g, " e "],
  // Search placeholders are built as `搜索${label}` at runtime.
  [/搜索([㐀-鿿]+)/g, (m, term) => (has(term) ? `Pesquisar ${dict[term].toLowerCase()}` : m)],
  [/(\d+(?:\.\d+)?)\s*(次|步|个|台|条|项)(?![㐀-鿿])/g, (_, n, u) => {
    const word = UNITS[u][Number(n) === 1 ? 0 : 1];
    return word ? `${n} ${word}` : n;
  }],
];
const cache = new Map();
const has = (k) => Object.prototype.hasOwnProperty.call(dict, k);

export function translate(text) {
  if (!text || !HAN.test(text)) return text;
  const hit = cache.get(text);
  if (hit !== undefined) return hit;
  const trimmed = text.trim();
  let out;
  if (has(trimmed)) {
    out = text.replace(trimmed, dict[trimmed]);
  } else {
    out = text;
    for (const [re, rep] of patterns) out = out.replace(re, rep);
    if (fragmentRE) out = out.replace(fragmentRE, (m) => dict[m]);
    if (sentenceRE && HAN.test(out)) out = out.replace(sentenceRE, (m) => dict[m]);
    // A sentence swap can free a neighbouring fragment, e.g. a Chinese dynamic value.
    if (fragmentRE && HAN.test(out)) out = out.replace(fragmentRE, (m) => dict[m]);
  }
  if (out !== text) {
    for (const [re, rep] of punct) out = out.replace(re, rep);
    out = out.replace(/:\s*:/g, ":").replace(/ {2,}/g, " ").replace(/ ([,.:;!?)])/g, "$1").replace(/\( /g, "(");
    if (!/^\s/.test(text)) out = out.replace(/^\s+/, "");
    if (!/\s$/.test(text)) out = out.replace(/\s+$/, "");
  }
  if (cache.size > 20000) cache.clear();
  cache.set(text, out);
  return out;
}

// For text the app writes into the composer, which the overlay never rewrites.
export const localize = (text) => (enabled() ? translate(text) : text);

function skipped(el) {
  return !el || SKIP.has(el.tagName) || (el.closest && el.closest(SKIP_SEL));
}
function translateText(node) {
  if (skipped(node.parentElement)) return;
  const next = translate(node.nodeValue);
  if (next !== node.nodeValue) node.nodeValue = next;
}
function translateAttrs(el) {
  if (el.closest && el.closest(SKIP_SEL)) return;
  for (const name of ATTRS) {
    const value = el.getAttribute && el.getAttribute(name);
    if (value && HAN.test(value)) {
      const next = translate(value);
      if (next !== value) el.setAttribute(name, next);
    }
  }
}
function walk(node) {
  if (node.nodeType === 3) return translateText(node);
  if (node.nodeType !== 1) return;
  if (skipped(node)) {
    if (node.tagName === "TEXTAREA") translateAttrs(node);
    return;
  }
  translateAttrs(node);
  const tw = document.createTreeWalker(node, NodeFilter.SHOW_TEXT | NodeFilter.SHOW_ELEMENT, {
    acceptNode: (n) => {
      if (n.nodeType !== 1 || !skipped(n)) return NodeFilter.FILTER_ACCEPT;
      if (n.tagName === "TEXTAREA") translateAttrs(n);
      return NodeFilter.FILTER_REJECT;
    },
  });
  let n;
  while ((n = tw.nextNode())) {
    if (n.nodeType === 3) translateText(n);
    else translateAttrs(n);
  }
}
function translateTitle() {
  const next = translate(document.title);
  if (next !== document.title) document.title = next;
}

export function startChatTranslation() {
  document.documentElement.lang = "pt-BR";
  const observer = new MutationObserver((records) => {
    for (const r of records) {
      if (r.type === "childList") r.addedNodes.forEach(walk);
      else if (r.type === "characterData") translateText(r.target);
      else if (r.type === "attributes") translateAttrs(r.target);
    }
  });
  const start = () => {
    walk(document.body);
    translateTitle();
    observer.observe(document.body, { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ATTRS });
    const title = document.querySelector("title");
    if (title) new MutationObserver(translateTitle).observe(title, { childList: true, characterData: true, subtree: true });
  };
  if (document.body) start();
  else document.addEventListener("DOMContentLoaded", start, { once: true });
  const nativeConfirm = window.confirm.bind(window);
  const nativeAlert = window.alert.bind(window);
  const nativePrompt = window.prompt.bind(window);
  window.confirm = (message) => nativeConfirm(translate(String(message ?? "")));
  window.alert = (message) => nativeAlert(translate(String(message ?? "")));
  window.prompt = (message, value) => nativePrompt(translate(String(message ?? "")), value);
}
