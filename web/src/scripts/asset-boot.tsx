/**
 * 不依赖任何 chunk 的资源失败兜底。
 *
 * BuildGuard / AssetFailureGuard 都是 React 组件：如果失败发生在入口 chunk 或
 * layout chunk 本身，React 根本不会挂载，两个守卫都不会运行，用户只看到白屏。
 * 这段脚本随 HTML 一起下发（内联在 <head>），是那种情况下唯一的反馈通道。
 *
 * 只提示 + 提供手动重载，不自动 reload：真正坏掉的构建不该把用户拖进重载循环。
 */
export function AssetBootScript() {
  const code = `
    (function () {
      var shown = false;
      function show() {
        if (shown) return;
        shown = true;
        try {
          var root = document.body || document.documentElement;
          if (!root) return;
          var box = document.createElement("div");
          box.setAttribute("role", "alert");
          box.style.cssText = "position:fixed;left:50%;bottom:16px;transform:translateX(-50%);" +
            "z-index:2147483647;max-width:92vw;display:flex;align-items:center;gap:12px;" +
            "padding:10px 14px;border-radius:8px;background:#18181b;color:#fafafa;" +
            "font:13px/1.5 system-ui,-apple-system,sans-serif;box-shadow:0 8px 24px rgba(0,0,0,.35)";
          var text = document.createElement("span");
          text.textContent = "页面脚本加载失败，可能不是最新版本。";
          var button = document.createElement("button");
          button.type = "button";
          button.textContent = "重新加载";
          button.style.cssText = "flex:none;padding:4px 10px;border-radius:6px;border:1px solid rgba(255,255,255,.4);" +
            "background:rgba(255,255,255,.12);color:inherit;font:inherit;cursor:pointer";
          button.onclick = function () { window.location.reload(); };
          box.appendChild(text);
          box.appendChild(button);
          root.appendChild(box);
        } catch (e) { /* 兜底失败就放弃，别抛出新的错误 */ }
      }

      function isNextAsset(url) {
        return typeof url === "string" && url.indexOf("/_next/") >= 0;
      }

      window.addEventListener("error", function (e) {
        var t = e && e.target;
        if (t && (t.tagName === "SCRIPT" || t.tagName === "LINK")) {
          if (isNextAsset(t.src || t.href)) show();
        }
      }, true);

      window.addEventListener("unhandledrejection", function (e) {
        var r = e && e.reason;
        var m = (r && (r.message || String(r))) || "";
        if (/ChunkLoadError|Loading chunk|dynamically imported module|module script failed|preload CSS/i.test(m)) show();
      });
    })();
  `;

  /* biome-ignore lint/security/noDangerouslySetInnerHtml: must run before hydration, without any chunk */
  return <script dangerouslySetInnerHTML={{ __html: code }} />;
}
