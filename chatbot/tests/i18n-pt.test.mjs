import { describe, expect, it } from "vitest";
import dict from "../src/i18n-pt-dict.json";
import { translate } from "../src/i18n-pt.js";

const HAN = /[㐀-鿿]/;
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("chat pt-BR overlay", () => {
  it("has a Chinese-free value for every key", () => {
    const bad = Object.entries(dict).filter(([, v]) => !v || HAN.test(v));
    expect(bad).toEqual([]);
  });

  it("translates whole strings and fragments around dynamic values", () => {
    expect(translate("刷新机器列表")).toBe(dict["刷新机器列表"]);
    const out = translate("当前会话执行机器：jean-win11。会话已绑定，后续运行始终使用此机器。");
    expect(out).toContain("jean-win11");
    expect(out).not.toMatch(HAN);
  });

  it("rewrites UI text but leaves model output, user text and aria-label alone", async () => {
    document.body.innerHTML =
      '<button title="刷新机器列表" aria-label="消息">刷新机器列表</button>' +
      '<div class="markdown">刷新机器列表</div><div class="user-bubble">刷新机器列表</div>';
    await tick();
    const [button, markdown, bubble] = document.body.children;
    expect(button.textContent).toBe(dict["刷新机器列表"]);
    expect(button.getAttribute("title")).toBe(dict["刷新机器列表"]);
    expect(button.getAttribute("aria-label")).toBe("消息");
    expect(markdown.textContent).toBe("刷新机器列表");
    expect(bubble.textContent).toBe("刷新机器列表");
  });
});
