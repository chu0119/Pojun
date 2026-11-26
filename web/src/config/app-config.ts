const currentYear = new Date().getFullYear();

// 文档标题与描述是公网未鉴权即可抓到的指纹面（根路径 / 404 页都会带）。
// 保持中性，产品名只出现在登录后界面（可见即已通过鉴权）。
//
// 注意：这里不再有 version。手写的 2.2.0 与二进制版本、前端构建代次都无关，
// 却在页面上被当成"版本"，会误导排障。服务器版本走 /api/version，前端代次走
// <meta name="pojun-build">。
export const APP_CONFIG = {
  name: "破军",
  copyright: `© ${currentYear}, Pojun (破军).`,
  meta: {
    title: "Console",
    description: "Authorized operations console",
  },
};
